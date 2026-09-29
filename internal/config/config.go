// Package config は、projectの設定（slotctl.toml）とmachineの設定を解決する。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

	// DefaultPool は、既定のpool（[slots]・[ports]・[commands]が定める枠）の名前。名前つきpoolには使えない。
	DefaultPool = "default"
)

var (
	projectNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	portNamePattern    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	poolNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
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
	Pools map[string]poolFile `toml:"pools"`
}

// poolFile は、名前つきpool（[pools.<名前>]）の設定である。
type poolFile struct {
	Count int      `toml:"count"`
	Up    string   `toml:"up"`
	Down  string   `toml:"down"`
	Ports []string `toml:"ports"`
}

type machinePool struct {
	Slots int `toml:"slots"`
}

type machineProject struct {
	Slots int                    `toml:"slots"`
	Env   map[string]string      `toml:"env"`
	Pools map[string]machinePool `toml:"pools"`
}

type machineFile struct {
	PortStart          int                       `toml:"port_start"`
	LogRetentionMonths int                       `toml:"log_retention_months"`
	Projects           map[string]machineProject `toml:"projects"`
}

// Pool は、枠の種類（pool）の設定である。
type Pool struct {
	Name      string
	Count     int
	PortNames []string
	Up        string
	Down      string
}

// Config は、projectの設定とmachineの設定を合わせた、実行時の設定である。
// Count・PortNames・Up・Downは、既定pool（[slots]・[ports]・[commands]）のもの。名前つきpoolはNamedにある。
type Config struct {
	Project            string
	Root               string // slotctl.tomlのあるdirectory
	TTL                time.Duration
	Count              int
	PortNames          []string
	Up                 string
	Down               string
	Named              []Pool            // 名前つきpool（名前順）
	Env                map[string]string // machine設定のenv
	PortStart          int
	LogRetentionMonths int
	StateDir           string // state.dbと記録の置き場
}

// SlotName は、既定poolの枠の名前（<project>-<slot>）を返す。
func (c *Config) SlotName(slot int) string { return c.SlotNameIn(DefaultPool, slot) }

// SlotNameIn は、poolの枠の名前を返す。既定poolは<project>-<slot>、名前つきpoolは<project>-<pool>-<slot>。
func (c *Config) SlotNameIn(pool string, slot int) string {
	if pool == DefaultPool {
		return fmt.Sprintf("%s-%d", c.Project, slot)
	}
	return fmt.Sprintf("%s-%s-%d", c.Project, pool, slot)
}

// DefaultPool は、既定poolを返す。
func (c *Config) DefaultPool() Pool {
	return Pool{Name: DefaultPool, Count: c.Count, PortNames: c.PortNames, Up: c.Up, Down: c.Down}
}

// Pools は、全pool（既定pool、続いて名前つきpoolの名前順）を返す。
func (c *Config) Pools() []Pool {
	return append([]Pool{c.DefaultPool()}, c.Named...)
}

// Pool は、名前のpoolを返す。無ければfoundがfalse。
func (c *Config) Pool(name string) (p Pool, found bool) {
	for _, p := range c.Pools() {
		if p.Name == name {
			return p, true
		}
	}
	return Pool{}, false
}

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
	names := make([]string, 0, len(pf.Pools))
	for name := range pf.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == DefaultPool {
			return nil, fmt.Errorf("%s: pool の名前 %q は既定のpoolの名前です。既定のpoolは [slots]・[ports]・[commands] で書いてください", path, DefaultPool)
		}
		if !poolNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%s: pool の名前は小文字・数字・- で、小文字から始めてください（%q）", path, name)
		}
		p := pf.Pools[name]
		cfg.Named = append(cfg.Named, Pool{Name: name, Count: p.Count, PortNames: p.Ports, Up: p.Up, Down: p.Down})
	}
	for i := range cfg.Named {
		if cfg.Named[i].Count == 0 {
			cfg.Named[i].Count = defaultSlotCount
		}
	}
	if mp, ok := mf.Projects[cfg.Project]; ok {
		if mp.Slots != 0 {
			cfg.Count = mp.Slots
		}
		if _, ok := mp.Pools[DefaultPool]; ok {
			return nil, fmt.Errorf("%s: [projects.%s.pools.%s] は書けません。既定のpoolの数は [projects.%s] の slots です", mpath, cfg.Project, DefaultPool, cfg.Project)
		}
		// slotctl.tomlに無いpoolの上書きは無視する（projectの設定が版ごとに違っても、命令を止めない）。
		for i := range cfg.Named {
			if mpool, ok := mp.Pools[cfg.Named[i].Name]; ok && mpool.Slots != 0 {
				cfg.Named[i].Count = mpool.Slots
			}
		}
		cfg.Env = make(map[string]string, len(mp.Env))
		home := getenv("HOME")
		for k, v := range mp.Env {
			cfg.Env[k] = expandHome(v, home)
		}
	}
	for _, p := range cfg.Pools() {
		if err := validatePool(p); err != nil {
			return nil, err
		}
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

// validatePool は、poolの枠の数とportの名前を確かめる。
func validatePool(p Pool) error {
	label := ""
	portsKey := "ports.names"
	if p.Name != DefaultPool {
		label = fmt.Sprintf("pool %q の", p.Name)
		portsKey = fmt.Sprintf("pools.%s.ports", p.Name)
	}
	if p.Count < 1 || p.Count > ports.MaxSlots {
		return fmt.Errorf("%s枠の数は1以上%d以下にしてください（%d）", label, ports.MaxSlots, p.Count)
	}
	if len(p.PortNames) > ports.PerSlot {
		return fmt.Errorf("%s は%d個以下にしてください", portsKey, ports.PerSlot)
	}
	seen := map[string]bool{}
	for _, n := range p.PortNames {
		if !portNamePattern.MatchString(n) {
			return fmt.Errorf("%s に使えない名前があります: %q", portsKey, n)
		}
		u := strings.ToUpper(n)
		if seen[u] {
			return fmt.Errorf("%s が大文字にすると重なります: %q", portsKey, n)
		}
		seen[u] = true
	}
	return nil
}

func expandHome(v, home string) string {
	if home != "" && (v == "~" || strings.HasPrefix(v, "~/")) {
		return home + v[1:]
	}
	return v
}
