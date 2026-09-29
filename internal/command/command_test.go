package command

import (
	"bytes"
	"strings"
	"testing"

	"github.com/taknya/slotctl/internal/ports"
)

func TestRunPassesEnvAndCwd(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	r := &Runner{Out: &out, Base: []string{"PATH=/usr/bin:/bin"}}
	v := Vars{
		Project: "demo", Slot: 2, Name: "demo-2", Holder: dir,
		Ports: []ports.Port{{Name: "web", Number: 12100}, {Name: "api_db", Number: 12101}},
		Extra: map[string]string{"EXTRA": "x"},
	}
	err := r.Run(`echo "$SLOTCTL_PROJECT $SLOTCTL_SLOT $SLOTCTL_NAME $SLOTCTL_PORT_WEB $SLOTCTL_PORT_API_DB $EXTRA"; pwd -P`, dir, v)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if lines[0] != "demo 2 demo-2 12100 12101 x" {
		t.Fatalf("env: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], strings.TrimPrefix(dir, "/private")) {
		t.Fatalf("cwd: %q (want %q)", lines[1], dir)
	}
}

func TestRunReportsFailure(t *testing.T) {
	r := &Runner{Out: &bytes.Buffer{}}
	if err := r.Run("exit 3", t.TempDir(), Vars{}); err == nil {
		t.Fatal("失敗するcommandはerrorを返すはず")
	}
}

func TestEnvIncludesPool(t *testing.T) {
	env := Env(nil, Vars{Project: "demo", Pool: "billing", Slot: 1, Name: "demo-billing-1"})
	var pool, name string
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "SLOTCTL_POOL="):
			pool = kv
		case strings.HasPrefix(kv, "SLOTCTL_NAME="):
			name = kv
		}
	}
	if pool != "SLOTCTL_POOL=billing" || name != "SLOTCTL_NAME=demo-billing-1" {
		t.Fatalf("env: %v", env)
	}
}
