package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/dashboard"
)

const viewUsage = "использование: lawa view <workflow.json> [--listen <host:port>] [--no-open]"

// viewCommand проверяет и раскрывает файл до открытия порта. Команда не получает
// dependencies runtime и не обращается к хранилищу запусков или настройкам Codex.
// Ошибка браузера идёт в stderr: напечатанный URL продолжает работать до Ctrl+C.
func viewCommand(ctx context.Context, args []string, out, stderr io.Writer, openBrowser func(context.Context, string) error) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(out, viewUsage)
		return err
	}
	var options []string
	noOpen := false
	for _, arg := range args {
		if arg == "--no-open" {
			if noOpen {
				return errors.New("параметр --no-open повторён")
			}
			noOpen = true
		} else {
			options = append(options, arg)
		}
	}
	pos, flags, err := parseOptions(options, map[string]bool{"listen": true})
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(viewUsage)
	}
	address := "127.0.0.1:0"
	if value, exists := flags["listen"]; exists {
		if strings.TrimSpace(value) == "" {
			return errors.New("--listen требует непустой host:port")
		}
		address = value
	}
	data, definition, err := loadWorkflowSource(pos[0])
	if err != nil {
		return fmt.Errorf("открыть workflow: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("слушать %s: %w", address, err)
	}
	defer listener.Close()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "::1"
		if ip.To4() != nil {
			host = "127.0.0.1"
		}
	}
	url := "http://" + net.JoinHostPort(host, port) + "/view"
	if _, err := fmt.Fprintln(out, url); err != nil {
		return err
	}
	if !noOpen {
		browserCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := openBrowser(browserCtx, url)
		cancel()
		if err != nil {
			fmt.Fprintf(stderr, "Не удалось открыть браузер: %v. Откройте URL вручную.\n", err)
		}
	}
	return dashboard.ServeDefinition(ctx, listener, data, definition)
}

// openViewBrowser передаёт URL отдельным аргументом системному обработчику.
// Shell не используется; таймаут не даёт внешней команде удерживать просмотрщик.
func openViewBrowser(ctx context.Context, url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "open", url).Run()
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url).Run()
	default:
		return exec.CommandContext(ctx, "xdg-open", url).Run()
	}
}
