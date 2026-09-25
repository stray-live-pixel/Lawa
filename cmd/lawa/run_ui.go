package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/dashboard"
)

// runUI обслуживает одну команду run, включая всю серию. Сервер создаётся лениво
// после сохранения первого run: неверный ввод и ожидание cron не открывают UI.
// Методы вызываются последовательно; nil означает явно отключённый UI либо тест
// runtime без подставленной границы startUI.
type runUI struct {
	ctx         context.Context
	root        string
	out, stderr io.Writer
	deps        dependencies
	baseURL     string
	stop        func() error
}

// newRunUI готовит сессию без открытия порта или обращения к браузеру.
func newRunUI(ctx context.Context, root string, disabled bool, out, stderr io.Writer, deps dependencies) *runUI {
	if disabled || deps.startUI == nil {
		return nil
	}
	return &runUI{ctx: ctx, root: root, out: out, stderr: stderr, deps: deps}
}

// Show открывает сохранённый run до запуска его кубиков. Ошибки вспомогательного
// UI видны в stderr, но не меняют результат workflow и не мешают CLI-наблюдению.
func (ui *runUI) Show(runID string) {
	if ui == nil || ui.ctx.Err() != nil {
		return
	}
	if ui.stop == nil {
		var err error
		ui.baseURL, ui.stop, err = ui.deps.startUI(ui.ctx, ui.root)
		if err != nil {
			fmt.Fprintf(ui.stderr, "Не удалось запустить UI: %v. Workflow продолжится без UI.\n", err)
			return
		}
	}
	pageURL := ui.baseURL + "/graph/" + url.PathEscape(runID)
	if _, err := fmt.Fprintf(ui.out, "UI: %s\n", pageURL); err != nil {
		fmt.Fprintf(ui.stderr, "Не удалось вывести URL интерфейса: %v.\n", err)
	}
	browserCtx, cancel := context.WithTimeout(ui.ctx, 5*time.Second)
	defer cancel()
	if err := ui.deps.openBrowser(browserCtx, pageURL); err != nil {
		fmt.Fprintf(ui.stderr, "Не удалось открыть браузер: %v. Откройте %s вручную.\n", err, pageURL)
	}
}

// Close освобождает порт при любом исходе команды и ждёт завершения сервера,
// чтобы его goroutine не пережила владельца. Повторный вызов безопасен.
func (ui *runUI) Close() {
	if ui == nil || ui.stop == nil {
		return
	}
	if err := ui.stop(); err != nil {
		fmt.Fprintf(ui.stderr, "Ошибка сервера UI: %v.\n", err)
	}
	ui.stop = nil
}

// startRunUI захватывает свободный loopback-порт до публикации URL. Каждый
// процесс смотрит в свой root и не зависит от чужого lawa serve на стандартном
// порту. Командный Engine здесь не запускается: workflow уже ведёт координатор.
// Возвращённый stop обязателен даже при отменённом ctx; он освобождает ресурсы.
func startRunUI(ctx context.Context, root string) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	serverCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- dashboard.ServeRuns(serverCtx, listener, root)
	}()
	stop := func() error {
		cancel()
		err := <-done
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return "http://" + listener.Addr().String(), stop, nil
}
