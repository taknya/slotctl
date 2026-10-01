package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestACS1ExplicitPoolsAndRequiredAcquirePool(t *testing.T) {
	e := newTestEnv(t)
	root := e.project(`project = "demo"
[pools.dev]
[pools.billing]
`, map[string]int{"dev": 3, "billing": 1})
	slots := statusPools(t, e, root)
	if len(slots) != 4 {
		t.Fatalf("slots: %+v", slots)
	}
	for _, s := range slots {
		if s.Name != fmt.Sprintf("demo-%s-%d", s.Pool, s.Slot) || s.Pool == "default" {
			t.Fatalf("slot: %+v", s)
		}
	}
	if code, _, _ := e.run(root, "acquire"); code != 2 {
		t.Fatalf("acquire without pool: %d", code)
	}
	for _, legacy := range []string{"[slots]\ncount=1", "[ports]\nnames=[]", "[commands]\ndown='true'"} {
		writeFile(t, filepath.Join(root, "slotctl.toml"), "project='demo'\n"+legacy)
		if code, _, _ := e.run(root, "status"); code != 1 {
			t.Fatalf("legacy %s: %d", legacy, code)
		}
	}
	writeFile(t, filepath.Join(root, "slotctl.toml"), "project='demo'\n[pools.dev]\n")
	writeFile(t, filepath.Join(e.home, "config.toml"), "[projects.demo]\nslots=3\n")
	if code, _, _ := e.run(root, "status"); code != 1 {
		t.Fatalf("legacy machine slots: %d", code)
	}
}

func TestACS3ExpiredLeaseRenewsWithoutDown(t *testing.T) {
	e := newTestEnv(t)
	log := filepath.Join(t.TempDir(), "log")
	root := e.project(fmt.Sprintf("project='demo'\n[pools.dev]\ndown='echo down >> %s'\n", log))
	a := holderDir(t, root, "a")
	mustAcquire(t, e, a)
	e.advance(11 * time.Minute)
	s := mustStatus(t, e, a).Slots[0]
	if s.State != "lent" || s.Holder != a || !strings.Contains(s.ExpiresAt, "10:10:00") {
		t.Fatalf("expired status: %+v", s)
	}
	if code, _, _ := e.run(a, "renew"); code != 0 {
		t.Fatal(code)
	}
	if s := mustStatus(t, e, a).Slots[0]; s.ExpiresAt != e.now.Add(10*time.Minute).Format(time.RFC3339) {
		t.Fatalf("renew: %+v", s)
	}
	if len(readLines(t, log)) != 0 {
		t.Fatal("renew ran down")
	}
}

func TestACS4ReclaimHeartbeatBoundaryAndNoOp(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='demo'\n[pools.dev]\ndown='true'\n", map[string]int{"dev": 3})
	a, b, c := holderDir(t, root, "a"), holderDir(t, root, "b"), holderDir(t, root, "c")
	mustAcquire(t, e, a)
	e.advance(time.Minute)
	mustAcquire(t, e, b)
	mustAcquire(t, e, c)
	e.advance(29 * time.Minute)
	e.run(c, "renew")
	if code, out, errs := e.run(c, "reclaim"); code != 0 || out != "" || errs != "" {
		t.Fatalf("reclaim: %d %q %q", code, out, errs)
	}
	slots := mustStatus(t, e, c).Slots
	if slots[0].State != "free" || slots[1].Holder != b || slots[2].Holder != c {
		t.Fatalf("slots: %+v", slots)
	}
	events := rawEvents(t, e.home)
	n := 0
	for _, ev := range events {
		if ev["event"] == "reclaim" {
			n++
			if ev["trigger"] != "reclaim" || ev["holder"] != a {
				t.Fatalf("event: %+v", ev)
			}
		}
	}
	if n != 1 {
		t.Fatalf("reclaims=%d", n)
	}
	before := len(events)
	if code, out, errs := e.run(c, "reclaim"); code != 0 || out != "" || errs != "" {
		t.Fatalf("noop: %d %q %q", code, out, errs)
	}
	if len(rawEvents(t, e.home)) != before {
		t.Fatal("noop wrote events")
	}
}

