// Package holder は、枠の借り手（holder）とrepositoryの識別を解決する。
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

// Resolve は、借り手（holder）とrepositoryの識別を返す。
//
// holderは、gitのworktreeの実path。gitの外ならcwdの実path。
// repositoryは、gitのcommon dirの実path。gitの外ならslotctl.tomlのあるdirectory。
func Resolve(cwd, configRoot string) (holder, repo string, err error) {
	real, err := RealPath(cwd)
	if err != nil {
		return "", "", fmt.Errorf("cwdを解決できません: %w", err)
	}
	top, ok := gitOutput(real, "rev-parse", "--show-toplevel")
	if !ok || top == "" {
		root, err := RealPath(configRoot)
		if err != nil {
			return "", "", err
		}
		return real, root, nil
	}
	if holder, err = RealPath(top); err != nil {
		return "", "", err
	}
	common, ok := gitOutput(real, "rev-parse", "--git-common-dir")
	if !ok || common == "" {
		return "", "", fmt.Errorf("git common dir を解決できません")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(real, common)
	}
	if repo, err = RealPath(common); err != nil {
		return "", "", err
	}
	return holder, repo, nil
}
