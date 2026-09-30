// Package lease は、枠を借りる・延ばす・返す・一覧する規則である。
// 時刻は注入され、commandの実行と記録は呼び出し側が渡した実体で行う。
package lease

import (
	"context"
	"database/sql"
	"errors"
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

// NoSlotError は、掃除後もpoolに空きが無いことを表す。
type NoSlotError struct {
	Project string
	Pool    string
	Leases  []store.Lease // poolの使用中の枠
}

func (e *NoSlotError) Error() string {
	return fmt.Sprintf("project %q の pool %q に空きがありません", e.Project, e.Pool)
}

// Grant は、acquireの結果である。
type Grant struct {
	store.Lease
	Name     string
	PortBase int // poolのport帯の先頭。portの名前を持たないpoolでは0
	Ports    []ports.Port
	Created  bool // 新しく借りた（同じholderの延長ではない）
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

// poolBand は、portの名前を持つpoolの帯を割り当てる。
func (m *Manager) poolBand(ctx context.Context, tx *sql.Tx, p config.Pool, _ int) (int, error) {
	if len(p.PortNames) == 0 {
		return 0, nil
	}
	return store.EnsurePoolBand(ctx, tx, m.Cfg.Project, p.Name, m.Cfg.PortStart)
}

// Acquire は、同じholderなら延長だけ行い、それ以外は全poolの期限切れを空けて最小の空き枠を貸す。
// downとlease削除を割当と同じtransactionに置き、別processへの二重割当を防ぐ。
func (m *Manager) Acquire(ctx context.Context, pool config.Pool) (*Grant, error) {
	started := time.Now()
	var g Grant
	var noSlot *NoSlotError
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
		g.PortBase = base
		own, err := store.LeaseOfHolder(ctx, tx, m.Cfg.Project, pool.Name, m.Holder)
		if err != nil {
			return err
		}
		if own != nil {
			if err := store.ExtendLease(ctx, tx, own.Project, own.Pool, own.Slot, now, exp); err != nil {
				return err
			}
			own.RenewedAt, own.ExpiresAt = now, exp
			g.Lease = *own
			return nil
		}
		// 個々のdown失敗は貸出中のまま保持し、他の空き枠を探す。
		_, err = m.reclaimIn(ctx, tx, "acquire", now, int64(projectBase))
		if err != nil {
			return err
		}
		leases, err := store.LeasesOfPool(ctx, tx, m.Cfg.Project, pool.Name)
		if err != nil {
			return err
		}
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
		noSlot = &NoSlotError{Project: m.Cfg.Project, Pool: pool.Name, Leases: leases}
		// 掃除できた他poolのlease削除をcommitしてから空き無しを返す。
		return nil
	})
	if err != nil {
		return nil, err
	}
	if noSlot != nil {
		m.record(eventlog.Event{Event: "acquire", Pool: pool.Name, Holder: m.Holder, OK: eventlog.BoolPtr(false), MS: eventlog.MS(time.Since(started)), Error: "no free slot"})
		return nil, noSlot
	}
	g.Name = m.Cfg.SlotNameIn(pool.Name, g.Slot)
	g.Ports = ports.Assign(pool.PortNames, g.PortBase, g.Slot)
	if !g.Created {
		return &g, nil
	}
	if err := m.runUp(ctx, pool, &g); err != nil {
		m.record(eventlog.Event{Event: "acquire", Pool: pool.Name, Holder: m.Holder, Slot: g.Slot, OK: eventlog.BoolPtr(false), MS: eventlog.MS(time.Since(started)), Error: err.Error()})
		return nil, fmt.Errorf("up が失敗したので枠を返しました: %w", err)
	}
	m.record(eventlog.Event{Event: "acquire", Pool: pool.Name, Holder: m.Holder, Slot: g.Slot, OK: eventlog.BoolPtr(true), MS: eventlog.MS(time.Since(started))})
	return &g, nil
}

