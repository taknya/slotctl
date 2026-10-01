package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// poolAcquire は、acquire --json の出力（poolを含む）である。
type poolAcquire struct {
	Pool      string         `json:"pool"`
	Slot      int            `json:"slot"`
	Name      string         `json:"name"`
	Holder    string         `json:"holder"`
	Ports     map[string]int `json:"ports"`
	ExpiresAt string         `json:"expires_at"`
}

func acquirePool(t *testing.T, e *testEnv, cwd string, pool ...string) poolAcquire {
	t.Helper()
	if len(pool) == 0 {
		pool = []string{"dev"}
	}
	args := append([]string{"acquire"}, pool...)
	args = append(args, "--json")
	code, out, errs := e.run(cwd, args...)
	if code != 0 {
		t.Fatalf("slotctl %v が終了code %d（stderr: %s）", args, code, errs)
	}
	var res poolAcquire
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("acquire --json の出力が不正: %q: %v", out, err)
	}
	return res
}

// poolSlot は、status --json の1要素（poolを含む）である。
type poolSlot struct {
	Pool      string         `json:"pool"`
	Slot      int            `json:"slot"`
	Name      string         `json:"name"`
	State     string         `json:"state"`
	Holder    string         `json:"holder"`
	ExpiresAt string         `json:"expires_at"`
	Ports     map[string]int `json:"ports"`
}

