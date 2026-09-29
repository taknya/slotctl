// Package config は、projectの設定（slotctl.toml）とmachineの設定を解決する。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/taknya/slotctl/internal/ports"
)

const (
	// ProjectFileName は、projectの設定file名。
	ProjectFileName = "slotctl.toml"
	// MachineFileName は、machineの設定file名。
	MachineFileName = "config.toml"

	defaultTTL       = 10 * time.Minute
	defaultPortStart = 12000
	defaultRetention = 3
	defaultSlotCount = 1
)

var (
	projectNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	portNamePattern    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
)

// ErrNoProjectFile は、cwdから上へ辿ってもslotctl.tomlが無いことを表す。
var ErrNoProjectFile = errors.New("slotctl.toml が見つかりません（cwdから上へ辿りました）")

type projectFile struct {
	Project string `toml:"project"`
	Lease   struct {
		TTL string `toml:"ttl"`
	} `toml:"lease"`
	Slots struct {
		Count int `toml:"count"`
	} `toml:"slots"`
	Ports struct {
		Names []string `toml:"names"`
	} `toml:"ports"`
	Commands struct {
		Up   string `toml:"up"`
		Down string `toml:"down"`
	} `toml:"commands"`
}

type machineProject struct {
	Slots int               `toml:"slots"`
	Env   map[string]string `toml:"env"`
}

type machineFile struct {
	PortStart          int                       `toml:"port_start"`
	LogRetentionMonths int                       `toml:"log_retention_months"`
	Projects           map[string]machineProject `toml:"projects"`
}

// Config は、projectの設定とmachineの設定を合わせた、実行時の設定である。
type Config struct {
	Project            string
	Root               string // slotctl.tomlのあるdirectory
	TTL                time.Duration
	Count              int
	PortNames          []string
	Up                 string
	Down               string
	Env                map[string]string // machine設定のenv
	PortStart          int
	LogRetentionMonths int
	StateDir           string // state.dbと記録の置き場
}

// SlotName は、枠の名前（<project>-<slot>）を返す。
func (c *Config) SlotName(slot int) string { return fmt.Sprintf("%s-%d", c.Project, slot) }

// Dirs は、状態と設定の置き場を返す。SLOTCTL_HOMEがあれば、どちらもそのdirectoryにする。
func Dirs(getenv func(string) string) (stateDir, configDir string, err error) {
	if h := getenv("SLOTCTL_HOME"); h != "" {
		return h, h, nil
	}
	home := getenv("HOME")
	pick := func(xdg, fallback string) (string, error) {
		if v := getenv(xdg); v != "" {
			return filepath.Join(v, "slotctl"), nil
		}
		if home == "" {
			return "", fmt.Errorf("HOME も %s も未設定で、置き場を決められません", xdg)
		}
		return filepath.Join(home, fallback, "slotctl"), nil
	}
	if stateDir, err = pick("XDG_STATE_HOME", filepath.Join(".local", "state")); err != nil {
		return "", "", err
	}
	if configDir, err = pick("XDG_CONFIG_HOME", ".config"); err != nil {
		return "", "", err
	}
	return stateDir, configDir, nil
}

// FindProjectFile は、cwdから上へ辿って最初のslotctl.tomlのpathを返す。
func FindProjectFile(cwd string) (string, error) {
	dir := cwd
	for {
		p := filepath.Join(dir, ProjectFileName)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNoProjectFile
		}
		dir = parent
	}
}

// Load は、cwdから設定を解決する。
func Load(getenv func(string) string, cwd string) (*Config, error) {
	stateDir, configDir, err := Dirs(getenv)
	if err != nil {
		return nil, err
	}
	path, err := FindProjectFile(cwd)
	if err != nil {
		return nil, err
	}
	var pf projectFile
	md, err := toml.DecodeFile(path, &pf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return nil, fmt.Errorf("%s: 未知の設定 %v", path, u)
	}
	if !projectNamePattern.MatchString(pf.Project) {
		return nil, fmt.Errorf("%s: project は英数字・.・_・- で書いてください（%q）", path, pf.Project)
	}

	var mf machineFile
	mpath := filepath.Join(configDir, MachineFileName)
	if _, err := os.Stat(mpath); err == nil {
		md, err := toml.DecodeFile(mpath, &mf)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", mpath, err)
		}
		if u := md.Undecoded(); len(u) > 0 {
			return nil, fmt.Errorf("%s: 未知の設定 %v", mpath, u)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	cfg := &Config{
		Project:            pf.Project,
		Root:               filepath.Dir(path),
		TTL:                defaultTTL,
		Count:              pf.Slots.Count,
		PortNames:          pf.Ports.Names,
		Up:                 pf.Commands.Up,
		Down:               pf.Commands.Down,
		PortStart:          mf.PortStart,
		LogRetentionMonths: mf.LogRetentionMonths,
		StateDir:           stateDir,
	}
	if pf.Lease.TTL != "" {
		if cfg.TTL, err = time.ParseDuration(pf.Lease.TTL); err != nil {
			return nil, fmt.Errorf("%s: lease.ttl: %w", path, err)
		}
	}
	if cfg.TTL < time.Second {
		return nil, fmt.Errorf("%s: lease.ttl は1秒以上にしてください", path)
	}
	if cfg.Count == 0 {
		cfg.Count = defaultSlotCount
	}
	if mp, ok := mf.Projects[cfg.Project]; ok {
		if mp.Slots != 0 {
			cfg.Count = mp.Slots
		}
		cfg.Env = make(map[string]string, len(mp.Env))
		home := getenv("HOME")
		for k, v := range mp.Env {
			cfg.Env[k] = expandHome(v, home)
		}
	}
	if cfg.Count < 1 || cfg.Count > ports.MaxSlots {
		return nil, fmt.Errorf("枠の数は1以上%d以下にしてください（%d）", ports.MaxSlots, cfg.Count)
	}
	if len(cfg.PortNames) > ports.PerSlot {
		return nil, fmt.Errorf("ports.names は%d個以下にしてください", ports.PerSlot)
	}
	seen := map[string]bool{}
	for _, n := range cfg.PortNames {
		if !portNamePattern.MatchString(n) {
			return nil, fmt.Errorf("ports.names に使えない名前があります: %q", n)
		}
		u := strings.ToUpper(n)
		if seen[u] {
			return nil, fmt.Errorf("ports.names が大文字にすると重なります: %q", n)
		}
		seen[u] = true
	}
	if cfg.PortStart == 0 {
		cfg.PortStart = defaultPortStart
	}
	if cfg.PortStart < 1024 || cfg.PortStart+ports.BandSize-1 > ports.Max {
		return nil, fmt.Errorf("port_start が範囲外です: %d", cfg.PortStart)
	}
	if cfg.LogRetentionMonths == 0 {
		cfg.LogRetentionMonths = defaultRetention
	}
	if cfg.LogRetentionMonths < 1 {
		return nil, fmt.Errorf("log_retention_months は1以上にしてください")
	}
	return cfg, nil
}

func expandHome(v, home string) string {
	if home != "" && (v == "~" || strings.HasPrefix(v, "~/")) {
		return home + v[1:]
	}
	return v
}
