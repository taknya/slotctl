// Package lease は、枠を借りる・延ばす・返す・一覧する規則である。
// 時刻は注入され、commandの実行と記録は呼び出し側が渡した実体で行う。
package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/taknya/slotctl/internal/command"
	"github.com/taknya/slotctl/internal/config"
	"github.com/taknya/slotctl/internal/eventlog"
	"github.com/taknya/slotctl/internal/ports"
	"github.com/taknya/slotctl/internal/store"
)

// Runner は、up・downのcommandを走らせる。
type Runner interface {
	Run(command, dir string, v command.Vars) error
}

// Manager は、1つのprojectの1つのholderが枠を扱うための入口である。
type Manager struct {
	Cfg    *config.Config
	Store  *store.Store
	Holder string
	Repo   string
	Now    func() time.Time
	Runner Runner
	Events *eventlog.Log
	// Warn は、記録やcommandの失敗など、命令を止めない警告の流し先。
	Warn io.Writer
}

// NoSlotError は、poolに空きが無い（貸し出し中で、期限切れも無い）ことを表す。
type NoSlotError struct {
	Project string
	Pool    string
	Leases  []store.Lease // poolの使用中の枠
}

func (e *NoSlotError) Error() string {
	if e.Pool == config.DefaultPool {
		return fmt.Sprintf("project %q の枠に空きがありません", e.Project)
	}
	return fmt.Sprintf("project %q の pool %q に空きがありません", e.Project, e.Pool)
}

// Grant は、acquireの結果である。
type Grant struct {
	store.Lease
	Name     string
	PortBase int // poolのport帯の先頭。portの名前を持たないpoolでは0
	Ports    []ports.Port
	Created  bool   // 新しく借りた（同じholderの延長ではない）
	Previous string // 譲られた枠なら、前のholder
}

func (m *Manager) ttlSeconds() int64 { return int64(m.Cfg.TTL / time.Second) }

func (m *Manager) warnf(format string, args ...any) {
	if m.Warn != nil {
		fmt.Fprintf(m.Warn, "slotctl: "+format+"\n", args...)
	}
}

func (m *Manager) record(ev eventlog.Event) {
	if m.Events == nil {
		return
	}
	if err := m.Events.Record(ev); err != nil {
		m.warnf("記録を書けませんでした: %v", err)
	}
}

func (m *Manager) vars(pool string, slot int, holder string, portNames []string, base int) command.Vars {
	return command.Vars{
		Project: m.Cfg.Project,
		Pool:    pool,
		Slot:    slot,
		Name:    m.Cfg.SlotNameIn(pool, slot),
		Holder:  holder,
		Ports:   ports.Assign(portNames, base, slot),
		Extra:   m.Cfg.Env,
	}
}

// poolBand は、poolのport帯の先頭を返す。既定poolはprojectの帯（defaultBase）。
// 名前つきpoolは、portの名前を持つときだけ帯を持ち、初めてなら割り当てて記録する。
func (m *Manager) poolBand(ctx context.Context, tx *sql.Tx, p config.Pool, defaultBase int) (int, error) {
	if p.Name == config.DefaultPool {
		return defaultBase, nil
	}
	if len(p.PortNames) == 0 {
		return 0, nil
	}
	return store.EnsurePoolBand(ctx, tx, m.Cfg.Project, p.Name, m.Cfg.PortStart)
}

// poolLabel は、警告や失敗の文に入れるpoolの表記。既定poolでは空。
func poolLabel(pool string) string {
	if pool == config.DefaultPool {
		return ""
	}
	return fmt.Sprintf("pool %q の", pool)
}

// Acquire は、poolの枠を借りる。
//
// 1つのtransactionで割り当てを決める。同じholderのpoolの枠は延長して返し（冪等）、
// 無ければ空き枠の最も小さい番号、それも無ければ期限切れのうちexpires_atが最も古い枠を譲る。
// どれも無ければ *NoSlotError を返す。1つのholderが持てる枠は、1poolにつき1つ。
// transactionの外で、譲った場合は前のholderのenvでdownを、次にupを走らせる。upが失敗したらleaseを消す。
func (m *Manager) Acquire(ctx context.Context, pool config.Pool) (*Grant, error) {
	started := time.Now()
	g, err := m.grant(ctx, pool)
	if err != nil {
		if _, ok := err.(*NoSlotError); ok {
			m.record(eventlog.Event{Event: "acquire", Pool: pool.Name, Holder: m.Holder, OK: eventlog.BoolPtr(false), MS: eventlog.MS(time.Since(started)), Error: "no free slot"})
		}
		return nil, err
	}
	if !g.Created {
		return g, nil
	}
	if g.Previous != "" {
		m.record(eventlog.Event{Event: "preempt", Pool: pool.Name, Holder: m.Holder, Slot: g.Slot, PreviousHolder: g.Previous})
		m.runDown(pool, g.Slot, g.Previous, g.PortBase)
	}
	if err := m.runUp(ctx, pool, g); err != nil {
		m.record(eventlog.Event{Event: "acquire", Pool: pool.Name, Holder: m.Holder, Slot: g.Slot, OK: eventlog.BoolPtr(false), MS: eventlog.MS(time.Since(started)), Error: err.Error()})
		return nil, fmt.Errorf("up が失敗したので枠を返しました: %w", err)
	}
	m.record(eventlog.Event{Event: "acquire", Pool: pool.Name, Holder: m.Holder, Slot: g.Slot, OK: eventlog.BoolPtr(true), MS: eventlog.MS(time.Since(started))})
	return g, nil
}

