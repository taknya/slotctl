package store

import (
	"context"
	"database/sql"
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
		if err := InsertLease(ctx, tx, Lease{Project: "p", Slot: 1, Holder: "a", AcquiredAt: 1, RenewedAt: 1, ExpiresAt: 10}); err != nil {
			return err
		}
		if err := ExtendHolder(ctx, tx, "p", "a", 5, 50); err != nil {
			return err
		}
		l, err := LeaseOfHolder(ctx, tx, "p", "a")
		if err != nil || l == nil || l.ExpiresAt != 50 || l.RenewedAt != 5 {
			t.Fatalf("延長: %+v %v", l, err)
		}
		if err := TransferLease(ctx, tx, Lease{Project: "p", Slot: 1, Holder: "b", AcquiredAt: 60, RenewedAt: 60, ExpiresAt: 70}); err != nil {
			return err
		}
		if l, _ := LeaseOfHolder(ctx, tx, "p", "a"); l != nil {
			t.Fatalf("譲った後は前のholderのleaseは無いはず: %+v", l)
		}
		// 古い貸し出し（acquired_atが違う）は消せない。
		if err := DeleteLease(ctx, tx, Lease{Project: "p", Slot: 1, Holder: "b", AcquiredAt: 1}); err != nil {
			return err
		}
		if ls, _ := LeasesOf(ctx, tx, "p"); len(ls) != 1 {
			t.Fatalf("別の貸し出しは消えないはず: %+v", ls)
		}
		if err := DeleteLease(ctx, tx, Lease{Project: "p", Slot: 1, Holder: "b", AcquiredAt: 60}); err != nil {
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
