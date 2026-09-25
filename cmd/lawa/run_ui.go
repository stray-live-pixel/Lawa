package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/dashboard"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// runUI обслуживает одну команду run, включая всю серию. Сервер создаётся лениво
// после сохранения первого run: неверный ввод и ожидание cron не открывают UI.
// После освобождения координатора UI продолжает показывать сохранённый результат
// до отмены контекста пользователем; фоновые процессы не создаются.
// Методы вызываются последовательно; nil означает явно отключённый UI либо тест
// runtime без подставленной границы startUI.
type runUI struct {
	ctx         context.Context
	root        string
	out, stderr io.Writer
	deps        dependencies
	server      *runUIServer
}

// runUIServer отделяет завершение HTTP от отмены CLI. done закрывается даже при
// отказе сервера, чтобы режим просмотра не ждал Ctrl+C у уже неработающего UI.
// stop отменяет сервер, ждёт его завершения и возвращает ошибку; повтор безопасен.
type runUIServer struct {
	url    string
	done   <-chan struct{}
	stop   func() error
	shared bool
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
	if ui.server == nil {
		var err error
		ui.server, err = ui.deps.startUI(ui.ctx, ui.root)
		if err != nil {
			fmt.Fprintf(ui.stderr, "Не удалось запустить UI: %v. Workflow продолжится без UI.\n", err)
			return
		}
	}
	pageURL := ui.server.url + "/graph/" + url.PathEscape(runID)
	if _, err := fmt.Fprintf(ui.out, "UI: %s\n", pageURL); err != nil {
		fmt.Fprintf(ui.stderr, "Не удалось вывести URL интерфейса: %v.\n", err)
	}
	browserCtx, cancel := context.WithTimeout(ui.ctx, 5*time.Second)
	defer cancel()
	if err := ui.deps.openBrowser(browserCtx, pageURL); err != nil {
		fmt.Fprintf(ui.stderr, "Не удалось открыть браузер: %v. Откройте %s вручную.\n", err, pageURL)
	}
}

// Wait оставляет UI после завершения координатора и всей серии. Вызывается,
// когда runtime уже освободил блокировки: просмотр не удерживает turn или run.
// Ошибка исполнения показывается до ожидания и сохраняется для вызывающего кода.
// При --no-ui, отказе сервера или отмене во время работы ожидания нет.
func (ui *runUI) Wait(runErr error) error {
	if ui == nil || ui.server == nil || ui.server.shared || ui.ctx.Err() != nil {
		return runErr
	}
	select {
	case <-ui.server.done:
		return runErr
	default:
	}
	if runErr != nil {
		fmt.Fprintf(ui.stderr, "Выполнение завершилось с ошибкой: %s\n", runstore.SafeTerminalText(runErr.Error()))
	}
	if _, err := fmt.Fprintln(ui.out, "Выполнение завершено. UI доступен до Ctrl+C."); err != nil {
		return errors.Join(runErr, fmt.Errorf("сообщить о режиме просмотра: %w", err))
	}
	select {
	case <-ui.ctx.Done():
	case <-ui.server.done:
	}
	return runErr
}

// Close освобождает порт при любом исходе команды и ждёт завершения сервера,
// чтобы его goroutine не пережила владельца. Повторный вызов безопасен.
func (ui *runUI) Close() {
	if ui == nil || ui.server == nil || ui.server.shared {
		return
	}
	if err := ui.server.stop(); err != nil {
		fmt.Fprintf(ui.stderr, "Ошибка сервера UI: %v.\n", err)
	}
	ui.server = nil
}

// startRunUI переиспользует готовый UI того же root или резервирует свободный
// loopback-порт. Только владелец нового listener обслуживает и закрывает сервер.
// Командный Engine здесь не запускается: workflow уже ведёт координатор.
// Вызов stop обязателен даже при отменённом ctx; он освобождает ресурсы.
func startRunUI(ctx context.Context, root string) (*runUIServer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	endpoint, err := dashboard.AcquireUI(ctx, root)
	if err != nil {
		return nil, err
	}
	if endpoint.Listener == nil {
		return &runUIServer{url: endpoint.URL, shared: true}, nil
	}
	serverCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var serverErr error
	go func() {
		serverErr = dashboard.ServeRuns(serverCtx, endpoint.Listener, root)
		serverErr = errors.Join(serverErr, endpoint.Close())
		close(done)
	}()
	stop := func() error {
		cancel()
		// Закрытие done синхронизирует чтение serverErr с записью в goroutine.
		<-done
		if isCancellationOnly(serverErr) {
			return nil
		}
		return serverErr
	}
	return &runUIServer{url: endpoint.URL, done: done, stop: stop}, nil
}
