// Package command は、枠のup・downをenvつきで`sh -c`で走らせる。
package command

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/taknya/slotctl/internal/ports"
)

// Vars は、commandに渡す枠の情報である。
type Vars struct {
	Project string
	Slot    int
	Name    string
	Holder  string
	Ports   []ports.Port
	// Extra は、machine設定のenv。
	Extra map[string]string
}

// Env は、baseにSLOTCTL_*とmachine設定のenvを足したenvを返す。
func Env(base []string, v Vars) []string {
	env := append([]string{}, base...)
	env = append(env,
		"SLOTCTL_PROJECT="+v.Project,
		fmt.Sprintf("SLOTCTL_SLOT=%d", v.Slot),
		"SLOTCTL_NAME="+v.Name,
		"SLOTCTL_HOLDER="+v.Holder,
	)
	for _, p := range v.Ports {
		env = append(env, fmt.Sprintf("SLOTCTL_PORT_%s=%d", strings.ToUpper(p.Name), p.Number))
	}
	keys := make([]string, 0, len(v.Extra))
	for k := range v.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+v.Extra[k])
	}
	return env
}

// Runner は、commandを走らせる。
type Runner struct {
	// Out は、commandのstdoutとstderrの流し先。stdoutを汚さないようstderrを渡す。
	Out io.Writer
	// Base は、commandの元のenv。nilならこのprocessのenv。
	Base []string
}

// Run は、dirをcwdにして、commandを`sh -c`で走らせる。
func (r *Runner) Run(command, dir string, v Vars) error {
	base := r.Base
	if base == nil {
		base = os.Environ()
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = Env(base, v)
	cmd.Stdout = r.Out
	cmd.Stderr = r.Out
	return cmd.Run()
}
