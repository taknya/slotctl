// Package cli は、slotctlの命令の解釈・出力・終了codeを扱う。
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/taknya/slotctl/internal/command"
	"github.com/taknya/slotctl/internal/config"
	"github.com/taknya/slotctl/internal/eventlog"
	"github.com/taknya/slotctl/internal/holder"
	"github.com/taknya/slotctl/internal/lease"
	"github.com/taknya/slotctl/internal/ports"
	"github.com/taknya/slotctl/internal/store"
)

// 終了code。
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
	ExitNoSlot  = 3
)

// App は、CLIの実行環境である。時刻・環境変数・cwd・出力先を差し替えられる。
type App struct {
	Now    func() time.Time
	Getenv func(string) string
	Cwd    string
	Stdout io.Writer
	Stderr io.Writer
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

const usageText = `使い方: slotctl <命令> [option]

命令:
  acquire <pool> [--json]  poolの枠を借りる（同じholderなら期限を延ばして同じ枠を返す）
  renew                    holderの全poolの枠の期限を延ばす（commandは走らせない）
  release [pool]           holderのpoolの枠のdownを走らせて返す（poolを省くと全poolの枠）
  reclaim                  projectの全poolでハートビートが途絶えた枠を空ける
  status [--json]          projectの全poolの全枠の状態を出す

終了code: 0=成功 1=その他の失敗 2=使い方の誤り 3=空きが無い
`

// Run は、命令を実行して終了codeを返す。
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stderr, usageText)
		return ExitUsage
	}
	name, rest := args[0], args[1:]
	var err error
	switch name {
	case "acquire":
		err = a.acquire(rest)
	case "renew":
		err = a.renew(rest)
	case "release":
		err = a.release(rest)
	case "reclaim":
		err = a.reclaim(rest)
	case "status":
		err = a.status(rest)
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usageText)
		return ExitOK
	default:
		fmt.Fprintf(a.Stderr, "slotctl: 未知の命令 %q\n\n%s", name, usageText)
		return ExitUsage
	}
	return a.exitCode(err)
}

func (a *App) exitCode(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(a.Stderr, "slotctl:", ue.msg)
		return ExitUsage
	}
	var ne *lease.NoSlotError
	if errors.As(err, &ne) {
		return ExitNoSlot
	}
	fmt.Fprintln(a.Stderr, "slotctl:", err)
	return ExitFailure
}

// parse は、flagと、最大maxPos個の位置引数（pool名）を解釈する。位置引数はflagの前後どちらにも置ける。
func (a *App) parse(name string, args []string, withJSON bool, maxPos int) (asJSON bool, pos []string, err error) {
	fs := flag.NewFlagSet("slotctl "+name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	var j *bool
	if withJSON {
		j = fs.Bool("json", false, "JSONで出力する")
	}
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return false, nil, err
			}
			return false, nil, usagef("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) > maxPos {
		return false, nil, usagef("%s に余分な引数があります: %v", name, pos[maxPos:])
	}
	return j != nil && *j, pos, nil
}

// session は、1回の命令が使う設定・状態・規則をまとめる。
type session struct {
	app   *App
	cfg   *config.Config
	store *store.Store
	mgr   *lease.Manager
}

func (s *session) close() { s.store.Close() }

func (a *App) open() (*session, error) {
	cfg, err := config.Load(a.Getenv, a.Cwd)
	if err != nil {
		return nil, err
	}
	h, repo, err := holder.Resolve(a.Cwd, cfg.Root)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	return &session{
		app:   a,
		cfg:   cfg,
		store: st,
		mgr: &lease.Manager{
			Cfg:    cfg,
			Store:  st,
			Holder: h,
			Repo:   repo,
			Now:    a.Now,
			Runner: &command.Runner{Out: a.Stderr},
			Events: &eventlog.Log{Dir: cfg.StateDir, Project: cfg.Project, RetentionMonths: cfg.LogRetentionMonths, Now: a.Now},
			Warn:   a.Stderr,
		},
	}, nil
}

func (a *App) formatTime(unix int64) string {
	return time.Unix(unix, 0).In(a.Now().Location()).Format(time.RFC3339)
}

func portList(ps []ports.Port) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = fmt.Sprintf("%s=%d", p.Name, p.Number)
	}
	return strings.Join(parts, " ")
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// pool は、必須の位置引数のpool名の設定を返す。無いpoolは使い方の誤り。
func (s *session) pool(pos []string) (config.Pool, error) {
	if len(pos) != 1 {
		return config.Pool{}, usagef("acquire <pool> はpoolを指定してください")
	}
	name := pos[0]
	p, ok := s.cfg.Pool(name)
	if !ok {
		return config.Pool{}, s.unknownPool(name)
	}
	return p, nil
}

func (s *session) unknownPool(name string) error {
	var names []string
	for _, p := range s.cfg.Pools() {
		names = append(names, p.Name)
	}
	return usagef("未知のpool %q です（設定にあるpool: %s）", name, strings.Join(names, "・"))
}

// AcquireResult は、acquire --json の出力である。
type AcquireResult struct {
	Project   string         `json:"project"`
	Pool      string         `json:"pool"`
	Slot      int            `json:"slot"`
	Name      string         `json:"name"`
	Holder    string         `json:"holder"`
	Ports     map[string]int `json:"ports"`
	ExpiresAt string         `json:"expires_at"`
}

