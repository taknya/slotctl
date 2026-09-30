package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// AC-5: 下位directoryから上のslotctl.tomlを見つける。
func TestLoadFindsProjectFileFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	write(t, filepath.Join(root, "slotctl.toml"), `
project = "demo"
[lease]
ttl = "5m"
[pools.dev]
count = 2
ports = ["web", "db"]
up = "u"
down = "d"
`)
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env(map[string]string{"SLOTCTL_HOME": home}), deep)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project != "demo" || cfg.Root != root || cfg.TTL != 5*time.Minute || cfg.Named[0].Count != 2 ||
		cfg.Named[0].Up != "u" || cfg.Named[0].Down != "d" || len(cfg.Named[0].PortNames) != 2 || cfg.PortStart != 12000 ||
		cfg.LogRetentionMonths != 3 || cfg.StateDir != home {
		t.Fatalf("設定: %+v", cfg)
	}
	if cfg.SlotNameIn("dev", 2) != "demo-dev-2" {
		t.Fatalf("SlotName: %s", cfg.SlotNameIn("dev", 2))
	}
}

// AC-5: machineの設定でslotsを上書きできる。
func TestMachineConfigOverrides(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	write(t, filepath.Join(root, "slotctl.toml"), "project = \"demo\"\n[pools.dev]\ncount = 3\n")
	write(t, filepath.Join(home, "config.toml"), `
port_start = 20000
log_retention_months = 6
[projects.demo.pools.dev]
slots = 1
[projects.demo]
env = { HOME_DIR = "~/x", PLAIN = "v" }
`)
	cfg, err := Load(env(map[string]string{"SLOTCTL_HOME": home, "HOME": "/home/u"}), root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Named[0].Count != 1 || cfg.PortStart != 20000 || cfg.LogRetentionMonths != 6 ||
		cfg.Env["HOME_DIR"] != "/home/u/x" || cfg.Env["PLAIN"] != "v" {
		t.Fatalf("machine設定: %+v", cfg)
	}
}

func TestDirsFollowXDGAndSlotctlHome(t *testing.T) {
	state, cfgDir, err := Dirs(env(map[string]string{"HOME": "/h"}))
	if err != nil || state != "/h/.local/state/slotctl" || cfgDir != "/h/.config/slotctl" {
		t.Fatalf("既定: %s %s %v", state, cfgDir, err)
	}
	state, cfgDir, _ = Dirs(env(map[string]string{"HOME": "/h", "XDG_STATE_HOME": "/s", "XDG_CONFIG_HOME": "/c"}))
	if state != "/s/slotctl" || cfgDir != "/c/slotctl" {
		t.Fatalf("XDG: %s %s", state, cfgDir)
	}
	state, cfgDir, _ = Dirs(env(map[string]string{"HOME": "/h", "XDG_STATE_HOME": "/s", "SLOTCTL_HOME": "/t"}))
	if state != "/t" || cfgDir != "/t" {
		t.Fatalf("SLOTCTL_HOME: %s %s", state, cfgDir)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	home := t.TempDir()
	for name, toml := range map[string]string{
		"未知のkey":          "project = \"demo\"\nbogus = 1\n",
		"projectが無い":      "[pools.dev]\ncount = 1\n",
		"ttlが不正":          "project = \"demo\"\n[lease]\nttl = \"abc\"\n",
		"枠が多すぎる":          "project = \"demo\"\n[pools.dev]\ncount = 11\n",
		"port名が大文字で重なる":   "project = \"demo\"\n[pools.dev]\nports = [\"web\", \"WEB\"]\n",
		"port名に使えない文字":    "project = \"demo\"\n[pools.dev]\nports = [\"a-b\"]\n",
		"project名に使えない文字": "project = \"a b\"\n",
	} {
		root := t.TempDir()
		write(t, filepath.Join(root, "slotctl.toml"), toml)
		if _, err := Load(env(map[string]string{"SLOTCTL_HOME": home}), root); err == nil {
			t.Errorf("%s: エラーになるはず", name)
		}
	}
}

func TestLoadWithoutProjectFile(t *testing.T) {
	_, err := Load(env(map[string]string{"SLOTCTL_HOME": t.TempDir()}), t.TempDir())
	if err != ErrNoProjectFile {
		t.Fatalf("got %v", err)
	}
}

func TestLoadPools(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	write(t, filepath.Join(root, "slotctl.toml"), `
project = "demo"
[pools.dev]
count = 2
ports = ["web"]
up = "u"
[pools.tunnel]
[pools.billing]
count = 3
up = "bu"
down = "bd"
ports = ["x", "y"]
`)
	write(t, filepath.Join(home, "config.toml"), `
[projects.demo.pools.billing]
slots = 4
[projects.demo.pools.gone]
slots = 2
`)
	cfg, err := Load(env(map[string]string{"SLOTCTL_HOME": home}), root)
	if err != nil {
		t.Fatal(err)
	}
	pools := cfg.Pools()
	if len(pools) != 3 || pools[0].Name != "billing" || pools[1].Name != "dev" || pools[2].Name != "tunnel" {
		t.Fatalf("poolの順番は名前順のはず: %+v", pools)
	}
	if d := pools[1]; d.Count != 2 || d.Up != "u" || d.Down != "" || len(d.PortNames) != 1 {
		t.Fatalf("dev pool: %+v", d)
	}
	b, ok := cfg.Pool("billing")
	if !ok || b.Count != 4 || b.Up != "bu" || b.Down != "bd" || len(b.PortNames) != 2 {
		t.Fatalf("billing（machine設定でcountを上書き）: %+v", b)
	}
	if tu, _ := cfg.Pool("tunnel"); tu.Count != 1 || tu.Up != "" || tu.Down != "" || len(tu.PortNames) != 0 {
		t.Fatalf("tunnel（他poolのcommandを引き継がない）: %+v", tu)
	}
	if _, ok := cfg.Pool("nope"); ok {
		t.Fatal("無いpoolは見つからないはず")
	}
	if cfg.SlotNameIn("dev", 2) != "demo-dev-2" || cfg.SlotNameIn("billing", 2) != "demo-billing-2" || cfg.SlotNameIn("dev", 1) != "demo-dev-1" {
		t.Fatal("枠の名前")
	}
}

func TestLoadRejectsInvalidPools(t *testing.T) {
	home := t.TempDir()
	for name, toml := range map[string]string{
		"旧slots":         "project = \"demo\"\n[slots]\n",
		"pool名が大文字":      "project = \"demo\"\n[pools.Billing]\n",
		"pool名が数字始まり":    "project = \"demo\"\n[pools.\"1a\"]\n",
		"pool名に_":        "project = \"demo\"\n[pools.a_b]\n",
		"poolの未知のkey":    "project = \"demo\"\n[pools.a]\nbogus = 1\n",
		"poolの数が多すぎる":    "project = \"demo\"\n[pools.a]\ncount = 11\n",
		"poolのport名が重なる": "project = \"demo\"\n[pools.a]\nports = [\"x\", \"X\"]\n",
	} {
		root := t.TempDir()
		write(t, filepath.Join(root, "slotctl.toml"), toml)
		if _, err := Load(env(map[string]string{"SLOTCTL_HOME": home}), root); err == nil {
			t.Errorf("%s: エラーになるはず", name)
		}
	}
	// pool名に-と数字は使える。
	root := t.TempDir()
	write(t, filepath.Join(root, "slotctl.toml"), "project = \"demo\"\n[pools.e2e-2]\n")
	if _, err := Load(env(map[string]string{"SLOTCTL_HOME": home}), root); err != nil {
		t.Errorf("e2e-2: %v", err)
	}
}

func TestReclaimAfterDefaultAndConfiguredValue(t *testing.T) {
	for _, v := range []string{"", "45m", "0s", "bogus"} {
		t.Run(v, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			toml := "project='demo'\n[lease]\n"
			if v != "" {
				toml += "reclaim_after='" + v + "'\n"
			}
			toml += "[pools.dev]\n"
			write(t, filepath.Join(root, "slotctl.toml"), toml)
			cfg, err := Load(env(map[string]string{"SLOTCTL_HOME": home}), root)
			if v == "0s" || v == "bogus" {
				if err == nil {
					t.Fatal("invalid reclaim_after accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 30 * time.Minute
			if v == "45m" {
				want = 45 * time.Minute
			}
			if cfg.ReclaimAfter != want {
				t.Fatalf("reclaim_after=%v", cfg.ReclaimAfter)
			}
		})
	}
}
