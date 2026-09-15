package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/stray-live-pixel/Lawa/internal/desktop"
)

// desktopCommand не принимает произвольные пути для привилегированной операции.
// --finder используется только служебным launcher: ошибки видны в диалоге,
// а пользовательский CLI по-прежнему возвращает ошибку в stderr.
func desktopCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	finder := len(args) == 2 && args[0] == "ui" && args[1] == "--finder"
	if len(args) != 1 && !finder {
		return fmt.Errorf("использование: lawa %s", args[0])
	}
	if args[0] == "desktop-app" {
		return desktop.RegisterApp()
	}
	if args[0] == "desktop-hosts" {
		return desktop.RegisterHosts()
	}
	home, err := deps.userHomeDir()
	if err == nil {
		if args[0] == "ui" {
			err = desktop.Open(ctx, home)
		} else {
			var executable string
			executable, err = os.Executable()
			if err == nil {
				executable, err = filepath.Abs(executable)
			}
			if err == nil {
				err = desktop.Install(ctx, home, executable, out)
			}
		}
	}
	if err != nil && finder {
		desktop.Alert(err.Error())
	}
	return err
}
