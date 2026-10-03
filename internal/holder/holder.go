// Package holder は、枠の借り手（holder）を解決する。
package holder

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// RealPath はsymlinkを解決した絶対pathを返す。
func RealPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func gitOutput(dir string, args ...string) (string, bool) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return strings.TrimSpace(out.String()), true
}

// Resolve は、借り手（holder）を返す。
//
// holderは、gitのworktreeの実path。gitの外ならcwdの実path。
func Resolve(cwd string) (string, error) {
	real, err := RealPath(cwd)
	if err != nil {
		return "", fmt.Errorf("cwdを解決できません: %w", err)
	}
	top, ok := gitOutput(real, "rev-parse", "--show-toplevel")
	if !ok || top == "" {
		return real, nil
	}
	return RealPath(top)
}