func (m *Manager) grant(ctx context.Context, pool config.Pool) (*Grant, error) {
	var g Grant
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error {
		projectBase, err := store.EnsureProject(ctx, tx, m.Cfg.Project, m.Repo, m.Cfg.PortStart)
		if err != nil {
			return err
		}
		base, err := m.poolBand(ctx, tx, pool, projectBase)
		if err != nil {
			return err
		}
		now := m.Now().Unix()
		exp := now + m.ttlSeconds()
		leases, err := store.LeasesOfPool(ctx, tx, m.Cfg.Project, pool.Name)
		if err != nil {
			return err
		}
		g = Grant{PortBase: base}

		// 1. 同じholderの枠は、延長して返す。
		for _, l := range leases {
			if l.Holder != m.Holder {
				continue
			}
			if err := store.ExtendLease(ctx, tx, l.Project, l.Pool, l.Slot, now, exp); err != nil {
				return err
			}
			l.RenewedAt, l.ExpiresAt = now, exp
			g.Lease = l
			return nil
		}

		// 2. 空き枠の最も小さい番号。
		taken := map[int]bool{}
		for _, l := range leases {
			taken[l.Slot] = true
		}
		for slot := 1; slot <= pool.Count; slot++ {
			if taken[slot] {
				continue
			}
			g.Lease = store.Lease{Project: m.Cfg.Project, Pool: pool.Name, Slot: slot, Holder: m.Holder, AcquiredAt: now, RenewedAt: now, ExpiresAt: exp}
			g.Created = true
			return store.InsertLease(ctx, tx, g.Lease)
		}

		// 3. 期限切れのうち、expires_atが最も古い枠を譲る。
		var victim *store.Lease
		inUse := make([]store.Lease, 0, len(leases))
		for i := range leases {
			if leases[i].Slot > pool.Count {
				continue
			}
			inUse = append(inUse, leases[i])
			if leases[i].ExpiresAt <= now && (victim == nil || leases[i].ExpiresAt < victim.ExpiresAt) {
				victim = &leases[i]
			}
		}
		if victim == nil {
			return &NoSlotError{Project: m.Cfg.Project, Pool: pool.Name, Leases: inUse}
		}
		g.Lease = store.Lease{Project: m.Cfg.Project, Pool: pool.Name, Slot: victim.Slot, Holder: m.Holder, AcquiredAt: now, RenewedAt: now, ExpiresAt: exp}
		g.Created = true
		g.Previous = victim.Holder
		return store.TransferLease(ctx, tx, g.Lease)
	})
	if err != nil {
		return nil, err
	}
	g.Name = m.Cfg.SlotNameIn(pool.Name, g.Slot)
	g.Ports = ports.Assign(pool.PortNames, g.PortBase, g.Slot)
	return &g, nil
}

// runUp は、poolのupを走らせて記録する。commandが無ければ何もしない。失敗したらleaseを消す。
func (m *Manager) runUp(ctx context.Context, pool config.Pool, g *Grant) error {
	if pool.Up == "" {
		return nil
	}
	started := time.Now()
	err := m.Runner.Run(pool.Up, m.Holder, m.vars(pool.Name, g.Slot, m.Holder, pool.PortNames, g.PortBase))
	ev := eventlog.Event{Event: "up", Pool: pool.Name, Holder: m.Holder, Slot: g.Slot, OK: eventlog.BoolPtr(err == nil), MS: eventlog.MS(time.Since(started))}
	if err != nil {
		ev.Error = err.Error()
	}
	m.record(ev)
	if err != nil {
		m.dropLease(ctx, g.Lease)
	}
	return err
}

// runDown は、holderのenvでpoolのdownを走らせて記録する。失敗は警告を出して、errorを返す。
func (m *Manager) runDown(pool config.Pool, slot int, holder string, base int) error {
	if pool.Down == "" {
		return nil
	}
	started := time.Now()
	err := m.Runner.Run(pool.Down, holder, m.vars(pool.Name, slot, holder, pool.PortNames, base))
	ev := eventlog.Event{Event: "down", Pool: pool.Name, Holder: holder, Slot: slot, OK: eventlog.BoolPtr(err == nil), MS: eventlog.MS(time.Since(started))}
	if err != nil {
		ev.Error = err.Error()
		m.warnf("%sslot %d のdownが失敗しました（holder %s）: %v", poolLabel(pool.Name), slot, holder, err)
	}
	m.record(ev)
	return err
}

