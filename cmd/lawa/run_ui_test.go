package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// TestRunUI проходит публичный CLI с настоящим HTTP-сервером и подставным Codex.
// Браузер проверяет страницу и API до первого turn: run уже сохранён, URL ведёт
// именно к нему. Проверяются opt-out, серия, отказы UI и освобождение порта.
func TestRunUI(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		noUI, repeat, browserFail, serverFail bool
		cancelRun, runError, delayedBrowser   bool
	}{
		{name: "default"},
		{name: "no-ui", noUI: true},
		{name: "series", repeat: true},
		{name: "series-no-ui", repeat: true, noUI: true},
		{name: "browser-failure", browserFail: true},
		{name: "server-failure", serverFail: true},
		{name: "cancel", cancelRun: true},
		{name: "failed", runError: true},
		{name: "delayed-browser", delayedBrowser: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, cwd := t.TempDir(), t.TempDir()
			workflowPath := filepath.Join(t.TempDir(), "workflow.json")
			if err := os.WriteFile(workflowPath, []byte(`{"version":2,"id":"ui-run","start":["work"],"steps":[{"id":"work","type":"agent","prompt":"Сделай","after":[]}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := newCLIFakeClient()
			runFailure := errors.New("ошибка кубика")
			if tc.cancelRun {
				client.onCommand = func(codex.Command) error { cancel(); return context.Canceled }
			}
			if tc.runError {
				client.onCommand = func(codex.Command) error { return runFailure }
			}
			deps := cliTestDependencies(client, func(context.Context, codex.Connection) error { return nil })
			starts, closes := 0, 0
			var baseURL string
			deps.startUI = func(ctx context.Context, actualRoot string) (*runUIServer, error) {
				starts++
				if actualRoot != root {
					t.Fatalf("UI читает другое хранилище: %s", actualRoot)
				}
				if tc.serverFail {
					return nil, errors.New("нет свободного порта")
				}
				server, err := startRunUI(ctx, actualRoot)
				if err != nil {
					return nil, err
				}
				baseURL = server.url
				stop := server.stop
				server.stop = func() error { closes++; return stop() }
				return server, nil
			}
			var opened []string
			deps.openBrowser = func(ctx context.Context, pageURL string) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("вызов браузера не ограничен таймаутом")
				}
				parsed, err := url.Parse(pageURL)
				if err != nil || !strings.HasPrefix(parsed.Path, "/graph/") {
					t.Fatalf("неверный URL: %s, %v", pageURL, err)
				}
				id := strings.TrimPrefix(parsed.Path, "/graph/")
				if _, err := runstore.Load(root, id); err != nil {
					t.Fatalf("UI открыт до сохранения run: %v", err)
				}
				client.mu.Lock()
				turns := client.runs["work"]
				client.mu.Unlock()
				if turns != len(opened) {
					t.Fatal("UI открывается после запуска кубика")
				}
				paths := []string{parsed.Path, "/api/graph/" + id}
				if tc.delayedBrowser {
					paths = nil
				}
				for _, path := range paths {
					request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
					if err != nil {
						t.Fatal(err)
					}
					response, err := http.DefaultClient.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					body, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr != nil || response.StatusCode != http.StatusOK {
						t.Fatalf("UI недоступен: %d, %s, %v", response.StatusCode, body, readErr)
					}
					if strings.HasPrefix(path, "/api/") && !bytes.Contains(body, []byte(id)) {
						t.Fatalf("API не вернул нужный run: %s", body)
					}
				}
				opened = append(opened, id)
				if tc.browserFail {
					return errors.New("браузер недоступен")
				}
				return nil
			}
			args := []string{"run", workflowPath, "--cwd", cwd, "--task", "Задача", "--root", root}
			if tc.noUI {
				args = append(args, "--no-ui")
			}
			wantRuns := 1
			if tc.repeat {
				args = append(args, "--repeat", "immediate", "--max-runs", "2")
				wantRuns = 2
			}
			var out, stderr bytes.Buffer
			waits := 0
			output := runUIWaitWriter{Writer: &out, onWait: func() {
				waits++
				defer cancel()
				for _, id := range opened {
					// HTTP-запрос сделан после возврата координатора, включая случай,
					// когда браузер впервые загружает страницу уже завершённого run.
					snapshot, err := runstore.Load(root, id)
					wantState := runstore.RunSucceeded
					if tc.runError {
						wantState = runstore.RunRunning
					}
					if err != nil || snapshot.Meta.RunState != wantState {
						t.Fatalf("неожиданное состояние run: %s, %v", snapshot.Meta.RunState, err)
					}
					lock, err := runstore.OpenLocked(root, id)
					if err != nil {
						t.Fatalf("просмотр удерживает координатор: %v", err)
					}
					lock.Close()
					for _, path := range []string{"/graph/" + id, "/api/graph/" + id} {
						response, err := (&http.Client{Timeout: 5 * time.Second}).Get(baseURL + path)
						if err != nil {
							t.Fatalf("UI завершённого run недоступен: %v", err)
						}
						body, readErr := io.ReadAll(response.Body)
						response.Body.Close()
						if readErr != nil || response.StatusCode != http.StatusOK {
							t.Fatalf("итог не загружается: %s, %v", body, readErr)
						}
						if strings.HasPrefix(path, "/api/") && !bytes.Contains(body, []byte(`"State":"`+string(wantState)+`"`)) {
							t.Fatalf("UI не получил итог: %s", body)
						}
					}
				}
				if tc.runError && !strings.Contains(stderr.String(), "Выполнение завершилось с ошибкой") {
					t.Fatal("ошибка скрыта до закрытия UI")
				}
			}}
			err := executeContext(ctx, args, output, &stderr, deps)
			wantWaits := 1
			if tc.noUI || tc.serverFail || tc.cancelRun {
				wantWaits = 0
			}
			if waits != wantWaits {
				t.Fatalf("режим просмотра: %d вместо %d", waits, wantWaits)
			}
			if tc.cancelRun {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("потеряна отмена: %v", err)
				}
			} else if tc.runError {
				if !errors.Is(err, runFailure) {
					t.Fatalf("потеряна ошибка исполнения: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(out.String(), "runId:"); got != wantRuns {
				t.Fatalf("создано %d run вместо %d", got, wantRuns)
			}
			if tc.noUI {
				if starts != 0 || len(opened) != 0 || strings.Contains(out.String(), "UI:") {
					t.Fatal("--no-ui запустил сервер или браузер")
				}
			} else if tc.serverFail {
				if starts != 1 || closes != 0 || len(opened) != 0 || !strings.Contains(stderr.String(), "Не удалось запустить UI") {
					t.Fatalf("неверная обработка отказа сервера: %s", stderr.String())
				}
			} else {
				if starts != 1 || closes != 1 || len(opened) != wantRuns {
					t.Fatalf("неверный жизненный цикл: starts=%d closes=%d opened=%v", starts, closes, opened)
				}
				for _, id := range opened {
					if !strings.Contains(out.String(), fmt.Sprintf("runId: %s\nUI: %s/graph/%s\n", id, baseURL, id)) {
						t.Fatal("URL не связан с напечатанным runId")
					}
				}
				address := strings.TrimPrefix(baseURL, "http://")
				listener, err := net.Listen("tcp", address)
				if err != nil {
					t.Fatalf("порт не освобождён: %v", err)
				}
				listener.Close()
			}
			if tc.browserFail && !strings.Contains(stderr.String(), "вручную") {
				t.Fatal("нет подсказки при отказе браузера")
			}
		})
	}
}

// TestRunUIArguments защищает отрицательный флаг от двусмысленных значений,
// повторов и ошибочного извлечения из текста постановки.
func TestRunUIArguments(t *testing.T) {
	base := []string{"workflow.json", "--cwd", "/tmp", "--task", "Задача"}
	for _, flags := range [][]string{{"--no-ui", "--no-ui"}, {"--no-ui=false"}, {"--no-ui=true"}, {"--no-ui="}, {"--no-ui", "false"}} {
		if _, err := parseRunArguments(append(append([]string{}, base...), flags...)); err == nil {
			t.Fatalf("приняты аргументы %v", flags)
		}
	}
	for _, args := range [][]string{append([]string{"--no-ui"}, base...), append(append([]string{}, base...), "--no-ui")} {
		if parsed, err := parseRunArguments(args); err != nil || !parsed.noUI {
			t.Fatalf("не принят --no-ui: %+v, %v", parsed, err)
		}
	}
	parsed, err := parseRunArguments([]string{"workflow.json", "--cwd", "/tmp", "--task", "--no-ui"})
	if err != nil || parsed.noUI || parsed.task != "--no-ui" {
		t.Fatalf("текст постановки изменил выбор UI: %+v, %v", parsed, err)
	}
	deps := productionDependencies()
	if deps.startUI == nil || deps.openBrowser == nil {
		t.Fatal("production не подключает UI")
	}
}

// TestRunUICancelledBeforeStart не допускает открытия порта при уже отменённом
// запуске, в том числе сразу после выхода из ожидания очередного run серии.
func TestRunUICancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	cancel()
	if server, err := startRunUI(ctx, t.TempDir()); !errors.Is(err, context.Canceled) || server != nil {
		t.Fatalf("сервер запущен после отмены: %v", err)
	}
}

// TestRunUIInvalidInputBeforeServer проверяет порядок побочных эффектов: UI не
// появляется, если workflow не удалось проверить или Codex не готов к запуску.
func TestRunUIInvalidInputBeforeServer(t *testing.T) {
	for _, source := range []string{"missing.json", "../../examples/review.json"} {
		t.Run(source, func(t *testing.T) {
			deps := cliTestDependencies(newCLIFakeClient(), func(context.Context, codex.Connection) error {
				return errors.New("Codex недоступен")
			})
			deps.startUI = func(context.Context, string) (*runUIServer, error) {
				t.Fatal("UI запущен до проверки workflow и Codex")
				return nil, nil
			}
			var out bytes.Buffer
			err := executeContext(t.Context(), []string{"run", source, "--cwd", t.TempDir(), "--task", "Задача", "--root", t.TempDir()}, &out, io.Discard, deps)
			if err == nil || out.Len() != 0 {
				t.Fatalf("ошибочный запуск создал вывод: %s, %v", out.String(), err)
			}
		})
	}
}

// runUIWaitWriter проверяет режим просмотра при появлении пользовательского
// сообщения. Callback выполняется в потоке CLI после освобождения координатора.
type runUIWaitWriter struct {
	io.Writer
	onWait func()
}

func (w runUIWaitWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if err == nil && strings.Contains(string(data), "UI доступен до Ctrl+C.") {
		w.onWait()
	}
	return n, err
}

// TestRunUIReusesServer проходит второй workflow через CLI. Команда возвращает
// результат, не ожидая закрытия чужого UI, а его URL продолжает обслуживаться.
func TestRunUIReusesServer(t *testing.T) {
	root := t.TempDir()
	owner, err := startRunUI(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.stop()
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"id":"reuse","steps":[{"id":"work","type":"agent","prompt":"Сделай","dependsOn":[]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	deps := cliTestDependencies(newCLIFakeClient(), func(context.Context, codex.Connection) error { return nil })
	deps.startUI = startRunUI
	opened := ""
	deps.openBrowser = func(_ context.Context, address string) error { opened = address; return nil }
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := executeContext(ctx, []string{"run", workflowPath, "--root", root, "--cwd", t.TempDir(), "--task", "Задача"}, &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "UI доступен до Ctrl+C.") || !strings.HasPrefix(opened, owner.url+"/graph/") {
		t.Fatalf("сервер не переиспользован: %s, %s", opened, out.String())
	}
	cancel()
	response, err := (&http.Client{Timeout: time.Second}).Get(opened)
	if err != nil {
		t.Fatalf("клиент остановил общий UI: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}
}