func statusPools(t *testing.T, e *testEnv, cwd string) []poolSlot {
	t.Helper()
	code, out, errs := e.run(cwd, "status", "--json")
	if code != 0 {
		t.Fatalf("status が終了code %d（stderr: %s）", code, errs)
	}
	var res struct {
		Slots []poolSlot `json:"slots"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("status --json の出力が不正: %q: %v", out, err)
	}
	return res.Slots
}

func findSlot(t *testing.T, slots []poolSlot, pool string, slot int) poolSlot {
	t.Helper()
	for _, s := range slots {
		if s.Pool == pool && s.Slot == slot {
			return s
		}
	}
	t.Fatalf("pool %q の slot %d が status に無い: %+v", pool, slot, slots)
	return poolSlot{}
}

// rawEvents は、記録の各行をmapとして読む（項目の有無を確かめるため）。
func rawEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	var out []map[string]any
	for _, f := range files {
		for _, line := range readLines(t, f) {
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("記録の行がJSONではありません: %q: %v", line, err)
			}
			out = append(out, m)
		}
	}
	return out
}

// 明示したdev poolの名前・port・env・status JSONの項目を確かめる。
func TestExplicitDevPoolKeepsOutputFields(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(`
project = "demo"
[pools.dev]
ports = ["web", "db"]
up = 'echo "$SLOTCTL_NAME $SLOTCTL_SLOT $SLOTCTL_PORT_WEB $SLOTCTL_PORT_DB $SLOTCTL_POOL" >> %s'
`, logFile), map[string]int{"dev": 2})
	a := holderDir(t, root, "a")

	code, out, errs := e.run(a, "acquire", "dev")
	want := fmt.Sprintf("slot: 1\nname: demo-dev-1\nport: web=12000 db=12001\nexpires: %s\n", e.now.Add(10*time.Minute).Format(time.RFC3339))
	if code != 0 || out != want {
		t.Fatalf("acquireの出力が変わっています: code=%d\n got %q\nwant %q (%s)", code, out, want, errs)
	}
	if lines := readLines(t, logFile); len(lines) != 1 || lines[0] != "demo-dev-1 1 12000 12001 dev" {
		t.Fatalf("upのenv: %v", lines)
	}

	// status --json の既存項目は変わらず、poolだけが足される。
	code, out, _ = e.run(a, "status", "--json")
	if code != 0 {
		t.Fatalf("status: %d", code)
	}
	var st struct {
		Project string           `json:"project"`
		Slots   []map[string]any `json:"slots"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if st.Project != "demo" || len(st.Slots) != 2 {
		t.Fatalf("status: %+v", st)
	}
	keys := func(m map[string]any) []string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	if got, want := keys(st.Slots[0]), []string{"expires_at", "holder", "name", "operations_defined", "pool", "ports", "slot", "state"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lentの項目: got %v want %v", got, want)
	}
	if got, want := keys(st.Slots[1]), []string{"name", "operations_defined", "pool", "ports", "slot", "state"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("freeの項目: got %v want %v", got, want)
	}
	if st.Slots[0]["pool"] != "dev" || st.Slots[0]["name"] != "demo-dev-1" || st.Slots[0]["state"] != "lent" ||
		st.Slots[0]["holder"] != a || st.Slots[1]["name"] != "demo-dev-2" || st.Slots[1]["state"] != "free" {
		t.Fatalf("status の値: %+v", st.Slots)
	}
	if p := st.Slots[1]["ports"].(map[string]any); p["web"] != float64(12100) || p["db"] != float64(12101) {
		t.Fatalf("free枠のport: %+v", p)
	}
}

const twoPoolTOML = `
project = "demo"
[lease]
ttl = "10m"
[pools.dev]
up = 'echo "up dev $SLOTCTL_HOLDER $SLOTCTL_SLOT" >> %[1]s'
down = 'echo "down dev $SLOTCTL_HOLDER $SLOTCTL_SLOT" >> %[1]s'
[pools.billing]
up = 'echo "up billing $SLOTCTL_HOLDER $SLOTCTL_SLOT $SLOTCTL_POOL" >> %[1]s'
down = 'echo "down billing $SLOTCTL_HOLDER $SLOTCTL_SLOT $SLOTCTL_POOL" >> %[1]s'
`

// AC-2 (a): 同じ借り手が、dev poolとbillingの枠を同時に持てる。1poolでは1枠まで。
func TestSameHolderHoldsSlotsInDifferentPools(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile), map[string]int{"dev": 2, "billing": 1})
	a := holderDir(t, root, "a")

	d := acquirePool(t, e, a)
	b := acquirePool(t, e, a, "billing")
	if d.Pool != "dev" || d.Slot != 1 || d.Name != "demo-dev-1" {
		t.Fatalf("dev pool: %+v", d)
	}
	if b.Pool != "billing" || b.Slot != 1 || b.Name != "demo-billing-1" || b.Holder != a {
		t.Fatalf("billing: %+v", b)
	}
	// 同じpoolを続けて借りても延長で、枠は増えずupも走らない（冪等）。
	if again := acquirePool(t, e, a, "billing"); again.Slot != 1 {
		t.Fatalf("billingの再acquire: %+v", again)
	}
	if again := acquirePool(t, e, a); again.Slot != 1 {
		t.Fatalf("dev poolの再acquire: %+v", again)
	}
	if lines := readLines(t, logFile); len(lines) != 2 ||
		lines[0] != "up dev "+a+" 1" || lines[1] != "up billing "+a+" 1 billing" {
		t.Fatalf("up: %v", lines)
	}
	slots := statusPools(t, e, a)
	if len(slots) != 3 {
		t.Fatalf("dev 2枠＋billing1枠のはず: %+v", slots)
	}
	if findSlot(t, slots, "dev", 1).Holder != a || findSlot(t, slots, "billing", 1).Holder != a ||
		findSlot(t, slots, "dev", 2).State != "free" {
		t.Fatalf("status: %+v", slots)
	}
}

// acquireは指定poolに加え、別poolの期限切れも回収する。
func TestAcquireReclaimsExpiredAcrossPools(t *testing.T) {
	e := newTestEnv(t)
	log := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, log), map[string]int{"dev": 2, "billing": 1})
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
	acquirePool(t, e, a)
	acquirePool(t, e, a, "billing")
	if code, _, _ := e.run(b, "acquire", "billing"); code != 3 {
		t.Fatalf("code=%d", code)
	}
	e.advance(10 * time.Minute)
	acquirePool(t, e, b, "billing")
	if s := findSlot(t, statusPools(t, e, b), "dev", 1); s.State != "free" {
		t.Fatalf("status: %+v", s)
	}
	n := 0
	for _, ev := range rawEvents(t, e.home) {
		if ev["event"] == "reclaim" {
			n++
			if ev["trigger"] != "acquire" || ev["holder"] != a {
				t.Fatalf("event: %+v", ev)
			}
		}
	}
	if n != 2 {
		t.Fatalf("reclaims=%d", n)
	}
}

