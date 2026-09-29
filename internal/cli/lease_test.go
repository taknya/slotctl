package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AC-1: 同じholderの申請は冪等で、空き枠が小さい番号から割り当たる。
func TestAcquireIsIdempotentAndUsesSmallestFreeSlot(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(`
project = "demo"
[lease]
ttl = "10m"
[slots]
count = 3
[ports]
names = ["web", "db"]
[commands]
up = 'echo "up $SLOTCTL_SLOT $SLOTCTL_PORT_WEB" >> %s'
`, logFile))
	a := holderDir(t, root, "a")
	b := holderDir(t, root, "b")

	first := mustAcquire(t, e, a)
	if first.Slot != 1 || first.Name != "demo-1" || first.Holder != a {
		t.Fatalf("holder A の最初のacquire: %+v", first)
	}
	if first.Ports["web"] != 12000 || first.Ports["db"] != 12001 {
		t.Fatalf("holder A のport: %+v", first.Ports)
	}

	e.advance(3 * time.Minute)
	again := mustAcquire(t, e, a)
	if again.Slot != 1 {
		t.Fatalf("同じholderの2回目のacquire は同じ枠のはず: %+v", again)
	}
	want := e.now.Add(10 * time.Minute).Format(time.RFC3339)
	if again.ExpiresAt != want {
		t.Fatalf("期限は延びるはず: got %s want %s", again.ExpiresAt, want)
	}
	if again.ExpiresAt == first.ExpiresAt {
		t.Fatalf("期限が延びていません: %s", again.ExpiresAt)
	}

	second := mustAcquire(t, e, b)
	if second.Slot != 2 || second.Ports["web"] != 12100 || second.Ports["db"] != 12101 {
		t.Fatalf("holder B は slot 2 のはず: %+v", second)
	}

	// upは新しく借りたときだけ走る（同じholderの再申請では走らない）。
	lines := readLines(t, logFile)
	if len(lines) != 2 || lines[0] != "up 1 12000" || lines[1] != "up 2 12100" {
		t.Fatalf("upの記録: %v", lines)
	}
}

// AC-2: 空きが無いときは最も古い期限切れを譲り、それも無ければ終了code 3。
func TestAcquirePreemptsOldestExpiredAndFailsWithExit3WhenNoneExpired(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(`
project = "demo"
[lease]
ttl = "10m"
[slots]
count = 3
[commands]
up = 'echo "up $SLOTCTL_HOLDER $SLOTCTL_SLOT" >> %[1]s'
down = 'echo "down $SLOTCTL_HOLDER $SLOTCTL_SLOT" >> %[1]s'
`, logFile))
	a, b, c, d := holderDir(t, root, "a"), holderDir(t, root, "b"), holderDir(t, root, "c"), holderDir(t, root, "d")

	mustAcquire(t, e, a)
	mustAcquire(t, e, b)
	if got := mustAcquire(t, e, c); got.Slot != 3 {
		t.Fatalf("C は slot 3 のはず: %+v", got)
	}

	// AとBは生存連絡で延びる。Cだけが最も古い期限のまま。
	e.advance(30 * time.Second)
	for _, h := range []string{a, b} {
		if code, _, errs := e.run(h, "renew"); code != 0 {
			t.Fatalf("renew: %d %s", code, errs)
		}
	}

	code, _, errs := e.run(d, "acquire")
	if code != 3 {
		t.Fatalf("空きが無いときの終了code: got %d want 3（stderr: %s）", code, errs)
	}
	for _, h := range []string{a, b, c} {
		if !strings.Contains(errs, h) {
			t.Fatalf("使用中のholder %s が出力に無い: %s", h, errs)
		}
	}
	if !strings.Contains(errs, e.now.Add(10*time.Minute-30*time.Second).Format(time.RFC3339)) {
		t.Fatalf("Cの期限が出力に無い: %s", errs)
	}

	e.advance(11 * time.Minute) // ttl + 1分。全て期限切れで、Cが最も古い。
	got := mustAcquire(t, e, d)
	if got.Slot != 3 || got.PreviousHolder != c {
		t.Fatalf("D は C の slot 3 を得るはず: %+v", got)
	}

	// Cのenvでdown、Dのenvでupの順に走る。
	lines := readLines(t, logFile)
	if len(lines) < 5 {
		t.Fatalf("記録が足りません: %v", lines)
	}
	tail := lines[len(lines)-2:]
	if tail[0] != "down "+c+" 3" || tail[1] != "up "+d+" 3" {
		t.Fatalf("down→upの順のはず: %v", tail)
	}

	var found bool
	for _, ev := range readEvents(t, e.home) {
		if ev.Event == "preempt" {
			found = true
			if ev.PreviousHolder != c || ev.Holder != d || ev.Slot != 3 || ev.Project != "demo" {
				t.Fatalf("preemptの記録: %+v", ev)
			}
		}
	}
	if !found {
		t.Fatal("preemptが記録に無い")
	}
}