// reclaimIn は、期限切れまたはreclaim_afterを超えた全poolのleaseを掃除する。
// down失敗とDB失敗を分け、down失敗でも成功した掃除の結果をcommitする。
func (m *Manager) reclaimIn(ctx context.Context, tx *sql.Tx, trigger string, now, projectBase int64) ([]error, error) {
	leases, err := store.LeasesOf(ctx, tx, m.Cfg.Project)
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, l := range leases {
		due := l.ExpiresAt <= now
		if trigger == "reclaim" {
			due = l.RenewedAt+int64(m.Cfg.ReclaimAfter/time.Second) <= now
		}
		if !due {
			continue
		}
		started := time.Now()
		p, ok := m.Cfg.Pool(l.Pool)
		var downErr error
		if !ok {
			downErr = fmt.Errorf("%s のpool %q が設定に無く、downを実行できません", m.Cfg.SlotNameIn(l.Pool, l.Slot), l.Pool)
			m.warnf("%v", downErr)
		} else {
			base, err := m.poolBand(ctx, tx, p, int(projectBase))
			if err != nil {
				return failures, err
			}
			downErr = m.runDown(p, l.Slot, l.Holder, base)
		}
		ev := eventlog.Event{Event: "reclaim", Pool: l.Pool, Holder: l.Holder, Slot: l.Slot, Trigger: trigger, OK: eventlog.BoolPtr(downErr == nil), MS: eventlog.MS(time.Since(started))}
		if downErr != nil {
			ev.Error = downErr.Error()
			failures = append(failures, downErr)
		} else {
			if err := store.DeleteLease(ctx, tx, l); err != nil {
				return failures, err
			}
		}
		m.record(ev)
	}
	return failures, nil
}

// Reclaim は、最後のハートビートからreclaim_afterたった枠を全poolで空ける。
func (m *Manager) Reclaim(ctx context.Context) error {
	var failures []error
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error {
		base, err := store.EnsureProject(ctx, tx, m.Cfg.Project, m.Repo, m.Cfg.PortStart)
		if err != nil {
			return err
		}
		failures, err = m.reclaimIn(ctx, tx, "reclaim", m.Now().Unix(), int64(base))
		return err
	})
	if err != nil {
		return err
	}
	return errors.Join(failures...)
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
		m.warnf("%s のdownが失敗しました（holder %s）: %v", m.Cfg.SlotNameIn(pool.Name, slot), holder, err)
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

// Release は、指定poolまたはholderの全poolの枠を、down成功後だけ空ける。
func (m *Manager) Release(ctx context.Context, pool string) error {
	var failures []error
	err := m.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, found, err := store.ProjectBase(ctx, tx, m.Cfg.Project)
		if err != nil || !found {
			return err
		}
		// 設定を別repositoryから使って既存leaseを止めることを防ぐ。
		projectBase, err := store.EnsureProject(ctx, tx, m.Cfg.Project, m.Repo, m.Cfg.PortStart)
		if err != nil {
			return err
		}
		leases, err := store.LeasesOfHolder(ctx, tx, m.Cfg.Project, m.Holder)
		if err != nil {
			return err
		}
		for _, l := range leases {
			if pool != "" && pool != l.Pool {
				continue
			}
			started := time.Now()
			var downErr error
			if p, ok := m.Cfg.Pool(l.Pool); ok {
				base, err := m.poolBand(ctx, tx, p, projectBase)
				if err != nil {
					return err
				}
				downErr = m.runDown(p, l.Slot, l.Holder, base)
			} else {
				downErr = fmt.Errorf("%s のpool %q が設定に無く、downを実行できません", m.Cfg.SlotNameIn(l.Pool, l.Slot), l.Pool)
				m.warnf("%v", downErr)
			}
			ev := eventlog.Event{Event: "release", Pool: l.Pool, Holder: l.Holder, Slot: l.Slot, OK: eventlog.BoolPtr(downErr == nil), MS: eventlog.MS(time.Since(started))}
			if downErr != nil {
				ev.Error = downErr.Error()
				failures = append(failures, downErr)
			} else {
				if err := store.DeleteLease(ctx, tx, l); err != nil {
					return err
				}
			}
			m.record(ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return errors.Join(failures...)
}

// State は、枠の状態である。
type State string

const (
	Lent State = "lent"
	Free State = "free"
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

// Status は、projectの全poolの全枠の状態を、poolの名前順で返す。
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
				st.Holder, st.ExpiresAt = l.Holder, l.ExpiresAt
			}
			out = append(out, st)
		}
	}
	return out, nil
}
