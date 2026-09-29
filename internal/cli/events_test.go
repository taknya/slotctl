package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// AC-6: 命令が記録を書くときに、machine設定のlog_retention_monthsに従って古い記録が消える。
func TestAcquireRotatesEventsByConfiguredRetention(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.home, "config.toml"), "log_retention_months = 2\n")
	for _, n := range []string{"events-2026-06.jsonl", "events-2026-07.jsonl", "events-2026-08.jsonl"} {
		writeFile(t, filepath.Join(e.home, n), "{}\n")
	}
	root := e.project("project = \"demo\"\n")
	mustAcquire(t, e, root)
	if exists(e.home, "events-2026-06.jsonl") || exists(e.home, "events-2026-07.jsonl") {
		t.Error("2か月より古い記録が残っています")
	}
	if !exists(e.home, "events-2026-08.jsonl") || !exists(e.home, "events-2026-09.jsonl") {
		t.Error("直近2か月の記録が消えています")
	}
}

// renewは記録を書かない。acquire・release・up・downは書く。
func TestEventsRecordedPerCommand(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project = \"demo\"\n[commands]\nup = \"true\"\ndown = \"true\"\n")
	mustAcquire(t, e, root)
	e.run(root, "renew")
	e.run(root, "release")
	var got []string
	for _, ev := range readEvents(t, e.home) {
		got = append(got, ev.Event)
		if ev.At == "" || ev.Project != "demo" || ev.Holder == "" {
			t.Errorf("記録の必須項目が足りません: %+v", ev)
		}
	}
	want := []string{"up", "acquire", "down", "release"}
	if len(got) != len(want) {
		t.Fatalf("events: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events: got %v want %v", got, want)
		}
	}
}
