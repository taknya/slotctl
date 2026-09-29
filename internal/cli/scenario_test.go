package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// AC-5: 設定の解決とportの割り当てが仕様どおり。
func TestConfigFoundFromSubdirectory(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project = \"demo\"\n[slots]\ncount = 2\n")
	deep := holderDir(t, root, "x/y/z")
	res := mustAcquire(t, e, deep)
	if res.Project != "demo" || res.Slot != 1 || res.Holder != deep {
		t.Fatalf("下位directoryから上のslotctl.tomlを見つけるはず: %+v", res)
	}
}

func TestMachineConfigOverridesSlots(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.demo]\nslots = 1\n")
	root := e.project("project = \"demo\"\n[slots]\ncount = 3\n")
	mustAcquire(t, e, holderDir(t, root, "a"))
	if code, _, _ := e.run(holderDir(t, root, "b"), "acquire"); code != 3 {
		t.Fatalf("machine設定のslots=1で2人目は空き無しのはず: %d", code)
	}
	if st := mustStatus(t, e, root); len(st.Slots) != 1 {
		t.Fatalf("statusの枠の数: %d", len(st.Slots))
	}
}

func TestPortBandsDoNotOverlap(t *testing.T) {
	e := newTestEnv(t)
	toml := func(name string) string {
		return "project = \"" + name + "\"\n[slots]\ncount = 10\n[ports]\nnames = [\"web\", \"db\"]\n"
	}
	one := e.project(toml("one"))
	two := e.project(toml("two"))
	minMax := func(root string, holders ...string) (lo, hi int) {
		lo = 1 << 30
		for _, h := range holders {
			res := mustAcquire(t, e, holderDir(t, root, h))
			for _, p := range res.Ports {
				lo, hi = min(lo, p), max(hi, p)
			}
		}
		return
	}
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	lo1, hi1 := minMax(one, names...)
	lo2, hi2 := minMax(two, names...)
	if lo1 != 12000 || lo2 != 13000 {
		t.Fatalf("帯は port_start から1000ずつ: one=%d two=%d", lo1, lo2)
	}
	if hi1 >= lo2 || hi2 < lo2 {
		t.Fatalf("帯が重なっています: one=%d-%d two=%d-%d", lo1, hi1, lo2, hi2)
	}
	// 同じprojectの帯は変わらない。
	if res := mustAcquire(t, e, holderDir(t, one, "a")); res.Ports["web"] != 12000 {
		t.Fatalf("帯は記録されて変わらないはず: %+v", res)
	}
}

func TestPortStartFromMachineConfig(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.home, "config.toml"), "port_start = 20000\n")
	root := e.project("project = \"demo\"\n[ports]\nnames = [\"web\"]\n")
	if res := mustAcquire(t, e, root); res.Ports["web"] != 20000 {
		t.Fatalf("port_start: %+v", res.Ports)
	}
}

func TestSameProjectNameFromAnotherRepositoryIsRejected(t *testing.T) {
	e := newTestEnv(t)
	first := e.project("project = \"demo\"\n")
	other := e.project("project = \"demo\"\n")
	mustAcquire(t, e, first)
	code, _, errs := e.run(other, "acquire")
	if code != 1 || !strings.Contains(errs, "別のrepository") {
		t.Fatalf("別repositoryの同名projectは拒むはず: code=%d %s", code, errs)
	}
	if code, _, _ := e.run(other, "status"); code != 1 {
		t.Fatalf("statusも拒むはず: %d", code)
	}
	// もとのrepositoryは影響を受けない。
	mustAcquire(t, e, first)
}

func TestInvalidConfigIsRejected(t *testing.T) {
	e := newTestEnv(t)
	for name, toml := range map[string]string{
		"未知のkey":        "project = \"demo\"\nbogus = 1\n",
		"projectが無い":    "[slots]\ncount = 1\n",
		"ttlが不正":        "project = \"demo\"\n[lease]\nttl = \"abc\"\n",
		"枠が多すぎる":        "project = \"demo\"\n[slots]\ncount = 11\n",
		"port名が大文字で重なる": "project = \"demo\"\n[ports]\nnames = [\"web\", \"WEB\"]\n",
	} {
		root := e.project(toml)
		if code, _, _ := e.run(root, "acquire"); code != 1 {
			t.Errorf("%s: 終了code got %d want 1", name, code)
		}
	}
}

// gitのworktreeでは、holderはworktreeごとに変わり、repositoryは共通になる。
func TestGitWorktreesAreDistinctHoldersOfOneRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git がありません")
	}
	e := newTestEnv(t)
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q")
	writeFile(t, filepath.Join(repo, "slotctl.toml"), "project = \"demo\"\n[slots]\ncount = 3\n")
	git(repo, "add", "slotctl.toml")
	git(repo, "commit", "-q", "-m", "init")
	wt := filepath.Join(base, "wt")
	git(repo, "worktree", "add", "-q", wt)

	realRepo, _ := filepath.EvalSymlinks(repo)
	realWt, _ := filepath.EvalSymlinks(wt)
	sub := holderDir(t, repo, "sub/dir")

	fromSub := mustAcquire(t, e, sub)
	if fromSub.Holder != realRepo {
		t.Fatalf("holderはgitのtoplevel: got %s want %s", fromSub.Holder, realRepo)
	}
	fromWt := mustAcquire(t, e, wt)
	if fromWt.Holder != realWt || fromWt.Slot != 2 {
		t.Fatalf("別のworktreeは別のholderで、同じprojectの別の枠: %+v", fromWt)
	}
}
