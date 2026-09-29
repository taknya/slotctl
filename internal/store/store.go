// Package store は、machineに1つのSQLite（state.db）である。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/taknya/slotctl/internal/config"
	"github.com/taknya/slotctl/internal/ports"
)

// DBName は、状態のfile名。
const DBName = "state.db"

// schemaVersion は、state.dbのschemaの版（PRAGMA user_version）。
// 1はv0.1.0（leaseが(project, slot)で決まる）。2はpoolを持つ。
const schemaVersion = 2

// projects.port_baseは、既定poolのport帯の先頭。名前つきpoolの帯はpool_bandsにある。
const schemaSQL = `
CREATE TABLE projects (
	name TEXT PRIMARY KEY,
	repo TEXT NOT NULL,
	port_base INTEGER NOT NULL
);
CREATE TABLE pool_bands (
	project TEXT NOT NULL,
	pool TEXT NOT NULL,
	port_base INTEGER NOT NULL,
	PRIMARY KEY (project, pool)
);
CREATE TABLE leases (
	project TEXT NOT NULL,
	pool TEXT NOT NULL,
	slot INTEGER NOT NULL,
	holder TEXT NOT NULL,
	acquired_at INTEGER NOT NULL,
	renewed_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (project, pool, slot)
);
`

// migrateV1SQL は、v0.1.0のstate.dbを引き継ぐ。既存のleaseは既定poolのもの、port帯（projects.port_base）は既定poolの帯のまま。
const migrateV1SQL = `
CREATE TABLE pool_bands (
	project TEXT NOT NULL,
	pool TEXT NOT NULL,
	port_base INTEGER NOT NULL,
	PRIMARY KEY (project, pool)
);
CREATE TABLE leases_v2 (
	project TEXT NOT NULL,
	pool TEXT NOT NULL,
	slot INTEGER NOT NULL,
	holder TEXT NOT NULL,
	acquired_at INTEGER NOT NULL,
	renewed_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (project, pool, slot)
);
INSERT INTO leases_v2(project, pool, slot, holder, acquired_at, renewed_at, expires_at)
	SELECT project, '` + config.DefaultPool + `', slot, holder, acquired_at, renewed_at, expires_at FROM leases;
DROP TABLE leases;
ALTER TABLE leases_v2 RENAME TO leases;
`

// Lease は、枠の貸し出しである。時刻はUnix秒。
type Lease struct {
	Project    string
	Pool       string
	Slot       int
	Holder     string
	AcquiredAt int64
	RenewedAt  int64
	ExpiresAt  int64
}

// Store は、state.dbを開いたものである。
type Store struct{ db *sql.DB }

// Open は、dirのstate.dbを開く。transactionは常にBEGIN IMMEDIATEで始まり、複数のprocessから安全に使える。
func Open(dir string) (*Store, error) {
	// 借り手のpathを持つので、同じmachineの他の利用者から読めないようにする
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// SQLiteに作らせると0644になるので、先に空のfileを0600で作る（journalもこの権限に合わせて作られる）
	f, err := os.OpenFile(filepath.Join(dir, DBName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(15000)")
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, DBName), RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("state.db を準備できません: %w", err)
	}
	return s, nil
}

// Close は、state.dbを閉じる。
func (s *Store) Close() error { return s.db.Close() }

// migrate は、schemaを今の版にする。版の確認から更新までを1つのtransactionで行い、
// 複数のprocessが同時に開いても、移行は1回だけ走る。
func (s *Store) migrate(ctx context.Context) error {
	// 今の版なら、書き込みのlockを取らずに済ませる（renewなどの速さを保つ）。
	var cur int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&cur); err != nil {
		return err
	}
	if cur == schemaVersion {
		return nil
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var v int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
			return err
		}
		switch {
		case v == schemaVersion:
			return nil
		case v > schemaVersion:
			return fmt.Errorf("state.db は新しい版のslotctlが作ったものです（schema %d、この版は%d）", v, schemaVersion)
		case v == 0:
			if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
				return err
			}
		case v == 1:
			if _, err := tx.ExecContext(ctx, migrateV1SQL); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
		return err
	})
}

// Tx は、BEGIN IMMEDIATEのtransactionでfnを実行する。fnがerrorを返したらrollbackする。
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// EnsureProject は、projectを記録して、そのport帯の先頭を返す。
// 初めてのprojectには、portStartから1000ずつ空いている帯を割り当てる。
// 同じnameを別のrepositoryが名乗ったら拒む。
func EnsureProject(ctx context.Context, tx *sql.Tx, name, repo string, portStart int) (int, error) {
	var storedRepo string
	var base int
	err := tx.QueryRowContext(ctx, `SELECT repo, port_base FROM projects WHERE name = ?`, name).Scan(&storedRepo, &base)
	if err == nil {
		if storedRepo != repo {
			return 0, fmt.Errorf("project %q は別のrepository（%s）が使っています。このrepositoryは %s です", name, storedRepo, repo)
		}
		return base, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	base, err = allocateBand(ctx, tx, portStart)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects(name, repo, port_base) VALUES (?, ?, ?)`, name, repo, base); err != nil {
		return 0, err
	}
	return base, nil
}

// allocateBand は、machineで使われていない帯（projectの既定poolと名前つきpoolの全て）の先頭を、portStartから1000ずつ探して返す。
func allocateBand(ctx context.Context, tx *sql.Tx, portStart int) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT port_base FROM projects UNION SELECT port_base FROM pool_bands`)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for rows.Next() {
		var b int
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return 0, err
		}
		used[b] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	base := portStart
	for used[base] {
		base += ports.BandSize
	}
	if base+ports.BandSize-1 > ports.Max {
		return 0, fmt.Errorf("portの帯が足りません（%dから始めて%dを超えます）", portStart, ports.Max)
	}
	return base, nil
}

