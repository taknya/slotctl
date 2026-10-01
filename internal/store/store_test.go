package store

import (
	"context"
	"database/sql"
	"github.com/taknya/slotctl/internal/config"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ensure(t *testing.T, s *Store, name, repo string) (int, error) {
	t.Helper()
	var base int
	err := s.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		base, err = EnsureProject(context.Background(), tx, name, repo, 12000)
		return err
	})
	return base, err
}

// AC-5: 2つのprojectのport帯は重ならず、同名を別のrepositoryが名乗ると失敗する。
func TestEnsureProjectAssignsBandsAndRejectsOtherRepository(t *testing.T) {
	s := open(t)
	one, err := ensure(t, s, "one", "/repo/one")
	if err != nil || one != 12000 {
		t.Fatalf("one: %d %v", one, err)
	}
	two, err := ensure(t, s, "two", "/repo/two")
	if err != nil || two != 13000 {
		t.Fatalf("two: %d %v", two, err)
	}
	again, err := ensure(t, s, "one", "/repo/one")
	if err != nil || again != one {
		t.Fatalf("同じprojectの帯は変わらないはず: %d %v", again, err)
	}
	if _, err := ensure(t, s, "one", "/repo/other"); err == nil || !strings.Contains(err.Error(), "別のrepository") {
		t.Fatalf("別repositoryの同名projectは拒むはず: %v", err)
	}
}

func TestLeaseRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := InsertLease(ctx, tx, Lease{Project: "p", Pool: "default", Slot: 1, Holder: "a", AcquiredAt: 1, RenewedAt: 1, ExpiresAt: 10}); err != nil {
			return err
		}
		if err := ExtendHolder(ctx, tx, "p", "a", 5, 50); err != nil {
			return err
		}
		l, err := LeaseOfHolder(ctx, tx, "p", "default", "a")
		if err != nil || l == nil || l.ExpiresAt != 50 || l.RenewedAt != 5 {
			t.Fatalf("延長: %+v %v", l, err)
		}
		if err := TransferLease(ctx, tx, Lease{Project: "p", Pool: "default", Slot: 1, Holder: "b", AcquiredAt: 60, RenewedAt: 60, ExpiresAt: 70}); err != nil {
			return err
		}
		if l, _ := LeaseOfHolder(ctx, tx, "p", "default", "a"); l != nil {
			t.Fatalf("譲った後は前のholderのleaseは無いはず: %+v", l)
		}
		// 古い貸し出し（acquired_atが違う）は消せない。
		if err := DeleteLease(ctx, tx, Lease{Project: "p", Pool: "default", Slot: 1, Holder: "b", AcquiredAt: 1}); err != nil {
			return err
		}
		if ls, _ := LeasesOf(ctx, tx, "p"); len(ls) != 1 {
			t.Fatalf("別の貸し出しは消えないはず: %+v", ls)
		}
		if err := DeleteLease(ctx, tx, Lease{Project: "p", Pool: "default", Slot: 1, Holder: "b", AcquiredAt: 60}); err != nil {
			return err
		}
		if ls, _ := LeasesOf(ctx, tx, "p"); len(ls) != 0 {
			t.Fatalf("消えるはず: %+v", ls)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenKeepsStatePrivateToTheUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, DBName): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s: mode %o, want %o", path, got, want)
		}
	}
}