func TestACS5DownFailureKeepsLeaseAndRetrySucceeds(t *testing.T) {
	for _, cmd := range []string{"release", "reclaim", "acquire"} {
		for _, count := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/count%d", cmd, count), func(t *testing.T) {
				e := newTestEnv(t)
				root := e.project("project='demo'\n[pools.dev]\ndown='false'\n", map[string]int{"dev": count})
				a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
				mustAcquire(t, e, a)
				e.advance(30 * time.Minute)
				cwd := b
				args := []string{cmd}
				want := 1
				if cmd == "release" {
					cwd = a
				}
				if cmd == "acquire" {
					args = append(args, "dev")
					want = 3
					if count == 2 {
						want = 0
					}
				}
				code, _, errs := e.run(cwd, args...)
				if code != want || !strings.Contains(errs, "demo-dev-1") || !strings.Contains(errs, "exit status 1") {
					t.Fatalf("failure: code=%d want=%d err=%q", code, want, errs)
				}
				if s := findSlot(t, statusPools(t, e, a), "dev", 1); s.State != "lent" || s.Holder != a {
					t.Fatalf("lease lost: %+v", s)
				}
				failed := false
				for _, ev := range rawEvents(t, e.home) {
					if ev["event"] == cmd || (cmd == "acquire" && ev["event"] == "reclaim") {
						if ev["ok"] == false && ev["error"] != "" {
							failed = true
						}
					}
				}
				if !failed {
					t.Fatal("missing failure event")
				}
				writeFile(t, filepath.Join(root, "slotctl.toml"), "project='demo'\n[pools.dev]\ndown='true'\n")
				if code, _, errs := e.run(b, "reclaim"); code != 0 {
					t.Fatalf("retry: %d %s", code, errs)
				}
				if s := findSlot(t, statusPools(t, e, a), "dev", 1); s.State != "free" {
					t.Fatalf("retry status: %+v", s)
				}
			})
		}
	}
}

func TestAcquireSameHolderOnlyRenewsBeforeReclaim(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='demo'\n[pools.dev]\ndown='false'\n", map[string]int{"dev": 2})
	a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
	mustAcquire(t, e, a)
	mustAcquire(t, e, b)
	e.advance(30 * time.Minute)
	before := len(rawEvents(t, e.home))
	mustAcquire(t, e, a)
	if len(rawEvents(t, e.home)) != before {
		t.Fatal("same holder triggered commands/events")
	}
	if findSlot(t, statusPools(t, e, a), "dev", 2).Holder != b {
		t.Fatal("other lease lost")
	}
}

func TestACS6StatusOnlyLentAndFreeAfterExpiry(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='demo'\n[pools.dev]\n", map[string]int{"dev": 2})
	mustAcquire(t, e, root)
	e.advance(11 * time.Minute)
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		code, out, errs := e.run(root, args...)
		if code != 0 || errs != "" || strings.Contains(out, "expired") || !strings.Contains(out, "lent") || !strings.Contains(out, "free") || !strings.Contains(out, "2026-09-15T10:10:00Z") {
			t.Fatalf("status: %d %q %q", code, out, errs)
		}
	}
}

func TestFullPoolReportsHoldersAndExpiryJSON(t *testing.T) {
	e := newTestEnv(t)
	root := e.project("project='demo'\n[pools.dev]\n", map[string]int{"dev": 2})
	a, b, c := holderDir(t, root, "a"), holderDir(t, root, "b"), holderDir(t, root, "c")
	mustAcquire(t, e, a)
	mustAcquire(t, e, b)
	code, out, errs := e.run(c, "acquire", "dev", "--json")
	if code != 3 || !strings.Contains(errs, a) || !strings.Contains(errs, b) || !strings.Contains(errs, e.now.Add(10*time.Minute).Format(time.RFC3339)) || !strings.Contains(out, `"error": "no free slot"`) || !strings.Contains(out, `"pool": "dev"`) || !strings.Contains(out, `"holders"`) {
		t.Fatalf("full: %d %q %q", code, out, errs)
	}
}

func TestReclaimContinuesAfterFailureAndAcquireCommitsOtherPoolCleanup(t *testing.T) {
	for _, cmd := range []string{"reclaim", "acquire"} {
		t.Run(cmd, func(t *testing.T) {
			e := newTestEnv(t)
			root := e.project("project='demo'\n[pools.billing]\ndown='true'\n[pools.dev]\ndown='false'\n")
			a, b := holderDir(t, root, "a"), holderDir(t, root, "b")
			mustAcquire(t, e, a)
			acquirePool(t, e, a, "billing")
			e.advance(30 * time.Minute)
			args := []string{cmd}
			want := 1
			if cmd == "acquire" {
				args = append(args, "dev")
				want = 3
			}
			if code, _, _ := e.run(b, args...); code != want {
				t.Fatalf("code=%d", code)
			}
			slots := statusPools(t, e, a)
			if findSlot(t, slots, "billing", 1).State != "free" || findSlot(t, slots, "dev", 1).Holder != a {
				t.Fatalf("partial cleanup: %+v", slots)
			}
		})
	}
}