// EnsurePoolBand は、名前つきpoolのport帯の先頭を返す。初めてなら、machineで空いている帯を割り当てて記録する。
func EnsurePoolBand(ctx context.Context, tx *sql.Tx, project, pool string, portStart int) (int, error) {
	var base int
	err := tx.QueryRowContext(ctx, `SELECT port_base FROM pool_bands WHERE project = ? AND pool = ?`, project, pool).Scan(&base)
	if err == nil {
		return base, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if base, err = allocateBand(ctx, tx, portStart); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pool_bands(project, pool, port_base) VALUES (?, ?, ?)`, project, pool, base); err != nil {
		return 0, err
	}
	return base, nil
}

// ProjectBase は、記録済みのprojectのport帯の先頭を返す。未記録ならfoundがfalse。
func ProjectBase(ctx context.Context, tx *sql.Tx, name string) (base int, found bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT port_base FROM projects WHERE name = ?`, name).Scan(&base)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return base, err == nil, err
}

// LeaseColumns は、leasesの列。
const LeaseColumns = `project, pool, slot, holder, acquired_at, renewed_at, expires_at`

func scanLease(row interface{ Scan(...any) error }) (Lease, error) {
	var l Lease
	err := row.Scan(&l.Project, &l.Pool, &l.Slot, &l.Holder, &l.AcquiredAt, &l.RenewedAt, &l.ExpiresAt)
	return l, err
}

func queryLeases(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]Lease, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+LeaseColumns+` FROM leases WHERE `+where+` ORDER BY pool, slot`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LeasesOf は、projectの全poolの全leaseを、poolの名前・枠の番号順に返す。
func LeasesOf(ctx context.Context, tx *sql.Tx, project string) ([]Lease, error) {
	return queryLeases(ctx, tx, `project = ?`, project)
}

// LeasesOfPool は、poolの全leaseを枠の番号順に返す。
func LeasesOfPool(ctx context.Context, tx *sql.Tx, project, pool string) ([]Lease, error) {
	return queryLeases(ctx, tx, `project = ? AND pool = ?`, project, pool)
}

// LeasesOfHolder は、holderの全poolのleaseを、poolの名前順に返す。
func LeasesOfHolder(ctx context.Context, tx *sql.Tx, project, holder string) ([]Lease, error) {
	return queryLeases(ctx, tx, `project = ? AND holder = ?`, project, holder)
}

// LeaseOfHolder は、holderのpoolのleaseを返す。無ければnil。
func LeaseOfHolder(ctx context.Context, tx *sql.Tx, project, pool, holder string) (*Lease, error) {
	l, err := scanLease(tx.QueryRowContext(ctx,
		`SELECT `+LeaseColumns+` FROM leases WHERE project = ? AND pool = ? AND holder = ?`, project, pool, holder))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// InsertLease は、leaseを作る。
func InsertLease(ctx context.Context, tx *sql.Tx, l Lease) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO leases(`+LeaseColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.Project, l.Pool, l.Slot, l.Holder, l.AcquiredAt, l.RenewedAt, l.ExpiresAt)
	return err
}

// TransferLease は、枠のleaseを新しいholderへ譲る。
func TransferLease(ctx context.Context, tx *sql.Tx, l Lease) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE leases SET holder = ?, acquired_at = ?, renewed_at = ?, expires_at = ? WHERE project = ? AND pool = ? AND slot = ?`,
		l.Holder, l.AcquiredAt, l.RenewedAt, l.ExpiresAt, l.Project, l.Pool, l.Slot)
	return err
}

// ExtendLease は、枠のleaseの生存連絡と期限を更新する。
func ExtendLease(ctx context.Context, tx *sql.Tx, project, pool string, slot int, renewedAt, expiresAt int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE leases SET renewed_at = ?, expires_at = ? WHERE project = ? AND pool = ? AND slot = ?`,
		renewedAt, expiresAt, project, pool, slot)
	return err
}

// ExtendHolder は、holderの全poolのleaseの期限を更新する。leaseが無ければ何もしない。
func ExtendHolder(ctx context.Context, tx *sql.Tx, project, holder string, renewedAt, expiresAt int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE leases SET renewed_at = ?, expires_at = ? WHERE project = ? AND holder = ?`,
		renewedAt, expiresAt, project, holder)
	return err
}

// DeleteLease は、そのleaseがまだ同じ貸し出しであるときだけ消す。
func DeleteLease(ctx context.Context, tx *sql.Tx, l Lease) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE project = ? AND pool = ? AND slot = ? AND holder = ? AND acquired_at = ?`,
		l.Project, l.Pool, l.Slot, l.Holder, l.AcquiredAt)
	return err
}
