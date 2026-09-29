package eventlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func touch(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func newLog(dir string, retention int, now time.Time) *Log {
	return &Log{Dir: dir, Project: "demo", RetentionMonths: retention, Now: func() time.Time { return now }}
}

// AC-6: 記録のローテーションが働く。
func TestRotationRemovesFilesOlderThanRetention(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "events-2026-05.jsonl", "events-2026-06.jsonl", "events-2026-07.jsonl", "events-2026-08.jsonl", "events-2026-09.jsonl", "notes.txt")

	l := newLog(dir, 3, time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC))
	if err := l.Record(Event{Event: "acquire", Holder: "/h"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"events-2026-05.jsonl": false,
		"events-2026-06.jsonl": false,
		"events-2026-07.jsonl": true,
		"events-2026-08.jsonl": true,
		"events-2026-09.jsonl": true,
		"notes.txt":            true,
	} {
		if got := exists(dir, name); got != want {
			t.Errorf("%s: 残っているか got %v want %v", name, got, want)
		}
	}
}

func TestRotationAcrossYearBoundary(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "events-2026-10.jsonl", "events-2026-11.jsonl", "events-2026-12.jsonl")
	l := newLog(dir, 3, time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC))
	if err := l.Record(Event{Event: "acquire", Holder: "/h"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"events-2026-10.jsonl": false,
		"events-2026-11.jsonl": true,
		"events-2026-12.jsonl": true,
		"events-2027-01.jsonl": true,
	} {
		if got := exists(dir, name); got != want {
			t.Errorf("%s: 残っているか got %v want %v", name, got, want)
		}
	}
}

func TestRecordAppendsJSONLines(t *testing.T) {
	dir := t.TempDir()
	l := newLog(dir, 3, time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC))
	for _, e := range []string{"up", "acquire"} {
		if err := l.Record(Event{Event: e, Holder: "/h", Slot: 1, OK: BoolPtr(true), MS: MS(1500 * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "events-2026-09.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"at":"2026-09-15T10:00:00Z","event":"up","project":"demo","slot":1,"holder":"/h","ok":true,"ms":1500}
{"at":"2026-09-15T10:00:00Z","event":"acquire","project":"demo","slot":1,"holder":"/h","ok":true,"ms":1500}
`
	if string(b) != want {
		t.Fatalf("記録:\n got %s\nwant %s", b, want)
	}
}

func TestRecordKeepsLogPrivateToTheUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	l := &Log{Dir: dir, Project: "demo", RetentionMonths: 3, Now: func() time.Time { return time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC) }}
	if err := l.Record(Event{Event: "acquire", Holder: "/w/a"}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "events-2026-09.jsonl"), 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s: mode %o, want %o", path, got, want)
	}
}
