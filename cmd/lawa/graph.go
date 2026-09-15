package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/stray-live-pixel/Lawa/internal/statusreport"
)

// graphCommand экспортирует снимок без запуска агентов. Файл создаётся через
// O_EXCL: повтор команды не уничтожит пользовательский файл или симлинк.
// При ошибке записи удаляется только файл, созданный этим вызовом.
func graphCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	pos, flags, err := parseOptions(args, map[string]bool{"root": true, "theme": true, "output": true})
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("использование: lawa graph <run-id> [--theme dark|light] [--output <файл.png>] [--root <путь>]")
	}
	theme, err := statusreport.ImageTheme(flags["theme"])
	if err != nil {
		return err
	}
	root, err := resolveRoot(flags["root"], deps.userHomeDir)
	if err != nil {
		return err
	}
	image, err := statusreport.RenderRunImage(ctx, root, pos[0], theme, deps.renderer)
	if err != nil {
		return err
	}
	path := flags["output"]
	if path == "" {
		path = "workflow-" + pos[0] + "-" + theme + ".png"
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(image)
	err = errors.Join(err, file.Close())
	if err != nil {
		return errors.Join(err, os.Remove(path))
	}
	_, err = fmt.Fprintln(out, path)
	return err
}