func (m *Manager) dropLease(ctx context.Context, l store.Lease) {
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error { return store.DeleteLease(ctx, tx, l) })
	if err != nil {
		m.warnf("leaseを消せませんでした: %v", err)
	}
}

// Renew は、holderの全poolのleaseの期限を「今＋ttl」にする。leaseが無ければ何もしない。commandは走らせず、記録も書かない。
func (m *Manager) Renew(ctx context.Context) error {
	now := m.Now().Unix()
	return m.Store.Tx(ctx, func(tx *sql.Tx) error {
		return store.ExtendHolder(ctx, tx, m.Cfg.Project, m.Holder, now, now+m.ttlSeconds())
	})
}

// Release は、holderの枠のdownを走らせて、leaseを消す。poolを指定すればそのpoolの枠、空なら全poolの枠を返す。
// 返す枠が無ければ何もしない。どれかのdownが失敗しても、全てのleaseを消し、失敗を記録して、errorを返す。
func (m *Manager) Release(ctx context.Context, pool string) error {
	type target struct {
		lease store.Lease
		base  int
	}
	var targets []target
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error {
		projectBase, found, err := store.ProjectBase(ctx, tx, m.Cfg.Project)
		if err != nil || !found {
			return err
		}
		leases, err := store.LeasesOfHolder(ctx, tx, m.Cfg.Project, m.Holder)
		if err != nil {
			return err
		}
		for _, l := range leases {
			if pool != "" && l.Pool != pool {
				continue
			}
			t := target{lease: l}
			if p, ok := m.Cfg.Pool(l.Pool); ok {
				if t.base, err = m.poolBand(ctx, tx, p, projectBase); err != nil {
					return err
				}
			}
			targets = append(targets, t)
		}
		return nil
	})
	if err != nil || len(targets) == 0 {
		return err
	}
	// 既定pool、続いて名前つきpoolの名前順に返す。
	sort.SliceStable(targets, func(i, j int) bool {
		a, b := targets[i].lease.Pool, targets[j].lease.Pool
		if (a == config.DefaultPool) != (b == config.DefaultPool) {
			return a == config.DefaultPool
		}
		return a < b
	})

	var errs []error
	for _, t := range targets {
		started := time.Now()
		var downErr error
		if p, ok := m.Cfg.Pool(t.lease.Pool); ok {
			downErr = m.runDown(p, t.lease.Slot, m.Holder, t.base)
		} else {
			m.warnf("pool %q は設定に無いので、downを走らせずに枠を返します", t.lease.Pool)
		}
		m.dropLease(ctx, t.lease)
		ev := eventlog.Event{Event: "release", Pool: t.lease.Pool, Holder: m.Holder, Slot: t.lease.Slot, OK: eventlog.BoolPtr(downErr == nil), MS: eventlog.MS(time.Since(started))}
		if downErr != nil {
			ev.Error = downErr.Error()
		}
		m.record(ev)
		if downErr != nil {
			errs = append(errs, fmt.Errorf("%sdown が失敗しました（枠は返しました）: %w", poolLabel(t.lease.Pool), downErr))
		}
	}
	return errors.Join(errs...)
}

// State は、枠の状態である。
type State string

const (
	Lent    State = "lent"
	Expired State = "expired"
	Free    State = "free"
)

// SlotStatus は、1つの枠の状態である。
type SlotStatus struct {
	Pool      string
	Slot      int
	Name      string
	State     State
	Holder    string
	ExpiresAt int64
	Ports     []ports.Port
}

// Status は、projectの全poolの全枠の状態を、既定pool、続いて名前つきpoolの名前順で返す。
func (m *Manager) Status(ctx context.Context) ([]SlotStatus, error) {
	pools := m.Cfg.Pools()
	bases := make([]int, len(pools))
	var leases []store.Lease
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error {
		projectBase, err := store.EnsureProject(ctx, tx, m.Cfg.Project, m.Repo, m.Cfg.PortStart)
		if err != nil {
			return err
		}
		for i, p := range pools {
			if bases[i], err = m.poolBand(ctx, tx, p, projectBase); err != nil {
				return err
			}
		}
		leases, err = store.LeasesOf(ctx, tx, m.Cfg.Project)
		return err
	})
	if err != nil {
		return nil, err
	}
	now := m.Now().Unix()
	type key struct {
		pool string
		slot int
	}
	byKey := map[key]store.Lease{}
	for _, l := range leases {
		byKey[key{l.Pool, l.Slot}] = l
	}
	var out []SlotStatus
	for i, p := range pools {
		for slot := 1; slot <= p.Count; slot++ {
			st := SlotStatus{Pool: p.Name, Slot: slot, Name: m.Cfg.SlotNameIn(p.Name, slot), State: Free, Ports: ports.Assign(p.PortNames, bases[i], slot)}
			if l, ok := byKey[key{p.Name, slot}]; ok {
				st.State = Lent
				if l.ExpiresAt <= now {
					st.State = Expired
				}
				st.Holder, st.ExpiresAt = l.Holder, l.ExpiresAt
			}
			out = append(out, st)
		}
	}
	return out, nil
}
