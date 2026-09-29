package main_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
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
