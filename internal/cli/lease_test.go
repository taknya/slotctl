package cli

import (
	"fmt"
	"path/filepath"
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
[pools.dev]
count = 3
ports = ["web", "db"]
up = 'echo "up $SLOTCTL_SLOT $SLOTCTL_PORT_WEB" >> %s'
`, logFile))
	a := holderDir(t, root, "a")
	b := holderDir(t, root, "b")

	first := mustAcquire(t, e, a)
	if first.Slot != 1 || first.Name != "demo-dev-1" || first.Holder != a {
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

// 期限切れを全て回収し、空きの最小番号を借りる。
func TestAcquireReclaimsAllExpiredAndUsesSmallestSlot(t *testing.T) {
	e := newTestEnv(t)
	log := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(`project = "demo"
[pools.dev]
count = 3
down = 'echo "$SLOTCTL_NAME $SLOTCTL_HOLDER $(pwd -P)" >> %s'
`, log))
	a, b, c := holderDir(t, root, "a"), holderDir(t, root, "b"), holderDir(t, root, "c")
	mustAcquire(t, e, a)
	mustAcquire(t, e, b)
	e.advance(10 * time.Minute)
	got := mustAcquire(t, e, c)
	if got.Slot != 1 {
		t.Fatalf("grant: %+v", got)
	}
	lines := readLines(t, log)
	if len(lines) != 2 || lines[0] != "demo-dev-1 "+a+" "+a || lines[1] != "demo-dev-2 "+b+" "+b {
		t.Fatalf("down: %v", lines)
	}
	st := mustStatus(t, e, c)
	if st.Slots[1].State != "free" {
		t.Fatalf("status: %+v", st)
	}
	n := 0
	for _, ev := range rawEvents(t, e.home) {
		if ev["event"] == "preempt" {
			t.Fatal("preempt event")
		}
		if ev["event"] == "reclaim" {
			n++
			if ev["trigger"] != "acquire" || ev["ok"] != true {
				t.Fatalf("event: %+v", ev)
			}
		}
	}
	if n != 2 {
		t.Fatalf("reclaim events: %d", n)
	}
}

func TestRenewAndRelease(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	toml := func(down string) string {
		return fmt.Sprintf(`
project = "demo"
[lease]
ttl = "10m"
[pools.dev]
count = 2
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

	// downが失敗したleaseは保持し、終了code 1になる。
	writeFile(t, filepath.Join(root, "slotctl.toml"), toml("false"))
	mustAcquire(t, e, a)
	code, _, errs := e.run(a, "release")
	if code != 1 {
		t.Fatalf("downが失敗したreleaseの終了code: got %d want 1（%s）", code, errs)
	}
	if st := mustStatus(t, e, a); st.Slots[0].State != "lent" {
		t.Fatalf("downが失敗したらleaseは残るはず: %+v", st.Slots[0])
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
[pools.dev]
count = 1
up = "false"
`)
	a := holderDir(t, root, "a")
	code, _, errs := e.run(a, "acquire", "dev")
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
[pools.dev]
count = 2
ports = ["web", "api_db"]
up = 'echo "$SLOTCTL_PROJECT $SLOTCTL_SLOT $SLOTCTL_NAME $SLOTCTL_HOLDER $SLOTCTL_PORT_WEB $SLOTCTL_PORT_API_DB $EXTRA $(pwd -P)" >> %s'
`, logFile))
	mustAcquire(t, e, holderDir(t, root, "a"))
	b := holderDir(t, root, "b")
	mustAcquire(t, e, b)
	lines := readLines(t, logFile)
	want := fmt.Sprintf("demo 2 demo-dev-2 %s 12100 12101 %s/extra %s", b, e.home, b)
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