func (a *App) acquire(args []string) error {
	asJSON, pos, err := a.parse("acquire", args, true, 1)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("acquire <pool> はpoolを指定してください")
	}
	s, err := a.open()
	if err != nil {
		return noProjectFileIsUsage(err)
	}
	defer s.close()
	pool, err := s.pool(pos)
	if err != nil {
		return err
	}

	g, err := s.mgr.Acquire(context.Background(), pool)
	if err != nil {
		var ne *lease.NoSlotError
		if errors.As(err, &ne) {
			a.printNoSlot(ne, asJSON)
		}
		return err
	}
	res := AcquireResult{
		Project:   s.cfg.Project,
		Pool:      pool.Name,
		Slot:      g.Slot,
		Name:      g.Name,
		Holder:    g.Holder,
		Ports:     ports.Map(g.Ports),
		ExpiresAt: a.formatTime(g.ExpiresAt),
	}
	if asJSON {
		return writeJSON(a.Stdout, res)
	}
	fmt.Fprintf(a.Stdout, "slot: %d\nname: %s\nport: %s\nexpires: %s\n", res.Slot, res.Name, portList(g.Ports), res.ExpiresAt)
	return nil
}

// printNoSlot は、使用中のholderと期限を出す。
func (a *App) printNoSlot(ne *lease.NoSlotError, asJSON bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s。使用中:", ne.Error())
	type holderJSON struct {
		Slot      int    `json:"slot"`
		Holder    string `json:"holder"`
		ExpiresAt string `json:"expires_at"`
	}
	hs := make([]holderJSON, 0, len(ne.Leases))
	for _, l := range ne.Leases {
		fmt.Fprintf(&b, "\n  slot %d: %s（期限 %s）", l.Slot, l.Holder, a.formatTime(l.ExpiresAt))
		hs = append(hs, holderJSON{l.Slot, l.Holder, a.formatTime(l.ExpiresAt)})
	}
	fmt.Fprintln(a.Stderr, "slotctl:", b.String())
	if asJSON {
		writeJSON(a.Stdout, struct {
			Error   string       `json:"error"`
			Project string       `json:"project"`
			Pool    string       `json:"pool"`
			Holders []holderJSON `json:"holders"`
		}{"no free slot", ne.Project, ne.Pool, hs})
	}
}

func noProjectFileIsUsage(err error) error {
	if errors.Is(err, config.ErrNoProjectFile) {
		return usagef("%v", err)
	}
	return err
}

func (a *App) renew(args []string) error {
	if _, _, err := a.parse("renew", args, false, 0); err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		if errors.Is(err, config.ErrNoProjectFile) {
			// hookからどのrepositoryで呼ばれても邪魔をしない。
			return nil
		}
		return err
	}
	defer s.close()
	return s.mgr.Renew(context.Background())
}

func (a *App) reclaim(args []string) error {
	if _, _, err := a.parse("reclaim", args, false, 0); err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		if errors.Is(err, config.ErrNoProjectFile) {
			return nil
		}
		return err
	}
	defer s.close()
	return s.mgr.Reclaim(context.Background())
}

func (a *App) release(args []string) error {
	_, pos, err := a.parse("release", args, false, 1)
	if err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		if errors.Is(err, config.ErrNoProjectFile) {
			return nil
		}
		return err
	}
	defer s.close()
	pool := ""
	if len(pos) > 0 {
		if _, ok := s.cfg.Pool(pos[0]); !ok {
			return s.unknownPool(pos[0])
		}
		pool = pos[0]
	}
	return s.mgr.Release(context.Background(), pool)
}

type statusSlot struct {
	Pool      string         `json:"pool"`
	Slot      int            `json:"slot"`
	Name      string         `json:"name"`
	State     lease.State    `json:"state"`
	Holder    string         `json:"holder,omitempty"`
	ExpiresAt string         `json:"expires_at,omitempty"`
	Ports     map[string]int `json:"ports"`
}

func (a *App) status(args []string) error {
	asJSON, _, err := a.parse("status", args, true, 0)
	if err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		return noProjectFileIsUsage(err)
	}
	defer s.close()
	slots, err := s.mgr.Status(context.Background())
	if err != nil {
		return err
	}

	if asJSON {
		out := make([]statusSlot, len(slots))
		for i, st := range slots {
			out[i] = statusSlot{Pool: st.Pool, Slot: st.Slot, Name: st.Name, State: st.State, Holder: st.Holder, Ports: ports.Map(st.Ports)}
			if st.State != lease.Free {
				out[i].ExpiresAt = a.formatTime(st.ExpiresAt)
			}
		}
		return writeJSON(a.Stdout, struct {
			Project string       `json:"project"`
			Slots   []statusSlot `json:"slots"`
		}{s.cfg.Project, out})
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tSLOT\tNAME\tSTATE\tHOLDER\tEXPIRES\tPORT")
	for _, st := range slots {
		holder, exp := "-", "-"
		if st.State != lease.Free {
			holder, exp = st.Holder, a.formatTime(st.ExpiresAt)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", st.Pool, st.Slot, st.Name, st.State, holder, exp, portList(st.Ports))
	}
	return tw.Flush()
}