func TestUnknownPoolIsUsageError(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(fmt.Sprintf(twoPoolTOML, filepath.Join(t.TempDir(), "log")), map[string]int{"dev": 2, "billing": 1})
	a := holderDir(t, root, "a")
	for _, args := range [][]string{
		{"acquire", "nope"},
		{"acquire", "nope", "--json"},
		{"acquire", "dev", "--json", "nope"},
		{"release", "nope"},
		{"acquire", "billing", "extra"},
		{"release", "billing", "extra"},
		{"renew", "billing"},
		{"status", "billing"},
	} {
		if code, _, _ := e.run(a, args...); code != 2 {
			t.Errorf("slotctl %v の終了code: got %d want 2", args, code)
		}
	}
	// 設定されたdev poolは明示して借りられる。
	if got := acquirePool(t, e, a, "dev"); got.Pool != "dev" || got.Slot != 1 {
		t.Fatalf("acquire default: %+v", got)
	}
}

// AC-2 (e): poolの設定の誤り。
func TestInvalidPoolConfigIsRejected(t *testing.T) {
	for name, toml := range map[string]string{
		"旧slotsは誤り":             "project = \"demo\"\n[slots]\ncount = 1\n",
		"pool名が大文字":             "project = \"demo\"\n[pools.Billing]\n",
		"pool名が数字で始まる":          "project = \"demo\"\n[pools.\"1x\"]\n",
		"pool名に使えない文字":          "project = \"demo\"\n[pools.a_b]\n",
		"pool名が空":               "project = \"demo\"\n[pools.\"\"]\n",
		"poolの未知のkey":           "project = \"demo\"\n[pools.billing]\nbogus = 1\n",
		"repositoryのcountは誤り":   "project = \"demo\"\n[pools.billing]\ncount = 11\n",
		"repositoryの負のcountは誤り": "project = \"demo\"\n[pools.billing]\ncount = -1\n",
		"poolのport名が大文字で重なる":    "project = \"demo\"\n[pools.billing]\nports = [\"a\", \"A\"]\n",
		"poolのport名に使えない文字":     "project = \"demo\"\n[pools.billing]\nports = [\"a-b\"]\n",
		"pools直下の未知のkey":        "project = \"demo\"\n[pools]\nbogus = 1\n",
	} {
		e := newTestEnv(t)
		root := e.project(toml)
		for _, cmd := range []string{"acquire", "status"} {
			args := []string{cmd}
			if cmd == "acquire" {
				args = append(args, "dev")
			}
			if code, _, errs := e.run(root, args...); code != 1 {
				t.Errorf("%s: slotctl %s の終了code got %d want 1（%s）", name, cmd, code, errs)
			}
		}
	}
}

// AC-3: renewは借り手の全poolのleaseを延ばす。commandも記録も走らない。
func TestRenewExtendsEveryPool(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile), map[string]int{"dev": 2, "billing": 1})
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
	acquirePool(t, e, a)
	acquirePool(t, e, a, "billing")
	acquirePool(t, e, b)
	before := len(rawEvents(t, e.home))
	logBefore := len(readLines(t, logFile))

	e.advance(7 * time.Minute)
	if code, out, errs := e.run(a, "renew"); code != 0 || out != "" || errs != "" {
		t.Fatalf("renew: code=%d out=%q err=%q", code, out, errs)
	}
	slots := statusPools(t, e, a)
	want := e.now.Add(10 * time.Minute).Format(time.RFC3339)
	if s := findSlot(t, slots, "dev", 1); s.ExpiresAt != want {
		t.Fatalf("dev poolの期限: %+v want %s", s, want)
	}
	if s := findSlot(t, slots, "billing", 1); s.ExpiresAt != want {
		t.Fatalf("billingの期限: %+v want %s", s, want)
	}
	// 他の借り手のleaseは延びない。
	if s := findSlot(t, slots, "dev", 2); s.ExpiresAt == want || s.Holder != b {
		t.Fatalf("bのleaseが延びた: %+v", s)
	}
	if len(rawEvents(t, e.home)) != before || len(readLines(t, logFile)) != logBefore {
		t.Fatal("renewは記録もcommandも走らせないはず")
	}
}

