// slotctl は、並列のsessionが開発用の実行環境を「枠」として借りて返すためのCLIである。
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/taknya/slotctl/internal/cli"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "slotctl:", err)
		os.Exit(cli.ExitFailure)
	}
	app := &cli.App{
		Now:    time.Now,
		Getenv: os.Getenv,
		Cwd:    cwd,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	os.Exit(app.Run(os.Args[1:]))
}