// 名前つきpoolの帯は、machineで空いている帯から割り当たり、記録されて変わらない。
func TestEnsurePoolBandAvoidsEveryUsedBand(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		one, err := EnsureProject(ctx, tx, "one", "/repo/one", 12000)
		if err != nil || one != 12000 {
			t.Fatalf("one: %d %v", one, err)
		}
		b1, err := EnsurePoolBand(ctx, tx, "one", "billing", 12000)
		if err != nil || b1 != 12000 {
			t.Fatalf("billing: %d %v", b1, err)
		}
		two, err := EnsureProject(ctx, tx, "two", "/repo/two", 12000)
		if err != nil || two != 13000 {
			t.Fatalf("poolの帯とも重ならないはず: %d %v", two, err)
		}
		b2, err := EnsurePoolBand(ctx, tx, "one", "tunnel", 12000)
		if err != nil || b2 != 14000 {
			t.Fatalf("tunnel: %d %v", b2, err)
		}
		if again, err := EnsurePoolBand(ctx, tx, "one", "billing", 12000); err != nil || again != b1 {
			t.Fatalf("帯は変わらないはず: %d %v", again, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// v0.1.0のstate.db（user_version 1）を開くと、leaseとport帯が既定poolのものとして引き継がれる。
func TestOpenMigratesV1ToDefaultPool(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, DBName))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE projects (name TEXT PRIMARY KEY, repo TEXT NOT NULL, port_base INTEGER NOT NULL)`,
		`CREATE TABLE leases (project TEXT NOT NULL, slot INTEGER NOT NULL, holder TEXT NOT NULL, acquired_at INTEGER NOT NULL, renewed_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY (project, slot))`,
		`INSERT INTO projects VALUES ('p', '/repo/p', 14000)`,
		`INSERT INTO leases VALUES ('p', 2, 'a', 1, 2, 30)`,
		`INSERT INTO leases VALUES ('p', 3, 'b', 4, 5, 60)`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		var v int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
			t.Fatalf("user_version: %d %v", v, err)
		}
		base, found, err := ProjectBase(ctx, tx, "p")
		if err != nil || !found || base != 14000 {
			t.Fatalf("port帯: %d %v %v", base, found, err)
		}
		ls, err := LeasesOfPool(ctx, tx, "p", "default")
		want := []Lease{
			{Project: "p", Pool: "default", Slot: 2, Holder: "a", AcquiredAt: 1, RenewedAt: 2, ExpiresAt: 30},
			{Project: "p", Pool: "default", Slot: 3, Holder: "b", AcquiredAt: 4, RenewedAt: 5, ExpiresAt: 60},
		}
		if err != nil || !reflect.DeepEqual(ls, want) {
			t.Fatalf("lease: %+v %v", ls, err)
		}
		// 新しいschemaで、同じ番号の枠を別のpoolが持てる。
		return InsertLease(ctx, tx, Lease{Project: "p", Pool: "billing", Slot: 2, Holder: "a", AcquiredAt: 1, RenewedAt: 1, ExpiresAt: 9})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("新しい版のschemaは拒むはず")
	}
}

// machineの定義変更は、同じstateにある別holderのleaseを削除しない。
func TestMachineInventoryChangesPreserveSharedLeases(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	machine := filepath.Join(home, "config.toml")
	if err := os.WriteFile(filepath.Join(root, "slotctl.toml"), []byte("project='p'\n[pools.dev]\ndown='true'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(machine, []byte("[projects.p.pools.dev]\nslots=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(func(k string) string {
		if k == "SLOTCTL_HOME" {
			return home
		}
		return ""
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := cfg.Pool("dev")
	if pool.Count != 2 {
		t.Fatalf("machine枠: %+v", pool)
	}
	s, err := Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	want := []Lease{{Project: "p", Pool: "dev", Slot: 1, Holder: "a", AcquiredAt: 1, RenewedAt: 1, ExpiresAt: 10}, {Project: "p", Pool: "dev", Slot: 2, Holder: "b", AcquiredAt: 1, RenewedAt: 1, ExpiresAt: 10}}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		for _, l := range want {
			if err := InsertLease(ctx, tx, l); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"[projects.p.pools.dev]\nslots=1\n", ""} {
		if err := os.WriteFile(machine, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(func(k string) string {
			if k == "SLOTCTL_HOME" {
				return home
			}
			return ""
		}, root); err != nil {
			t.Fatal(err)
		}
		if err := s.Tx(ctx, func(tx *sql.Tx) error {
			got, err := LeasesOf(ctx, tx, "p")
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("定義変更後のlease: %+v", got)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