// AC-3: release [pool]は指定したpoolだけ、省略すれば借り手の全poolの枠を返す。
func TestReleaseOnePoolOrAllPools(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile), map[string]int{"dev": 2, "billing": 1})
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
	acquirePool(t, e, a)
	acquirePool(t, e, a, "billing")
	acquirePool(t, e, b)

	if code, _, errs := e.run(a, "release", "billing"); code != 0 {
		t.Fatalf("release billing: %d %s", code, errs)
	}
	slots := statusPools(t, e, a)
	if findSlot(t, slots, "billing", 1).State != "free" || findSlot(t, slots, "dev", 1).Holder != a {
		t.Fatalf("billingだけが返るはず: %+v", slots)
	}
	if lines := readLines(t, logFile); lines[len(lines)-1] != "down billing "+a+" 1 billing" {
		t.Fatalf("billingのdown: %v", lines)
	}

	acquirePool(t, e, a, "billing")
	if code, _, errs := e.run(a, "release"); code != 0 {
		t.Fatalf("release: %d %s", code, errs)
	}
	slots = statusPools(t, e, a)
	if findSlot(t, slots, "billing", 1).State != "free" || findSlot(t, slots, "dev", 1).State != "free" {
		t.Fatalf("全poolの枠が返るはず: %+v", slots)
	}
	if findSlot(t, slots, "dev", 2).Holder != b {
		t.Fatalf("bの枠は残るはず: %+v", slots)
	}
	downs := 0
	for _, l := range readLines(t, logFile) {
		if strings.HasPrefix(l, "down ") {
			downs++
		}
	}
	if downs != 3 { // billing（1回目）＋ billing・default（2回目）
		t.Fatalf("downの回数: %d %v", downs, readLines(t, logFile))
	}
	var rel []string
	for _, ev := range rawEvents(t, e.home) {
		if ev["event"] == "release" {
			rel = append(rel, fmt.Sprint(ev["pool"]))
		}
	}
	if !reflect.DeepEqual(rel, []string{"billing", "dev", "billing"}) && !reflect.DeepEqual(rel, []string{"billing", "billing", "dev"}) {
		t.Fatalf("releaseの記録のpool: %v", rel)
	}
}

// down失敗のleaseを保持し、他のpoolの返却は続ける。
func TestReleaseKeepsFailedLeaseAndReleasesOtherPools(t *testing.T) {
	for name, failing := range map[string]string{"dev": "dev", "billing": "billing"} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			logFile := filepath.Join(t.TempDir(), "log")
			root := e.project(fmt.Sprintf(`
project = "demo"
[pools.dev]
down = 'echo "down dev" >> %[1]s; [ %[2]s != dev ]'
[pools.billing]
down = 'echo "down billing" >> %[1]s; [ %[2]s != billing ]'
`, logFile, failing), map[string]int{"dev": 1})
			a := holderDir(t, root, "a")
			acquirePool(t, e, a)
			acquirePool(t, e, a, "billing")

			code, _, errs := e.run(a, "release")
			if code != 1 {
				t.Fatalf("downが失敗したreleaseの終了code: got %d want 1（%s）", code, errs)
			}
			for _, s := range statusPools(t, e, a) {
				want := "free"
				if s.Pool == failing {
					want = "lent"
				}
				if s.State != want {
					t.Fatalf("成功したleaseだけ消えるはず: %+v", s)
				}
			}
			lines := readLines(t, logFile)
			sort.Strings(lines)
			if !reflect.DeepEqual(lines, []string{"down billing", "down dev"}) {
				t.Fatalf("両方のdownが走るはず: %v", lines)
			}
			var failed []string
			for _, ev := range rawEvents(t, e.home) {
				if ev["event"] == "down" && ev["ok"] == false {
					failed = append(failed, fmt.Sprint(ev["pool"]))
				}
			}
			if !reflect.DeepEqual(failed, []string{failing}) {
				t.Fatalf("失敗したdownの記録: %v", failed)
			}
		})
	}
}

// AC-3: 借りていなければrenew・releaseは0。slotctl.tomlが無くても0。
func TestRenewAndReleaseWithoutLeaseAreNoOps(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile), map[string]int{"dev": 2, "billing": 1})
	nobody := holderDir(t, root, "nobody")
	for _, args := range [][]string{{"renew"}, {"release"}, {"release", "billing"}, {"release", "dev"}} {
		if code, out, errs := e.run(nobody, args...); code != 0 || out != "" || errs != "" {
			t.Errorf("slotctl %v（leaseなし）: code=%d out=%q err=%q", args, code, out, errs)
		}
	}
	if len(readLines(t, logFile)) != 0 {
		t.Fatal("leaseが無いときはcommandを走らせないはず")
	}
	bare := t.TempDir()
	if code, _, errs := e.run(bare, "release", "billing"); code != 0 || errs != "" {
		t.Errorf("slotctl release billing（設定なし）: code=%d err=%q", code, errs)
	}
}

