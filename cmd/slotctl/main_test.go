package main_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// AC-4: 同時の申請で、1つの枠が二重に貸されない。
// binaryをbuildして、別々のprocessから同じDBを開く。
func TestConcurrentAcquireNeverLendsASlotTwice(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "slotctl")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	const procs = 10
	for round := 0; round < 3; round++ {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			home := t.TempDir()
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "slotctl.toml"), []byte("project = \"demo\"\n[slots]\ncount = 3\n[ports]\nnames = [\"web\"]\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			type result struct {
				code int
				out  []byte
			}
			results := make([]result, procs)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < procs; i++ {
				dir := filepath.Join(root, fmt.Sprintf("holder%d", i))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					cmd := exec.Command(bin, "acquire", "--json")
					cmd.Dir = dir
					cmd.Env = append(os.Environ(), "SLOTCTL_HOME="+home)
					<-start
					out, err := cmd.Output()
					code := 0
					var ee *exec.ExitError
					if errors.As(err, &ee) {
						code = ee.ExitCode()
					} else if err != nil {
						code = -1
					}
					results[i] = result{code, out}
				}()
			}
			close(start)
			wg.Wait()

			ok, full := 0, 0
			slots := map[int]bool{}
			for i, r := range results {
				switch r.code {
				case 0:
					ok++
					var res struct {
						Slot int `json:"slot"`
					}
					if err := json.Unmarshal(r.out, &res); err != nil {
						t.Fatalf("process %d の出力が不正: %q", i, r.out)
					}
					if slots[res.Slot] {
						t.Errorf("slot %d が二重に貸されました", res.Slot)
					}
					slots[res.Slot] = true
				case 3:
					full++
				default:
					t.Errorf("process %d の終了code %d", i, r.code)
				}
			}
			if ok != 3 || full != 7 || len(slots) != 3 {
				t.Fatalf("成功 %d・終了code 3が %d・異なるslot %d（成功3・7・3のはず）", ok, full, len(slots))
			}
		})
	}
}

// 同時に開いても、v0.1.0のstate.dbの移行は1回だけで、貸し出し中の枠が失われない。
func TestConcurrentOpenMigratesV1StateOnce(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "slotctl")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	home := t.TempDir()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "slotctl.toml"), []byte("project = \"demo\"\n[slots]\ncount = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	holderDir := filepath.Join(root, "a")
	if err := os.MkdirAll(holderDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realHolder, _ := filepath.EvalSymlinks(holderDir)
	realRoot, _ := filepath.EvalSymlinks(root)

	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE projects (name TEXT PRIMARY KEY, repo TEXT NOT NULL, port_base INTEGER NOT NULL)`,
		`CREATE TABLE leases (project TEXT NOT NULL, slot INTEGER NOT NULL, holder TEXT NOT NULL, acquired_at INTEGER NOT NULL, renewed_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY (project, slot))`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO projects VALUES ('demo', ?, 12000)`, realRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO leases VALUES ('demo', 1, ?, 1, 1, 99999999999)`, realHolder); err != nil {
		t.Fatal(err)
	}
	db.Close()

	const procs = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	outs := make([]string, procs)
	codes := make([]int, procs)
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(bin, "status", "--json")
			cmd.Dir = holderDir
			cmd.Env = append(os.Environ(), "SLOTCTL_HOME="+home)
			<-start
			out, err := cmd.CombinedOutput()
			outs[i] = string(out)
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				codes[i] = ee.ExitCode()
			} else if err != nil {
				codes[i] = -1
			}
		}()
	}
	close(start)
	wg.Wait()
	for i := range outs {
		if codes[i] != 0 {
			t.Fatalf("process %d の終了code %d: %s", i, codes[i], outs[i])
		}
		var res struct {
			Slots []struct {
				Pool   string `json:"pool"`
				Slot   int    `json:"slot"`
				State  string `json:"state"`
				Holder string `json:"holder"`
			} `json:"slots"`
		}
		if err := json.Unmarshal([]byte(outs[i]), &res); err != nil {
			t.Fatalf("process %d の出力が不正: %q", i, outs[i])
		}
		if len(res.Slots) != 3 || res.Slots[0].State != "lent" || res.Slots[0].Holder != realHolder || res.Slots[0].Pool != "default" {
			t.Fatalf("process %d: 貸し出し中の枠が引き継がれるはず: %+v", i, res.Slots)
		}
	}
}
