// Package lease は、枠を借りる・延ばす・返す・一覧する規則である。
// 時刻は注入され、commandの実行と記録は呼び出し側が渡した実体で行う。
package lease

import (
	"context"
	"database/sql"
	"fmt"
	"io"
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

// NoSlotError は、空きが無い（貸し出し中で、期限切れも無い）ことを表す。
type NoSlotError struct {
	Project string
	Leases  []store.Lease // 使用中の枠
}

func (e *NoSlotError) Error() string {
	return fmt.Sprintf("project %q の枠に空きがありません", e.Project)
}

// Grant は、acquireの結果である。
type Grant struct {
	store.Lease
	Name     string
	PortBase int
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

func (m *Manager) vars(slot int, holder string, base int) command.Vars {
	return command.Vars{
		Project: m.Cfg.Project,
		Slot:    slot,
		Name:    m.Cfg.SlotName(slot),
		Holder:  holder,
		Ports:   ports.Assign(m.Cfg.PortNames, base, slot),
		Extra:   m.Cfg.Env,
	}
}

// Acquire は、枠を借りる。
//
// 1つのtransactionで割り当てを決める。同じholderの枠は延長して返し（冪等）、
// 無ければ空き枠の最も小さい番号、それも無ければ期限切れのうちexpires_atが最も古い枠を譲る。
// どれも無ければ *NoSlotError を返す。
// transactionの外で、譲った場合は前のholderのenvでdownを、次にupを走らせる。upが失敗したらleaseを消す。
func (m *Manager) Acquire(ctx context.Context) (*Grant, error) {
	started := time.Now()
	g, err := m.grant(ctx)
	if err != nil {
		if _, ok := err.(*NoSlotError); ok {
			m.record(eventlog.Event{Event: "acquire", Holder: m.Holder, OK: eventlog.BoolPtr(false), MS: eventlog.MS(time.Since(started)), Error: "no free slot"})
		}
		return nil, err
	}
	if !g.Created {
		return g, nil
	}
	if g.Previous != "" {
		m.record(eventlog.Event{Event: "preempt", Holder: m.Holder, Slot: g.Slot, PreviousHolder: g.Previous})
		m.runDown(g.Slot, g.Previous, g.PortBase)
	}
	if err := m.runUp(ctx, g); err != nil {
		m.record(eventlog.Event{Event: "acquire", Holder: m.Holder, Slot: g.Slot, OK: eventlog.BoolPtr(false), MS: eventlog.MS(time.Since(started)), Error: err.Error()})
		return nil, fmt.Errorf("up が失敗したので枠を返しました: %w", err)
	}
	m.record(eventlog.Event{Event: "acquire", Holder: m.Holder, Slot: g.Slot, OK: eventlog.BoolPtr(true), MS: eventlog.MS(time.Since(started))})
	return g, nil
}

func (m *Manager) grant(ctx context.Context) (*Grant, error) {
	var g Grant
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error {
		base, err := store.EnsureProject(ctx, tx, m.Cfg.Project, m.Repo, m.Cfg.PortStart)
		if err != nil {
			return err
		}
		now := m.Now().Unix()
		exp := now + m.ttlSeconds()
		leases, err := store.LeasesOf(ctx, tx, m.Cfg.Project)
		if err != nil {
			return err
		}
		g = Grant{PortBase: base}

		// 1. 同じholderの枠は、延長して返す。
		for _, l := range leases {
			if l.Holder != m.Holder {
				continue
			}
			if err := store.ExtendLease(ctx, tx, l.Project, l.Slot, now, exp); err != nil {
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
		for slot := 1; slot <= m.Cfg.Count; slot++ {
			if taken[slot] {
				continue
			}
			g.Lease = store.Lease{Project: m.Cfg.Project, Slot: slot, Holder: m.Holder, AcquiredAt: now, RenewedAt: now, ExpiresAt: exp}
			g.Created = true
			return store.InsertLease(ctx, tx, g.Lease)
		}

		// 3. 期限切れのうち、expires_atが最も古い枠を譲る。
		var victim *store.Lease
		inUse := make([]store.Lease, 0, len(leases))
		for i := range leases {
			if leases[i].Slot > m.Cfg.Count {
				continue
			}
			inUse = append(inUse, leases[i])
			if leases[i].ExpiresAt <= now && (victim == nil || leases[i].ExpiresAt < victim.ExpiresAt) {
				victim = &leases[i]
			}
		}
		if victim == nil {
			return &NoSlotError{Project: m.Cfg.Project, Leases: inUse}
		}
		g.Lease = store.Lease{Project: m.Cfg.Project, Slot: victim.Slot, Holder: m.Holder, AcquiredAt: now, RenewedAt: now, ExpiresAt: exp}
		g.Created = true
		g.Previous = victim.Holder
		return store.TransferLease(ctx, tx, g.Lease)
	})
	if err != nil {
		return nil, err
	}
	g.Name = m.Cfg.SlotName(g.Slot)
	g.Ports = ports.Assign(m.Cfg.PortNames, g.PortBase, g.Slot)
	return &g, nil
}

// runUp は、upを走らせて記録する。commandが無ければ何もしない。失敗したらleaseを消す。
func (m *Manager) runUp(ctx context.Context, g *Grant) error {
	if m.Cfg.Up == "" {
		return nil
	}
	started := time.Now()
	err := m.Runner.Run(m.Cfg.Up, m.Holder, m.vars(g.Slot, m.Holder, g.PortBase))
	ev := eventlog.Event{Event: "up", Holder: m.Holder, Slot: g.Slot, OK: eventlog.BoolPtr(err == nil), MS: eventlog.MS(time.Since(started))}
	if err != nil {
		ev.Error = err.Error()
	}
	m.record(ev)
	if err != nil {
		m.dropLease(ctx, g.Lease)
	}
	return err
}

// runDown は、holderのenvでdownを走らせて記録する。失敗は警告を出して、errorを返す。
func (m *Manager) runDown(slot int, holder string, base int) error {
	if m.Cfg.Down == "" {
		return nil
	}
	started := time.Now()
	err := m.Runner.Run(m.Cfg.Down, holder, m.vars(slot, holder, base))
	ev := eventlog.Event{Event: "down", Holder: holder, Slot: slot, OK: eventlog.BoolPtr(err == nil), MS: eventlog.MS(time.Since(started))}
	if err != nil {
		ev.Error = err.Error()
		m.warnf("slot %d のdownが失敗しました（holder %s）: %v", slot, holder, err)
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

// Renew は、holderのleaseの期限を「今＋ttl」にする。leaseが無ければ何もしない。commandは走らせず、記録も書かない。
func (m *Manager) Renew(ctx context.Context) error {
	now := m.Now().Unix()
	return m.Store.Tx(ctx, func(tx *sql.Tx) error {
		return store.ExtendHolder(ctx, tx, m.Cfg.Project, m.Holder, now, now+m.ttlSeconds())
	})
}

// Release は、holderの枠のdownを走らせて、leaseを消す。leaseが無ければ何もしない。
// downが失敗してもleaseは消し、失敗を記録して、errorを返す。
func (m *Manager) Release(ctx context.Context) error {
	started := time.Now()
	var lease *store.Lease
	var base int
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error {
		b, found, err := store.ProjectBase(ctx, tx, m.Cfg.Project)
		if err != nil || !found {
			return err
		}
		base = b
		lease, err = store.LeaseOfHolder(ctx, tx, m.Cfg.Project, m.Holder)
		return err
	})
	if err != nil || lease == nil {
		return err
	}

	downErr := m.runDown(lease.Slot, m.Holder, base)
	m.dropLease(ctx, *lease)
	ev := eventlog.Event{Event: "release", Holder: m.Holder, Slot: lease.Slot, OK: eventlog.BoolPtr(downErr == nil), MS: eventlog.MS(time.Since(started))}
	if downErr != nil {
		ev.Error = downErr.Error()
	}
	m.record(ev)
	if downErr != nil {
		return fmt.Errorf("down が失敗しました（枠は返しました）: %w", downErr)
	}
	return nil
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
	Slot      int
	Name      string
	State     State
	Holder    string
	ExpiresAt int64
	Ports     []ports.Port
}

// Status は、projectの全枠の状態を返す。
func (m *Manager) Status(ctx context.Context) ([]SlotStatus, error) {
	var base int
	var leases []store.Lease
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if base, err = store.EnsureProject(ctx, tx, m.Cfg.Project, m.Repo, m.Cfg.PortStart); err != nil {
			return err
		}
		leases, err = store.LeasesOf(ctx, tx, m.Cfg.Project)
		return err
	})
	if err != nil {
		return nil, err
	}
	now := m.Now().Unix()
	bySlot := map[int]store.Lease{}
	for _, l := range leases {
		bySlot[l.Slot] = l
	}
	out := make([]SlotStatus, 0, m.Cfg.Count)
	for slot := 1; slot <= m.Cfg.Count; slot++ {
		st := SlotStatus{Slot: slot, Name: m.Cfg.SlotName(slot), State: Free, Ports: ports.Assign(m.Cfg.PortNames, base, slot)}
		if l, ok := bySlot[slot]; ok {
			st.State = Lent
			if l.ExpiresAt <= now {
				st.State = Expired
			}
			st.Holder, st.ExpiresAt = l.Holder, l.ExpiresAt
		}
		out = append(out, st)
	}
	return out, nil
}