// AC-4: 名前つきpoolの名前と、pool専用のport帯。portはportの名前を持つpoolだけにある。
func TestNamedPoolNamesAndPortBands(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(`
project = "demo"
[pools.dev]
ports = ["web", "api"]
[pools.billing]
ports = ["x", "y"]
[pools.tunnel]
`, map[string]int{"dev": 2, "billing": 2})
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")

	d := acquirePool(t, e, a)
	if d.Ports["web"] != 12000 || d.Ports["api"] != 12001 {
		t.Fatalf("dev poolの帯は今のまま: %+v", d.Ports)
	}
	b1 := acquirePool(t, e, a, "billing")
	b2 := acquirePool(t, e, b, "billing")
	if b1.Name != "demo-billing-1" || b2.Name != "demo-billing-2" {
		t.Fatalf("名前: %s %s", b1.Name, b2.Name)
	}
	if len(b1.Ports) != 2 || b1.Ports["x"] != 13000 || b1.Ports["y"] != 13001 || b2.Ports["x"] != 13100 || b2.Ports["y"] != 13101 {
		t.Fatalf("billingのport（既定の帯と重ならない専用の帯）: %+v %+v", b1.Ports, b2.Ports)
	}
	tu := acquirePool(t, e, a, "tunnel")
	if tu.Name != "demo-tunnel-1" || len(tu.Ports) != 0 {
		t.Fatalf("portの名前を持たないpoolはportを持たない: %+v", tu)
	}

	// statusでも同じ。帯は記録されて変わらない。
	slots := statusPools(t, e, a)
	if len(slots) != 5 {
		t.Fatalf("dev 2＋billing2＋tunnel1: %+v", slots)
	}
	if s := findSlot(t, slots, "billing", 2); s.Ports["x"] != 13100 || s.State != "lent" {
		t.Fatalf("billing slot 2: %+v", s)
	}
	if s := findSlot(t, slots, "tunnel", 1); len(s.Ports) != 0 || s.Name != "demo-tunnel-1" {
		t.Fatalf("tunnel slot 1: %+v", s)
	}
	if s := findSlot(t, slots, "dev", 2); s.Ports["web"] != 12100 {
		t.Fatalf("既定 slot 2: %+v", s)
	}

	// 別のprojectの帯は、machineで空いている帯から割り当たる（dev・billingの帯と重ならない）。
	other := e.project("project = \"other\"\n[pools.dev]\nports = [\"web\"]\n[pools.q]\nports = [\"z\"]\n")
	o := acquirePool(t, e, other)
	oq := acquirePool(t, e, other, "q")
	if o.Ports["web"] != 14000 || oq.Ports["z"] != 15000 {
		t.Fatalf("空いている帯から: %+v %+v", o.Ports, oq.Ports)
	}
	// dev pool・billingの帯は変わらない。
	if again := acquirePool(t, e, a, "billing"); again.Ports["x"] != 13000 {
		t.Fatalf("帯は変わらないはず: %+v", again.Ports)
	}
}

// AC-4: SLOTCTL_POOL・SLOTCTL_NAME・SLOTCTL_PORT_*（そのpoolのportだけ）がcommandに渡る。
func TestPoolEnvForCommands(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(`
project = "demo"
[pools.dev]
ports = ["web"]
up = 'echo "$SLOTCTL_POOL $SLOTCTL_NAME ${SLOTCTL_PORT_WEB-unset} ${SLOTCTL_PORT_X-unset}" >> %[1]s'
[pools.billing]
ports = ["x"]
up = 'echo "$SLOTCTL_POOL $SLOTCTL_NAME ${SLOTCTL_PORT_WEB-unset} ${SLOTCTL_PORT_X-unset}" >> %[1]s'
`, logFile))
	acquirePool(t, e, holderDir(t, root, "a"))
	acquirePool(t, e, holderDir(t, root, "a"), "billing")
	got := readLines(t, logFile)
	want := []string{"dev demo-dev-1 12000 unset", "billing demo-billing-1 unset 13000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env:\n got %q\nwant %q", got, want)
	}
}

