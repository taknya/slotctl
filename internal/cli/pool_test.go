package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
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
	Pool           string         `json:"pool"`
	Slot           int            `json:"slot"`
	Name           string         `json:"name"`
	Holder         string         `json:"holder"`
	Ports          map[string]int `json:"ports"`
	ExpiresAt      string         `json:"expires_at"`
	PreviousHolder string         `json:"previous_holder"`
}

func acquirePool(t *testing.T, e *testEnv, cwd string, pool ...string) poolAcquire {
	t.Helper()
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

// AC-1: poolを書かない設定では、既定poolの枠の名前・port・env・status --jsonの既存項目が変わらない。
func TestNoPoolConfigKeepsDefaultPoolBehavior(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(`
project = "demo"
[slots]
count = 2
[ports]
names = ["web", "db"]
[commands]
up = 'echo "$SLOTCTL_NAME $SLOTCTL_SLOT $SLOTCTL_PORT_WEB $SLOTCTL_PORT_DB $SLOTCTL_POOL" >> %s'
`, logFile))
	a := holderDir(t, root, "a")

	code, out, errs := e.run(a, "acquire")
	want := fmt.Sprintf("slot: 1\nname: demo-1\nport: web=12000 db=12001\nexpires: %s\n", e.now.Add(10*time.Minute).Format(time.RFC3339))
	if code != 0 || out != want {
		t.Fatalf("acquireの出力が変わっています: code=%d\n got %q\nwant %q (%s)", code, out, want, errs)
	}
	if lines := readLines(t, logFile); len(lines) != 1 || lines[0] != "demo-1 1 12000 12001 default" {
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
	if got, want := keys(st.Slots[0]), []string{"expires_at", "holder", "name", "pool", "ports", "slot", "state"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lentの項目: got %v want %v", got, want)
	}
	if got, want := keys(st.Slots[1]), []string{"name", "pool", "ports", "slot", "state"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("freeの項目: got %v want %v", got, want)
	}
	if st.Slots[0]["pool"] != "default" || st.Slots[0]["name"] != "demo-1" || st.Slots[0]["state"] != "lent" ||
		st.Slots[0]["holder"] != a || st.Slots[1]["name"] != "demo-2" || st.Slots[1]["state"] != "free" {
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
[slots]
count = 2
[commands]
up = 'echo "up default $SLOTCTL_HOLDER $SLOTCTL_SLOT" >> %[1]s'
down = 'echo "down default $SLOTCTL_HOLDER $SLOTCTL_SLOT" >> %[1]s'
[pools.billing]
count = 1
up = 'echo "up billing $SLOTCTL_HOLDER $SLOTCTL_SLOT $SLOTCTL_POOL" >> %[1]s'
down = 'echo "down billing $SLOTCTL_HOLDER $SLOTCTL_SLOT $SLOTCTL_POOL" >> %[1]s'
`

// AC-2 (a): 同じ借り手が、既定poolとbillingの枠を同時に持てる。1poolでは1枠まで。
func TestSameHolderHoldsSlotsInDifferentPools(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile))
	a := holderDir(t, root, "a")

	d := acquirePool(t, e, a)
	b := acquirePool(t, e, a, "billing")
	if d.Pool != "default" || d.Slot != 1 || d.Name != "demo-1" {
		t.Fatalf("既定pool: %+v", d)
	}
	if b.Pool != "billing" || b.Slot != 1 || b.Name != "demo-billing-1" || b.Holder != a {
		t.Fatalf("billing: %+v", b)
	}
	// 同じpoolを続けて借りても延長で、枠は増えずupも走らない（冪等）。
	if again := acquirePool(t, e, a, "billing"); again.Slot != 1 {
		t.Fatalf("billingの再acquire: %+v", again)
	}
	if again := acquirePool(t, e, a); again.Slot != 1 {
		t.Fatalf("既定poolの再acquire: %+v", again)
	}
	if lines := readLines(t, logFile); len(lines) != 2 ||
		lines[0] != "up default "+a+" 1" || lines[1] != "up billing "+a+" 1 billing" {
		t.Fatalf("up: %v", lines)
	}
	slots := statusPools(t, e, a)
	if len(slots) != 3 {
		t.Fatalf("既定2枠＋billing1枠のはず: %+v", slots)
	}
	if findSlot(t, slots, "default", 1).Holder != a || findSlot(t, slots, "billing", 1).Holder != a ||
		findSlot(t, slots, "default", 2).State != "free" {
		t.Fatalf("status: %+v", slots)
	}
}

// AC-2 (b)(c): billing（数1）は、別の借り手には終了code 3。期限切れなら譲り受け、前の借り手のbillingのdownが走る。
func TestNamedPoolIsFullAndPreemptedPerPool(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile))
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")

	acquirePool(t, e, a)
	acquirePool(t, e, a, "billing")

	code, out, errs := e.run(b, "acquire", "billing", "--json")
	if code != 3 {
		t.Fatalf("billingが埋まっているときの終了code: got %d want 3（%s）", code, errs)
	}
	if !strings.Contains(errs, a) || !strings.Contains(errs, "billing") {
		t.Fatalf("使用中の借り手とpoolが出力に無い: %s", errs)
	}
	var noSlot struct {
		Error string `json:"error"`
		Pool  string `json:"pool"`
	}
	if err := json.Unmarshal([]byte(out), &noSlot); err != nil || noSlot.Error != "no free slot" || noSlot.Pool != "billing" {
		t.Fatalf("空き無しのJSON: %q %v", out, err)
	}
	// 別のpoolの空き枠には影響しない。
	if got := acquirePool(t, e, b); got.Pool != "default" || got.Slot != 2 {
		t.Fatalf("bの既定pool: %+v", got)
	}

	e.advance(11 * time.Minute) // ttl + 1分。全て期限切れ。
	got := acquirePool(t, e, b, "billing")
	if got.Slot != 1 || got.PreviousHolder != a || got.Holder != b {
		t.Fatalf("bはaのbilling slot 1 を譲り受けるはず: %+v", got)
	}
	lines := readLines(t, logFile)
	tail := lines[len(lines)-2:]
	if tail[0] != "down billing "+a+" 1 billing" || tail[1] != "up billing "+b+" 1 billing" {
		t.Fatalf("aのbillingのdown→bのupの順のはず: %v", lines)
	}
	// aの既定poolの枠は、譲られていない。
	if s := findSlot(t, statusPools(t, e, a), "default", 1); s.Holder != a {
		t.Fatalf("aの既定poolの枠が変わった: %+v", s)
	}
	var pre map[string]any
	for _, ev := range rawEvents(t, e.home) {
		if ev["event"] == "preempt" {
			pre = ev
		}
	}
	if pre == nil || pre["pool"] != "billing" || pre["previous_holder"] != a || pre["holder"] != b {
		t.Fatalf("preemptの記録: %+v", pre)
	}
}

// AC-2 (d): 未知のpool名は使い方の誤り（終了code 2）。
func TestUnknownPoolIsUsageError(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(fmt.Sprintf(twoPoolTOML, filepath.Join(t.TempDir(), "log")))
	a := holderDir(t, root, "a")
	for _, args := range [][]string{
		{"acquire", "nope"},
		{"acquire", "nope", "--json"},
		{"acquire", "--json", "nope"},
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
	// `default`は既定poolの名前として指定できる。
	if got := acquirePool(t, e, a, "default"); got.Pool != "default" || got.Slot != 1 {
		t.Fatalf("acquire default: %+v", got)
	}
}

// AC-2 (e): poolの設定の誤り。
func TestInvalidPoolConfigIsRejected(t *testing.T) {
	e := newTestEnv(t)
	for name, toml := range map[string]string{
		"[pools.default]は誤り": "project = \"demo\"\n[pools.default]\ncount = 1\n",
		"pool名が大文字":          "project = \"demo\"\n[pools.Billing]\ncount = 1\n",
		"pool名が数字で始まる":       "project = \"demo\"\n[pools.\"1x\"]\ncount = 1\n",
		"pool名に使えない文字":       "project = \"demo\"\n[pools.a_b]\ncount = 1\n",
		"pool名が空":            "project = \"demo\"\n[pools.\"\"]\ncount = 1\n",
		"poolの未知のkey":        "project = \"demo\"\n[pools.billing]\ncount = 1\nbogus = 1\n",
		"poolの枠が多すぎる":        "project = \"demo\"\n[pools.billing]\ncount = 11\n",
		"poolの枠が負":           "project = \"demo\"\n[pools.billing]\ncount = -1\n",
		"poolのport名が大文字で重なる": "project = \"demo\"\n[pools.billing]\nports = [\"a\", \"A\"]\n",
		"poolのport名に使えない文字":  "project = \"demo\"\n[pools.billing]\nports = [\"a-b\"]\n",
		"pools直下の未知のkey":     "project = \"demo\"\n[pools]\nbogus = 1\n",
	} {
		root := e.project(toml)
		for _, cmd := range []string{"acquire", "status"} {
			if code, _, errs := e.run(root, cmd); code != 1 {
				t.Errorf("%s: slotctl %s の終了code got %d want 1（%s）", name, cmd, code, errs)
			}
		}
	}
}

// AC-3: renewは借り手の全poolのleaseを延ばす。commandも記録も走らない。
func TestRenewExtendsEveryPool(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile))
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
	if s := findSlot(t, slots, "default", 1); s.ExpiresAt != want {
		t.Fatalf("既定poolの期限: %+v want %s", s, want)
	}
	if s := findSlot(t, slots, "billing", 1); s.ExpiresAt != want {
		t.Fatalf("billingの期限: %+v want %s", s, want)
	}
	// 他の借り手のleaseは延びない。
	if s := findSlot(t, slots, "default", 2); s.ExpiresAt == want || s.Holder != b {
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
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile))
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
	acquirePool(t, e, a)
	acquirePool(t, e, a, "billing")
	acquirePool(t, e, b)

	if code, _, errs := e.run(a, "release", "billing"); code != 0 {
		t.Fatalf("release billing: %d %s", code, errs)
	}
	slots := statusPools(t, e, a)
	if findSlot(t, slots, "billing", 1).State != "free" || findSlot(t, slots, "default", 1).Holder != a {
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
	if findSlot(t, slots, "billing", 1).State != "free" || findSlot(t, slots, "default", 1).State != "free" {
		t.Fatalf("全poolの枠が返るはず: %+v", slots)
	}
	if findSlot(t, slots, "default", 2).Holder != b {
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
	if !reflect.DeepEqual(rel, []string{"billing", "default", "billing"}) && !reflect.DeepEqual(rel, []string{"billing", "billing", "default"}) {
		t.Fatalf("releaseの記録のpool: %v", rel)
	}
}

// AC-3: どれかのdownが失敗しても全leaseを消し、終了code 1。
func TestReleaseDropsEveryLeaseEvenWhenADownFails(t *testing.T) {
	for name, failing := range map[string]string{"default": "default", "billing": "billing"} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			logFile := filepath.Join(t.TempDir(), "log")
			root := e.project(fmt.Sprintf(`
project = "demo"
[slots]
count = 1
[commands]
down = 'echo "down default" >> %[1]s; [ %[2]s != default ]'
[pools.billing]
down = 'echo "down billing" >> %[1]s; [ %[2]s != billing ]'
`, logFile, failing))
			a := holderDir(t, root, "a")
			acquirePool(t, e, a)
			acquirePool(t, e, a, "billing")

			code, _, errs := e.run(a, "release")
			if code != 1 {
				t.Fatalf("downが失敗したreleaseの終了code: got %d want 1（%s）", code, errs)
			}
			for _, s := range statusPools(t, e, a) {
				if s.State != "free" {
					t.Fatalf("全leaseが消えるはず: %+v", s)
				}
			}
			lines := readLines(t, logFile)
			sort.Strings(lines)
			if !reflect.DeepEqual(lines, []string{"down billing", "down default"}) {
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
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile))
	nobody := holderDir(t, root, "nobody")
	for _, args := range [][]string{{"renew"}, {"release"}, {"release", "billing"}, {"release", "default"}} {
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
[slots]
count = 2
[ports]
names = ["web", "api"]
[pools.billing]
count = 2
ports = ["x", "y"]
[pools.tunnel]
`)
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")

	d := acquirePool(t, e, a)
	if d.Ports["web"] != 12000 || d.Ports["api"] != 12001 {
		t.Fatalf("既定poolの帯は今のまま: %+v", d.Ports)
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
		t.Fatalf("既定2＋billing2＋tunnel1: %+v", slots)
	}
	if s := findSlot(t, slots, "billing", 2); s.Ports["x"] != 13100 || s.State != "lent" {
		t.Fatalf("billing slot 2: %+v", s)
	}
	if s := findSlot(t, slots, "tunnel", 1); len(s.Ports) != 0 || s.Name != "demo-tunnel-1" {
		t.Fatalf("tunnel slot 1: %+v", s)
	}
	if s := findSlot(t, slots, "default", 2); s.Ports["web"] != 12100 {
		t.Fatalf("既定 slot 2: %+v", s)
	}

	// 別のprojectの帯は、machineで空いている帯から割り当たる（既定・billingの帯と重ならない）。
	other := e.project("project = \"other\"\n[ports]\nnames = [\"web\"]\n[pools.q]\nports = [\"z\"]\n")
	o := acquirePool(t, e, other)
	oq := acquirePool(t, e, other, "q")
	if o.Ports["web"] != 14000 || oq.Ports["z"] != 15000 {
		t.Fatalf("空いている帯から: %+v %+v", o.Ports, oq.Ports)
	}
	// 既定pool・billingの帯は変わらない。
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
[ports]
names = ["web"]
[commands]
up = 'echo "$SLOTCTL_POOL $SLOTCTL_NAME ${SLOTCTL_PORT_WEB-unset} ${SLOTCTL_PORT_X-unset}" >> %[1]s'
[pools.billing]
ports = ["x"]
up = 'echo "$SLOTCTL_POOL $SLOTCTL_NAME ${SLOTCTL_PORT_WEB-unset} ${SLOTCTL_PORT_X-unset}" >> %[1]s'
`, logFile))
	acquirePool(t, e, holderDir(t, root, "a"))
	acquirePool(t, e, holderDir(t, root, "a"), "billing")
	got := readLines(t, logFile)
	want := []string{"default demo-1 12000 unset", "billing demo-billing-1 unset 13000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env:\n got %q\nwant %q", got, want)
	}
}

// AC-4: statusの表にPOOL列があり、全poolの全枠が並ぶ。記録の各行にpoolがある。
func TestStatusTableAndEventsCarryPool(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(`
project = "demo"
[slots]
count = 1
[commands]
up = "true"
down = "true"
[pools.billing]
up = "true"
down = "true"
`)
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
	if f := strings.Fields(lines[1]); f[0] != "default" || f[2] != "demo-1" {
		t.Fatalf("既定poolの行: %q", lines[1])
	}
	if f := strings.Fields(lines[2]); f[0] != "billing" || f[2] != "demo-billing-1" {
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
	if !pools["default"] || !pools["billing"] {
		t.Fatalf("記録のpool: %v", pools)
	}
	// 空きが無くて借りられなかった記録にもpoolがある。
	last := evs[len(evs)-1]
	if last["event"] != "acquire" || last["ok"] != false || last["pool"] != "billing" {
		t.Fatalf("最後の記録: %+v", last)
	}
}

// AC-4: machineの設定でpoolごとの数を上書きできる。
func TestMachineConfigOverridesPoolSlots(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.home, "config.toml"), `
[projects.demo]
slots = 1
[projects.demo.pools.billing]
slots = 3
[projects.other.pools.billing]
slots = 9
`)
	root := e.project("project = \"demo\"\n[slots]\ncount = 2\n[pools.billing]\ncount = 1\n[pools.tunnel]\ncount = 2\n")
	slots := statusPools(t, e, root)
	count := map[string]int{}
	for _, s := range slots {
		count[s.Pool]++
	}
	if count["default"] != 1 || count["billing"] != 3 || count["tunnel"] != 2 {
		t.Fatalf("machine設定で上書きされた数: %v", count)
	}
	// 未知のkeyは、既存のmachine設定と同じくエラー。
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.demo.pools.billing]\nbogus = 1\n")
	if code, _, _ := e.run(root, "status"); code != 1 {
		t.Fatalf("machine設定のpoolの未知のkeyはエラーのはず: %d", code)
	}
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.demo.pools.default]\nslots = 2\n")
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

// AC-5: v0.1.0のstate.dbを開くと、既存のleaseとport帯が既定poolのものとして引き継がれる。
func TestV1StateIsMigratedToDefaultPool(t *testing.T) {
	for _, first := range []string{"renew", "status", "release"} {
		t.Run("最初の命令が"+first, func(t *testing.T) {
			e := newTestEnv(t)
			root := e.project(`
project = "demo"
[lease]
ttl = "10m"
[slots]
count = 3
[ports]
names = ["web"]
[pools.billing]
count = 1
`)
			a, b, c := holderDir(t, root, "a"), holderDir(t, root, "b"), holderDir(t, root, "c")
			realRoot, _ := filepath.EvalSymlinks(root)
			now := e.now.Unix()
			makeV1State(t, e.home, realRoot, 14000, [][]any{
				{1, a, now - 120, now - 60, now + 540},
				{2, b, now - 900, now - 800, now - 200}, // 期限切れ
			})

			args := []string{first}
			cwd := a
			if first == "status" {
				args = []string{"status", "--json"}
			}
			if code, _, errs := e.run(cwd, args...); code != 0 {
				t.Fatalf("slotctl %v: %d %s", args, code, errs)
			}
			if v := userVersion(t, e.home); v != 2 {
				t.Fatalf("user_version: %d", v)
			}

			if first == "renew" {
				want := e.now.Add(10 * time.Minute).Format(time.RFC3339)
				if s := findSlot(t, statusPools(t, e, a), "default", 1); s.ExpiresAt != want {
					t.Fatalf("移行後のrenew: %+v want %s", s, want)
				}
			}
			slots := statusPools(t, e, a)
			s1, s2, s3 := findSlot(t, slots, "default", 1), findSlot(t, slots, "default", 2), findSlot(t, slots, "default", 3)
			wantS1 := "lent"
			if first == "release" {
				wantS1 = "free" // 移行後のreleaseで返る
			}
			if s1.State != wantS1 || s2.Holder != b || s2.State != "expired" || s3.State != "free" ||
				(wantS1 == "lent" && s1.Holder != a) {
				t.Fatalf("貸し出し中の枠が引き継がれるはず: %+v", slots)
			}
			if s1.Ports["web"] != 14000 || s2.Ports["web"] != 14100 || s3.Ports["web"] != 14200 {
				t.Fatalf("既定poolのport帯が引き継がれるはず: %+v", slots)
			}
			if findSlot(t, slots, "billing", 1).State != "free" {
				t.Fatalf("billingは空きのはず: %+v", slots)
			}

			if first != "release" {
				if code, _, errs := e.run(a, "release"); code != 0 {
					t.Fatalf("release: %d %s", code, errs)
				}
				slots = statusPools(t, e, a)
				if findSlot(t, slots, "default", 1).State != "free" || findSlot(t, slots, "default", 2).Holder != b {
					t.Fatalf("aだけが返るはず: %+v", slots)
				}
			}
			// 期限切れの枠でも、空き枠（slot 1・3）が先に使われ、bの枠は譲られない。
			if got := acquirePool(t, e, c); got.Slot != 1 || got.Ports["web"] != 14000 {
				t.Fatalf("cの枠: %+v", got)
			}
			// 移行しても、名前つきpoolの枠は借りられる。
			if got := acquirePool(t, e, c, "billing"); got.Name != "demo-billing-1" {
				t.Fatalf("billing: %+v", got)
			}
		})
	}
}

func TestStateNewerThanThisVersionIsRejected(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project = \"demo\"\n")
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

// 設定から消えたpoolの枠は、downを走らせずに返す（leaseが残り続けない）。
func TestReleaseDropsLeaseOfPoolRemovedFromConfig(t *testing.T) {
	e := newTestEnv(t)
	logFile := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf(twoPoolTOML, logFile))
	a := holderDir(t, root, "a")
	acquirePool(t, e, a, "billing")
	writeFile(t, filepath.Join(root, "slotctl.toml"), "project = \"demo\"\n")

	code, _, errs := e.run(a, "release")
	if code != 0 || !strings.Contains(errs, "billing") {
		t.Fatalf("release: code=%d %s", code, errs)
	}
	for _, l := range readLines(t, logFile) {
		if strings.HasPrefix(l, "down billing") {
			t.Fatalf("設定に無いpoolのdownは走らないはず: %v", l)
		}
	}
	writeFile(t, filepath.Join(root, "slotctl.toml"), fmt.Sprintf(twoPoolTOML, logFile))
	if s := findSlot(t, statusPools(t, e, a), "billing", 1); s.State != "free" {
		t.Fatalf("leaseは消えるはず: %+v", s)
	}
}
