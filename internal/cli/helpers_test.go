package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/BurntSushi/toml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/taknya/slotctl/internal/eventlog"
)

// testEnv は、SLOTCTL_HOMEを一時directoryにして、時刻を差し替えられるtest環境である。
type testEnv struct {
	t    *testing.T
	home string
	now  time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return &testEnv{
		t:    t,
		home: t.TempDir(),
		now:  time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
	}
}

func (e *testEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

// run は、cwdでslotctlを実行して、終了code・stdout・stderrを返す。
func (e *testEnv) run(cwd string, args ...string) (int, string, string) {
	e.t.Helper()
	var out, errb bytes.Buffer
	app := &App{
		Now: func() time.Time { return e.now },
		Getenv: func(k string) string {
			switch k {
			case "SLOTCTL_HOME", "HOME":
				return e.home
			}
			return ""
		},
		Cwd:    cwd,
		Stdout: &out,
		Stderr: &errb,
	}
	code := app.Run(args)
	return code, out.String(), errb.String()
}

// project は、repositoryの操作とtemp machineの枠を用意する。
// testの既定は各pool 1枠。明示した数と既存machine設定を優先する。
func (e *testEnv) project(source string, counts ...map[string]int) string {
	e.t.Helper()
	root := e.t.TempDir()
	writeFile(e.t, filepath.Join(root, "slotctl.toml"), source)
	var pf struct {
		Project string
		Pools   map[string]any
	}
	if _, err := toml.Decode(source, &pf); err == nil && pf.Project != "" {
		path := filepath.Join(e.home, "config.toml")
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			e.t.Fatal(err)
		}
		var mf struct {
			Projects map[string]struct{ Pools map[string]any }
		}
		if _, err := toml.Decode(string(data), &mf); err != nil {
			e.t.Fatal(err)
		}
		for name := range pf.Pools {
			if name == "" {
				continue
			}
			if _, exists := mf.Projects[pf.Project].Pools[name]; exists {
				continue
			}
			n := 1
			if len(counts) > 0 {
				if v, ok := counts[0][name]; ok {
					n = v
				}
			}
			data = append(data, []byte(fmt.Sprintf("\n[projects.%q.pools.%q]\nslots = %d\n", pf.Project, name, n))...)
		}
		writeFile(e.t, path, string(data))
	}
	return root
}

// holderDir は、rootの下にholder用のdirectoryを作って、その実pathを返す。
func holderDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func readEvents(t *testing.T, dir string) []eventlog.Event {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	var evs []eventlog.Event
	for _, f := range files {
		for _, line := range readLines(t, f) {
			var ev eventlog.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("記録の行がJSONではありません: %q: %v", line, err)
			}
			evs = append(evs, ev)
		}
	}
	return evs
}

func mustAcquire(t *testing.T, e *testEnv, cwd string) AcquireResult {
	t.Helper()
	code, out, errs := e.run(cwd, "acquire", "dev", "--json")
	if code != 0 {
		t.Fatalf("acquire が終了code %d（stderr: %s）", code, errs)
	}
	var res AcquireResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("acquire --json の出力が不正: %q: %v", out, err)
	}
	return res
}

type statusResult struct {
	Project string       `json:"project"`
	Slots   []statusSlot `json:"slots"`
}

func mustStatus(t *testing.T, e *testEnv, cwd string) statusResult {
	t.Helper()
	code, out, errs := e.run(cwd, "status", "--json")
	if code != 0 {
		t.Fatalf("status が終了code %d（stderr: %s）", code, errs)
	}
	var res statusResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("status --json の出力が不正: %q: %v", out, err)
	}
	return res
}