// AC-4: statusの表にPOOL列があり、全poolの全枠が並ぶ。記録の各行にpoolがある。
func TestStatusTableAndEventsCarryPool(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(`
project = "demo"
[pools.dev]
up = "true"
down = "true"
[pools.billing]
up = "true"
down = "true"
`, map[string]int{"dev": 1})
	a := holderDir(t, root, "a")
	acquirePool(t, e, a)
	acquirePool(t, e, a, "billing")
	e.run(a, "release", "billing")
	e.run(holderDir(t, root, "b"), "acquire", "billing")
	e.run(holderDir(t, root, "c"), "acquire", "billing") // 空き無し

	code, out, _ := e.run(a, "status")
	if code != 0 {
		t.Fatalf("status: %d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "POOL") || !strings.Contains(lines[0], "SLOT") {
		t.Fatalf("表の見出し: %q", lines)
	}
	if f := strings.Fields(lines[2]); f[0] != "dev" || f[2] != "demo-dev-1" {
		t.Fatalf("dev poolの行: %q", lines[1])
	}
	if f := strings.Fields(lines[1]); f[0] != "billing" || f[2] != "demo-billing-1" {
		t.Fatalf("billingの行: %q", lines[2])
	}

	evs := rawEvents(t, e.home)
	if len(evs) == 0 {
		t.Fatal("記録が無い")
	}
	pools := map[string]bool{}
	for _, ev := range evs {
		p, ok := ev["pool"].(string)
		if !ok || p == "" {
			t.Fatalf("記録の各行にpoolがあるはず: %+v", ev)
		}
		pools[p] = true
	}
	if !pools["dev"] || !pools["billing"] {
		t.Fatalf("記録のpool: %v", pools)
	}
	// 空きが無くて借りられなかった記録にもpoolがある。
	last := evs[len(evs)-1]
	if last["event"] != "acquire" || last["ok"] != false || last["pool"] != "billing" {
		t.Fatalf("最後の記録: %+v", last)
	}
}

// machineの設定がpoolごとの数を定義する。
func TestMachineConfigDefinesPoolSlots(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.home, "config.toml"), `
[projects.demo.pools.dev]
slots = 1
[projects.demo.pools.billing]
slots = 3
[projects.other.pools.billing]
slots = 9
`)
	root := e.project("project = \"demo\"\n[pools.dev]\n[pools.billing]\n[pools.tunnel]\n", map[string]int{"dev": 2, "billing": 1, "tunnel": 2})
	slots := statusPools(t, e, root)
	count := map[string]int{}
	for _, s := range slots {
		count[s.Pool]++
	}
	if count["dev"] != 1 || count["billing"] != 3 || count["tunnel"] != 2 {
		t.Fatalf("machine設定の数: %v", count)
	}
	// 未知のkeyは、既存のmachine設定と同じくエラー。
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.demo.pools.billing]\nbogus = 1\n")
	if code, _, _ := e.run(root, "status"); code != 1 {
		t.Fatalf("machine設定のpoolの未知のkeyはエラーのはず: %d", code)
	}
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.demo]\nslots = 2\n")
	if code, _, _ := e.run(root, "status"); code != 1 {
		t.Fatalf("machine設定の[pools.default]はエラーのはず: %d", code)
	}
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.demo.pools.billing]\nslots = 11\n")
	if code, _, _ := e.run(root, "status"); code != 1 {
		t.Fatalf("machine設定のpoolの数が範囲外ならエラーのはず: %d", code)
	}
}

// v0.1.0のstate.db（user_version 1）を作る。
func makeV1State(t *testing.T, home, repo string, portBase int, leases [][]any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE projects (name TEXT PRIMARY KEY, repo TEXT NOT NULL, port_base INTEGER NOT NULL)`,
		`CREATE TABLE leases (project TEXT NOT NULL, slot INTEGER NOT NULL, holder TEXT NOT NULL, acquired_at INTEGER NOT NULL, renewed_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY (project, slot))`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO projects VALUES ('demo', ?, ?)`, repo, portBase); err != nil {
		t.Fatal(err)
	}
	for _, l := range leases {
		if _, err := db.Exec(`INSERT INTO leases VALUES ('demo', ?, ?, ?, ?, ?)`, l...); err != nil {
			t.Fatal(err)
		}
	}
}

