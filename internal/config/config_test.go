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
[slots]
count = 2
[ports]
names = ["web", "db"]
[commands]
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
	if cfg.Project != "demo" || cfg.Root != root || cfg.TTL != 5*time.Minute || cfg.Count != 2 ||
		cfg.Up != "u" || cfg.Down != "d" || len(cfg.PortNames) != 2 || cfg.PortStart != 12000 ||
		cfg.LogRetentionMonths != 3 || cfg.StateDir != home {
		t.Fatalf("設定: %+v", cfg)
	}
	if cfg.SlotName(2) != "demo-2" {
		t.Fatalf("SlotName: %s", cfg.SlotName(2))
	}
}

// AC-5: machineの設定でslotsを上書きできる。
func TestMachineConfigOverrides(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	write(t, filepath.Join(root, "slotctl.toml"), "project = \"demo\"\n[slots]\ncount = 3\n")
	write(t, filepath.Join(home, "config.toml"), `
port_start = 20000
log_retention_months = 6
[projects.demo]
slots = 1
env = { HOME_DIR = "~/x", PLAIN = "v" }
`)
	cfg, err := Load(env(map[string]string{"SLOTCTL_HOME": home, "HOME": "/home/u"}), root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Count != 1 || cfg.PortStart != 20000 || cfg.LogRetentionMonths != 6 ||
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
		"projectが無い":      "[slots]\ncount = 1\n",
		"ttlが不正":          "project = \"demo\"\n[lease]\nttl = \"abc\"\n",
		"枠が多すぎる":          "project = \"demo\"\n[slots]\ncount = 11\n",
		"port名が大文字で重なる":   "project = \"demo\"\n[ports]\nnames = [\"web\", \"WEB\"]\n",
		"port名に使えない文字":    "project = \"demo\"\n[ports]\nnames = [\"a-b\"]\n",
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