// AC-3: renew・releaseが仕様どおりに動く。
func TestRenewAndRelease(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	toml := func(down string) string {
		return fmt.Sprintf(`
project = "demo"
[lease]
ttl = "10m"
[slots]
count = 2
[commands]
up = 'echo "up $SLOTCTL_SLOT" >> %[1]s'
down = '%[2]s'
`, logFile, down)
	}
	root := e.project(toml(fmt.Sprintf(`echo "down $SLOTCTL_SLOT" >> %s`, logFile)))
	a := holderDir(t, root, "a")
	nobody := holderDir(t, root, "nobody")

	mustAcquire(t, e, a)
	e.advance(7 * time.Minute)
	if code, out, errs := e.run(a, "renew"); code != 0 || out != "" {
		t.Fatalf("renew: code=%d out=%q err=%q", code, out, errs)
	}
	st := mustStatus(t, e, a)
	want := e.now.Add(10 * time.Minute).Format(time.RFC3339)
	if st.Slots[0].ExpiresAt != want || st.Slots[0].State != "lent" {
		t.Fatalf("renewで期限は今+ttlのはず: %+v want %s", st.Slots[0], want)
	}
	if n := len(readLines(t, logFile)); n != 1 {
		t.Fatalf("renewはcommandを走らせないはず: %v", readLines(t, logFile))
	}

	// leaseの無いholderは、renewもreleaseも何もせず0。
	for _, cmd := range []string{"renew", "release"} {
		if code, out, errs := e.run(nobody, cmd); code != 0 || out != "" || errs != "" {
			t.Fatalf("%s(leaseなし): code=%d out=%q err=%q", cmd, code, out, errs)
		}
	}
	if n := len(readLines(t, logFile)); n != 1 {
		t.Fatalf("leaseの無いreleaseはcommandを走らせないはず: %v", readLines(t, logFile))
	}
	if st := mustStatus(t, e, a); st.Slots[0].State != "lent" {
		t.Fatalf("他holderのreleaseでAの枠が消えた: %+v", st.Slots[0])
	}

	// releaseはdownを走らせてleaseを消す。
	if code, _, errs := e.run(a, "release"); code != 0 {
		t.Fatalf("release: %d %s", code, errs)
	}
	lines := readLines(t, logFile)
	if len(lines) != 2 || lines[1] != "down 1" {
		t.Fatalf("releaseはdownを走らせるはず: %v", lines)
	}
	if st := mustStatus(t, e, a); st.Slots[0].State != "free" || st.Slots[0].Holder != "" {
		t.Fatalf("releaseで枠は空くはず: %+v", st.Slots[0])
	}

	// downが失敗してもleaseは消え、終了code 1になる。
	writeFile(t, filepath.Join(root, "slotctl.toml"), toml("false"))
	mustAcquire(t, e, a)
	code, _, errs := e.run(a, "release")
	if code != 1 {
		t.Fatalf("downが失敗したreleaseの終了code: got %d want 1（%s）", code, errs)
	}
	if st := mustStatus(t, e, a); st.Slots[0].State != "free" {
		t.Fatalf("downが失敗してもleaseは消えるはず: %+v", st.Slots[0])
	}
	var failed bool
	for _, ev := range readEvents(t, e.home) {
		if ev.Event == "down" && ev.OK != nil && !*ev.OK && ev.Error != "" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("downの失敗が記録に無い")
	}
}

// upが失敗したら、leaseを消して失敗を返す。
func TestAcquireFailsAndDropsLeaseWhenUpFails(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(`
project = "demo"
[slots]
count = 1
[commands]
up = "false"
`)
	a := holderDir(t, root, "a")
	code, _, errs := e.run(a, "acquire")
	if code != 1 {
		t.Fatalf("up失敗のacquireの終了code: got %d want 1（%s）", code, errs)
	}
	if st := mustStatus(t, e, a); st.Slots[0].State != "free" {
		t.Fatalf("upが失敗したらleaseは消えるはず: %+v", st.Slots[0])
	}
}

// commandには、枠の情報とmachine設定のenvが渡り、holderのworktreeをcwdに走る。
func TestCommandEnvAndCwd(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	writeFile(t, filepath.Join(e.home, "config.toml"), `
[projects.demo]
env = { EXTRA = "~/extra" }
`)
	root := e.project(fmt.Sprintf(`
project = "demo"
[slots]
count = 2
[ports]
names = ["web", "api_db"]
[commands]
up = 'echo "$SLOTCTL_PROJECT $SLOTCTL_SLOT $SLOTCTL_NAME $SLOTCTL_HOLDER $SLOTCTL_PORT_WEB $SLOTCTL_PORT_API_DB $EXTRA $(pwd -P)" >> %s'
`, logFile))
	mustAcquire(t, e, holderDir(t, root, "a"))
	b := holderDir(t, root, "b")
	mustAcquire(t, e, b)
	lines := readLines(t, logFile)
	want := fmt.Sprintf("demo 2 demo-2 %s 12100 12101 %s/extra %s", b, e.home, b)
	if len(lines) != 2 || lines[1] != want {
		t.Fatalf("commandのenv:\n got %q\nwant %q", lines, want)
	}
}

func TestUsageErrors(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(`project = "demo"`)
	for _, args := range [][]string{
		{},
		{"nope"},
		{"acquire", "--bogus"},
		{"acquire", "extra"},
		{"renew", "extra"},
	} {
		if code, _, _ := e.run(root, args...); code != 2 {
			t.Errorf("slotctl %v の終了code: got %d want 2", args, code)
		}
	}
	// slotctl.tomlが無いと、acquire・statusは使い方の誤り。renew・releaseはhookから呼ばれるので何もしない。
	bare := t.TempDir()
	for _, cmd := range []string{"acquire", "status"} {
		if code, _, _ := e.run(bare, cmd); code != 2 {
			t.Errorf("slotctl %s（設定なし）の終了code: got %d want 2", cmd, code)
		}
	}
	for _, cmd := range []string{"renew", "release"} {
		if code, _, errs := e.run(bare, cmd); code != 0 || errs != "" {
			t.Errorf("slotctl %s（設定なし）: code=%d err=%q", cmd, code, errs)
		}
	}
}