func userVersion(t *testing.T, home string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// v0.1.0のleaseを保持して移行し、設定したdev poolに自動で付け替えない。
func TestV1StateRetainsLegacyLeaseWithoutInventingConfiguredPool(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(`project = "demo"
[pools.dev]
`, map[string]int{"dev": 3})
	a := holderDir(t, root, "a")
	realRoot, _ := filepath.EvalSymlinks(root)
	makeV1State(t, e.home, realRoot, 14000, [][]any{{1, a, 1, 1, 99999999999}})
	slots := statusPools(t, e, a)
	if len(slots) != 4 || slots[0].Pool != "default" || slots[0].State != "lent" || slots[0].Holder != a || slots[1].Pool != "dev" || slots[1].State != "free" {
		t.Fatalf("status: %+v", slots)
	}
	if v := userVersion(t, e.home); v != 2 {
		t.Fatalf("version=%d", v)
	}
}

func TestStateNewerThanThisVersionIsRejected(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project = \"demo\"\n[pools.dev]\n")
	mustAcquire(t, e, root) // schemaを作る
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	code, _, errs := e.run(root, "status")
	if code != 1 || !strings.Contains(errs, "新しい") {
		t.Fatalf("新しい版のstate.dbは拒むはず: code=%d %s", code, errs)
	}
	_ = os.Remove(filepath.Join(e.home, "state.db"))
}

// 設定から消えたpoolはdownできないのでleaseを保持して知らせる。
func TestReleaseKeepsLeaseOfPoolRemovedFromConfig(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile), map[string]int{"dev": 2, "billing": 1})
	a := holderDir(t, root, "a")
	acquirePool(t, e, a, "billing")
	writeFile(t, filepath.Join(root, "slotctl.toml"), "project = \"demo\"\n")

	code, _, errs := e.run(a, "release")
	if code != 1 || !strings.Contains(errs, "billing") {
		t.Fatalf("release: code=%d %s", code, errs)
	}
	for _, l := range readLines(t, logFile) {
		if strings.HasPrefix(l, "down billing") {
			t.Fatalf("設定に無いpoolのdownは走らないはず: %v", l)
		}
	}
	writeFile(t, filepath.Join(root, "slotctl.toml"), fmt.Sprintf(twoPoolTOML, logFile))
	if s := findSlot(t, statusPools(t, e, a), "billing", 1); s.State != "lent" {
		t.Fatalf("leaseは残るはず: %+v", s)
	}
}

// machineが数を持ち、worktreeの操作の違いで一覧が変わらない。
func TestMachineInventoryAcrossWorktrees(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='p'\n[pools.dev]\ndown='true'\n", map[string]int{"dev": 2})
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git(root, "init", "-q")
	git(root, "add", "slotctl.toml")
	git(root, "commit", "-q", "-m", "fixture")
	a, b, c := root, filepath.Join(t.TempDir(), "b"), filepath.Join(t.TempDir(), "c")
	git(root, "worktree", "add", "-q", b)
	git(root, "worktree", "add", "-q", c)
	writeFile(t, filepath.Join(a, "slotctl.toml"), "project='p'\n[pools.dev]\ndown='echo down'\n")
	one, two := acquirePool(t, e, a), acquirePool(t, e, b)
	if one.Name != "p-dev-1" || two.Name != "p-dev-2" {
		t.Fatalf("名前: %+v %+v", one, two)
	}
	if code, _, errs := e.run(c, "acquire", "dev"); code != 3 {
		t.Fatalf("満杯: %d %s", code, errs)
	}
	sa, sb := mustStatus(t, e, a), mustStatus(t, e, b)
	if len(sa.Slots) != 2 || !reflect.DeepEqual(sa, sb) {
		t.Fatalf("一覧不一致: %+v %+v", sa, sb)
	}
	t.Logf("AC1/3: %s %s、第三acquire exit3、status一致 %v", one.Name, two.Name, sa.Slots)
}
func TestMissingMachinePoolAndRepositoryCountGuidance(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='p'\n[pools.dev]\ndown='true'\n")
	writeFile(t, filepath.Join(e.home, "config.toml"), "")
	if st := mustStatus(t, e, root); len(st.Slots) != 0 {
		t.Fatalf("枠なし: %+v", st)
	}
	check := func(args ...string) {
		t.Helper()
		code, _, errs := e.run(root, args...)
		if code != 1 || !strings.Contains(errs, filepath.Join(e.home, "config.toml")) || !strings.Contains(errs, "[projects.p.pools.dev] slots") {
			t.Fatalf("%v: %d %s", args, code, errs)
		}
		t.Logf("AC2 %v: exit%d %s", args, code, errs)
	}
	check("acquire", "dev")
	if code, _, _ := e.run(root, "acquire", "unknown"); code != 2 {
		t.Fatalf("未知pool: %d", code)
	}
	writeFile(t, filepath.Join(root, "slotctl.toml"), "project='p'\n[pools.dev]\ncount=3\n")
	check("acquire", "dev")
	check("status")
}
func TestMachineOnlyPoolStatusAndUnknownAcquire(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='p'\n")
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.p.pools.dev]\nslots=2\n")
	if st := mustStatus(t, e, root); len(st.Slots) != 2 {
		t.Fatalf("machineのみ: %+v", st)
	}
	if code, _, _ := e.run(root, "acquire", "dev"); code != 2 {
		t.Fatalf("操作未定義: %d", code)
	}
	code, out, errs := e.run(root, "status")
	if code != 0 || !strings.Contains(out, "未定義") {
		t.Fatalf("down列: %d %s %s", code, out, errs)
	}
}
func TestShrunkAndRemovedMachinePoolRetainsLease(t *testing.T) {
	for _, command := range []string{"release", "reclaim"} {
		t.Run(command, func(t *testing.T) {
			e := newTestEnv(t)
			root := e.project("project='p'\n[pools.dev]\nports=['web']\ndown='true'\n", map[string]int{"dev": 2})
			a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
			one, two := acquirePool(t, e, a), acquirePool(t, e, b)
			writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.p.pools.dev]\nslots=1\n")
			st := mustStatus(t, e, root)
			if len(st.Slots) != 2 || st.Slots[1].State != "lent" || st.Slots[1].Holder != two.Holder || !reflect.DeepEqual(st.Slots[1].Ports, two.Ports) {
				t.Fatalf("縮小: %+v", st)
			}
			if command == "reclaim" {
				e.advance(31 * time.Minute)
			}
			if code, _, errs := e.run(b, command); code != 0 {
				t.Fatalf("返却: %d %s", code, errs)
			}
			st = mustStatus(t, e, root)
			if len(st.Slots) != 1 {
				t.Fatalf("返却後: %+v", st)
			}
			if command == "reclaim" {
				one = acquirePool(t, e, a)
			}
			writeFile(t, filepath.Join(e.home, "config.toml"), "")
			st = mustStatus(t, e, root)
			if len(st.Slots) != 1 || st.Slots[0].State != "lent" || st.Slots[0].Holder != one.Holder {
				t.Fatalf("削除: %+v", st)
			}
			if command == "reclaim" {
				e.advance(31 * time.Minute)
			}
			if code, _, errs := e.run(a, command); code != 0 {
				t.Fatalf("削除後返却: %d %s", code, errs)
			}
			if st := mustStatus(t, e, root); len(st.Slots) != 0 {
				t.Fatalf("枠が捏造された: %+v", st)
			}
		})
	}
}

func TestIncreasingMachineSlotsAllowsNextAcquire(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='p'\n[pools.dev]\ndown='true'\n")
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
	first := acquirePool(t, e, a)
	if code, _, _ := e.run(b, "acquire", "dev"); code != 3 {
		t.Fatalf("拡張前: %d", code)
	}
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.p.pools.dev]\nslots=2\n")
	second := acquirePool(t, e, b)
	if first.Slot != 1 || second.Slot != 2 || len(mustStatus(t, e, root).Slots) != 2 {
		t.Fatalf("拡張後: %+v %+v", first, second)
	}
}
func TestRemovedOperationKeepsLeaseOnReclaim(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='p'\n[pools.dev]\ndown='true'\n")
	original := acquirePool(t, e, root)
	writeFile(t, filepath.Join(root, "slotctl.toml"), "project='p'\n")
	writeFile(t, filepath.Join(e.home, "config.toml"), "")
	e.advance(31 * time.Minute)
	code, _, errs := e.run(root, "reclaim")
	if code != 1 || !strings.Contains(errs, "downを実行できません") {
		t.Fatalf("警告: %d %s", code, errs)
	}
	st := mustStatus(t, e, root)
	if len(st.Slots) != 1 || st.Slots[0].Holder != original.Holder || st.Slots[0].State != "lent" || st.Slots[0].OperationsDefined {
		t.Fatalf("保持lease: %+v", st)
	}
}
