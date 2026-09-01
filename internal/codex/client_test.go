package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/buildinfo"
)

// TestMain позволяет запускать этот же бинарник вместо Codex. Проверяются
// настоящие stdio и завершение процесса, но модель и пользовательские чаты не нужны.
func TestMain(m *testing.M) {
	if scenario := os.Getenv("LAWA_TEST_CODEX_SERVER"); scenario != "" {
		fakeServer(scenario)
		return
	}
	os.Exit(m.Run())
}

// fakeServer проверяет порядок и содержимое запросов, в том числе встречный
// запрос со строковым ID. Завершение раньше ответа и чужие события искусственны:
// это регрессионная защита маршрутизации, не описание известного сбоя Codex.
func fakeServer(scenario string) {
	input, output := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	send := func(v any) { _ = output.Encode(v) }
	// Политика процесса обязана предшествовать всем RPC и не зависеть от того,
	// создаётся thread, продолжается он или открывается read-only observer.
	arguments := strings.Join(os.Args[1:], "\n")
	if !strings.Contains(arguments, "app-server\n-c\napproval_policy=\"on-request\"\n-c\napprovals_reviewer=\"auto_review\"") {
		panic("политика approval не передана в argv App Server")
	}
	configuredRuntime := scenario == "settings-run" || scenario == "settings-continue"
	runtimeMatches := func(params map[string]any, includeEffort bool) bool {
		if !configuredRuntime {
			return params["model"] == nil && params["effort"] == nil && params["serviceTier"] == nil
		}
		if params["model"] != "gpt-test" || params["serviceTier"] != "fast" {
			return false
		}
		return !includeEffort && params["effort"] == nil || includeEffort && params["effort"] == "high"
	}
	for {
		var m envelope
		if input.Decode(&m) != nil {
			return
		}
		fmt.Fprintln(os.Stderr, m.Method)
		var p map[string]any
		_ = json.Unmarshal(m.Params, &p)
		if scenario == "eof:"+m.Method {
			return
		}
		if scenario == "error:"+m.Method {
			// Ответ собираем не через RPCError, чтобы тест проверял
			// декодирование полного внешнего JSON, а не обратную сериализацию
			// того же Go-типа. Data нужно вызывающему коду для решения о повторе,
			// но оно не должно попадать в обычный текст ошибки.
			send(map[string]any{"id": m.ID, "error": map[string]any{
				"code": -32000, "message": "test rejection", "data": map[string]any{"retryable": true},
			}})
			continue
		}
		reply := func(v any) { send(map[string]any{"id": m.ID, "result": v}) }
		switch m.Method {
		case "initialize":
			clientInfo := p["clientInfo"].(map[string]any)
			if p["capabilities"].(map[string]any)["experimentalApi"] != true ||
				clientInfo["name"] != "lawa" || clientInfo["version"] != buildinfo.CodexVersion() {
				panic("неверны возможности или единая версия clientInfo")
			}
			reply(map[string]any{})
		case "initialized":
			if len(m.ID) != 0 {
				panic("initialized должен быть уведомлением")
			}
		case "thread/start":
			cwd, _ := os.Getwd()
			_, hasHistoryMode := p["historyMode"]
			validIsolation := p["sandbox"] == "read-only" && p["permissions"] == nil
			if scenario == "permissions" {
				validIsolation = p["sandbox"] == nil && p["permissions"] == "lawa-test"
				arguments := strings.Join(os.Args, "\n")
				want := `permissions.lawa-test={extends=":workspace",filesystem={"/run dir"="read","/run dir/own.md"="write"}}`
				if !strings.Contains(arguments, "\n-c\n"+want+"\n--stdio") {
					panic("профиль не передан app-server одним безопасным аргументом")
				}
			}
			dynamicTools, hasDynamicTools := p["dynamicTools"].([]any)
			validDynamicTools := !hasDynamicTools
			if scenario == "dynamic" {
				validDynamicTools = hasDynamicTools && len(dynamicTools) == 2 &&
					dynamicTools[0].(map[string]any)["name"] == "run_child" && dynamicTools[0].(map[string]any)["type"] == "function" &&
					dynamicTools[0].(map[string]any)["inputSchema"].(map[string]any)["type"] == "object" &&
					dynamicTools[1].(map[string]any)["name"] == "run_children"
			}
			if p["cwd"] != cwd || hasHistoryMode || !validIsolation || !validDynamicTools || !runtimeMatches(p, false) || p["approvalPolicy"] != "on-request" || p["approvalsReviewer"] != "auto_review" {
				panic("искажены параметры чата, автономность или модель")
			}
			id := "thread-1"
			if scenario == "missing-id" {
				id = ""
			}
			reply(map[string]any{"thread": map[string]any{"id": id}})
		case "thread/resume":
			cwd, _ := os.Getwd()
			validIsolation := p["sandbox"] == "read-only" && p["permissions"] == nil
			if p["threadId"] != "thread-1" || p["cwd"] != cwd || !validIsolation || !runtimeMatches(p, false) ||
				p["approvalPolicy"] != "on-request" || p["approvalsReviewer"] != "auto_review" {
				panic("искажены параметры продолжения чата")
			}
			reply(map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd}})
		case "thread/name/set":
			// Имя задаётся уже созданному чату. Проверка обоих полей защищает
			// контракт запроса: сервер не должен молча принимать неверный ID или заголовок.
			if p["threadId"] != "thread-1" || p["name"] != "Test" {
				panic("искажены ID чата или его имя")
			}
			reply(map[string]any{})
		case "turn/start":
			if scenario == "malformed" {
				fmt.Fprintln(os.Stdout, "{broken JSON")
				return
			}
			items := p["input"].([]any)
			want := "literal '$()`\\n"
			if strings.HasPrefix(scenario, "continue:") || scenario == "settings-continue" {
				want = "continue"
			}
			if scenario == "skill" {
				want = "$demo " + want
				if len(items) != 2 || items[1].(map[string]any)["path"] != "/test/SKILL.md" || items[1].(map[string]any)["type"] != "skill" {
					panic("скилл не передан отдельным input")
				}
			}
			if items[0].(map[string]any)["text"] != want || p["threadId"] != "thread-1" || !runtimeMatches(p, true) || p["approvalPolicy"] != "on-request" || p["approvalsReviewer"] != "auto_review" {
				panic("искажены команда, ID чата или автономность turn")
			}
			// Ответ должен сохранить строковый ID, не принять его за ответ turn/start.
			send(map[string]any{"id": "clock", "method": "currentTime/read", "params": map[string]any{}})
			var clock struct {
				ID     string
				Result struct{ CurrentTimeAt int64 }
			}
			if input.Decode(&clock) != nil || clock.ID != "clock" || clock.Result.CurrentTimeAt == 0 {
				panic("нет корректного ответа currentTime/read")
			}
			finish := func(thread, turn, status string) {
				turnData := map[string]any{"id": turn, "status": status}
				if status == "failed" {
					// Разные сообщения доказывают, что клиент не связывает с результатом
					// ошибку чужого чата или другого turn из общего потока событий.
					turnData["error"] = map[string]any{
						"message":           thread + "/" + turn + " failed",
						"codexErrorInfo":    "usageLimitExceeded",
						"additionalDetails": "retry after usage reset",
					}
				}
				send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": thread, "turn": turnData}})
			}
			finish("other-thread", "turn-1", "failed")
			finish("thread-1", "other-turn", "failed")
			if scenario == "early" {
				finish("thread-1", "turn-1", "completed")
			}
			reply(map[string]any{"turn": map[string]any{"id": "turn-1"}})
			send(map[string]any{"method": "turn/started", "params": map[string]any{}})
			if scenario == "interrupt" {
				// Turn остаётся активным, пока тот же stdio-клиент не отправит
				// turn/interrupt. Возврат в общий цикл важен: второй процесс или
				// thread/resume в этом сценарии не участвуют.
				continue
			}
			if scenario == "approval" || scenario == "handled" || scenario == "unsupported-request" {
				// Даже неожиданный ручной запрос при auto_review нельзя подтвердить молча.
				method := "item/commandExecution/requestApproval"
				if scenario == "unsupported-request" {
					method = "account/chatgptAuthTokens/refresh"
				}
				send(map[string]any{"id": 900, "method": method, "params": map[string]any{}})
				var decision struct {
					ID     int
					Result struct{ Decision string }
				}
				if err := input.Decode(&decision); err == io.EOF {
					return // Клиент без обработчика обязан остановиться, не подтвердить.
				}
				if decision.ID != 900 || decision.Result.Decision != "decline" {
					panic("решение обработчика изменено")
				}
			}
			if scenario == "dynamic" {
				send(map[string]any{"id": "tool-request", "method": "item/tool/call", "params": map[string]any{
					"threadId": "thread-1", "turnId": "turn-1", "callId": "call-1", "namespace": nil,
					"tool": "run_child", "arguments": map[string]any{"parentRun": "parent-1"},
				}})
				var response struct {
					ID     string
					Result struct {
						Success      bool
						ContentItems []struct{ Type, Text string }
					}
				}
				if input.Decode(&response) != nil || response.ID != "tool-request" || !response.Result.Success ||
					len(response.Result.ContentItems) != 1 || response.Result.ContentItems[0].Type != "inputText" ||
					response.Result.ContentItems[0].Text != `{"runId":"child-1"}` {
					panic("dynamic tool не вернул структурированный результат")
				}
			}
			status := "completed"
			if scenario == "failed" || scenario == "interrupted" {
				status = scenario
			}
			if strings.HasPrefix(scenario, "continue:") {
				status = strings.TrimPrefix(scenario, "continue:")
			}
			if scenario == "unknown-status" {
				status = "new-unknown-status"
			}
			finish("thread-1", "turn-1", status)
		case "turn/interrupt":
			if scenario != "interrupt" || p["threadId"] != "thread-1" || p["turnId"] != "turn-1" {
				panic("искажён адрес отменяемого turn")
			}
			reply(map[string]any{})
			send(map[string]any{"method": "turn/completed", "params": map[string]any{
				"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "interrupted"},
			}})
		case "thread/read":
			requestedThreadID, _ := p["threadId"].(string)
			multiple := scenario == "inspect:multiple"
			if (!multiple && requestedThreadID != "thread-1") || p["includeTurns"] != true {
				panic("искажён запрос чтения чата")
			}
			if multiple && requestedThreadID != "thread-1" && requestedThreadID != "thread-2" && requestedThreadID != "thread-3" {
				panic("общая сессия прочитала неожиданный чат")
			}
			cwd, _ := os.Getwd()
			threadID, threadCWD, threadStatus := "thread-1", cwd, "idle"
			if multiple {
				threadID = requestedThreadID
			}
			turnStatus, activeFlags := "completed", []string{}
			switch scenario {
			case "inspect:active":
				threadStatus, turnStatus = "active", "inProgress"
			case "inspect:waiting":
				threadStatus, turnStatus, activeFlags = "active", "inProgress", []string{"waitingOnApproval"}
			case "inspect:user-input":
				threadStatus, turnStatus, activeFlags = "active", "inProgress", []string{"waitingOnUserInput"}
			case "inspect:failed":
				turnStatus = "failed"
			case "inspect:interrupted":
				turnStatus = "interrupted"
			case "inspect:empty":
				turnStatus = ""
			case "inspect:system-error":
				threadStatus, turnStatus = "systemError", ""
			case "inspect:wrong-thread":
				threadID = "other-thread"
			case "inspect:wrong-cwd":
				threadCWD = filepath.Dir(cwd)
			case "inspect:unknown-thread":
				threadStatus = "future-status"
			case "inspect:unknown-turn":
				turnStatus = "future-status"
			case "inspect:unknown-flag":
				threadStatus, turnStatus, activeFlags = "active", "inProgress", []string{"future-flag"}
			}
			turns := []any{}
			if turnStatus != "" {
				turn := map[string]any{"id": "turn-1", "status": turnStatus, "items": []any{}}
				if turnStatus == "failed" {
					turn["error"] = map[string]any{"message": "failed"}
				}
				turns = append(turns, turn)
			}
			reply(map[string]any{"thread": map[string]any{
				"id": threadID, "cwd": threadCWD, "status": map[string]any{"type": threadStatus, "activeFlags": activeFlags}, "turns": turns,
			}})
		default:
			panic("неожиданный метод: " + m.Method)
		}
	}
}

// TestRun проверяет бизнес-результат отдельно от ошибок клиента. Потерянные
// ответы, отказ сохранения ID и отмена никогда не приводят ко второму запросу.
func TestRun(t *testing.T) {
	for _, tc := range []struct {
		name, status   string
		creates, turns int
	}{
		{"ok", "completed", 1, 1}, {"early", "completed", 1, 1},
		{"settings-run", "completed", 1, 1},
		{"skill", "completed", 1, 1}, {"permissions", "completed", 1, 1}, {"handled", "completed", 1, 1}, {"dynamic", "completed", 1, 1},
		{"failed", "failed", 1, 1}, {"interrupted", "interrupted", 1, 1},
		{"error:initialize", "", 0, 0}, {"eof:thread/start", "", 1, 0},
		{"missing-id", "", 1, 0}, {"save", "", 1, 0},
		{"error:thread/name/set", "", 1, 0},
		{"error:turn/start", "", 1, 1}, {"eof:turn/start", "", 1, 1},
		{"malformed", "", 1, 1}, {"unknown-status", "", 1, 1},
		{"approval", "", 1, 1}, {"unsupported-request", "", 1, 1},
		{"notify", "", 1, 1}, {"cancel", "", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LAWA_TEST_CODEX_SERVER", tc.name)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var trace bytes.Buffer
			failure := errors.New("callback failure")
			saved := ""
			command := Command{Executable: binary, CWD: t.TempDir(), Text: "literal '$()`\\n", Title: "Test", Sandbox: "read-only", Stderr: &trace,
				OnThread: func(id string) error {
					saved = id
					if tc.name == "save" {
						return failure
					}
					return nil
				}, Notify: func(e Event) error {
					if e.Method == "turn/started" {
						if tc.name == "cancel" {
							cancel()
						}
						if tc.name == "notify" {
							return failure
						}
					}
					return nil
				}}
			if tc.name == "skill" {
				command.Skill = &Skill{"demo", "/test/SKILL.md"}
			}
			if tc.name == "settings-run" {
				command.Model, command.Effort, command.ServiceTier = "gpt-test", "high", "fast"
			}
			if tc.name == "dynamic" {
				command.DynamicTools = []DynamicTool{
					{Name: "run_child", Description: "Создать один", InputSchema: json.RawMessage(`{"type":"object"}`)},
					{Name: "run_children", Description: "Создать несколько", InputSchema: json.RawMessage(`{"type":"object"}`)},
				}
				command.CallDynamicTool = func(_ context.Context, call DynamicToolCall) (string, error) {
					var arguments struct{ ParentRun string }
					if err := json.Unmarshal(call.Arguments, &arguments); err != nil || call.ThreadID != "thread-1" || call.TurnID != "turn-1" ||
						call.CallID != "call-1" || call.Tool != "run_child" || arguments.ParentRun != "parent-1" {
						return "", errors.New("искажён вызов dynamic tool")
					}
					return `{"runId":"child-1"}`, nil
				}
			}
			if tc.name == "permissions" {
				command.Sandbox = ""
				command.Permissions = &PermissionProfile{Name: "lawa-test", ReadPaths: []string{"/run dir"}, WritePaths: []string{"/run dir/own.md"}}
			}
			if tc.name == "handled" || tc.name == "unsupported-request" {
				command.Respond = func(_ context.Context, event Event) (any, error) {
					switch event.Method {
					case "item/commandExecution/requestApproval":
						return map[string]string{"decision": "decline"}, nil
					default:
						return nil, &InteractionRequired{Event: event}
					}
				}
			}
			result, err := Run(ctx, command)
			if (err != nil) != (tc.status == "") || result.Status != tc.status {
				t.Fatalf("результат %+v, ошибка %v; сервер: %s", result, err, &trace)
			}
			if tc.status == "failed" {
				if result.TurnError == nil || result.TurnError.Message != "thread-1/turn-1 failed" ||
					string(result.TurnError.CodexErrorInfo) != `"usageLimitExceeded"` || result.TurnError.AdditionalDetails == nil ||
					*result.TurnError.AdditionalDetails != "retry after usage reset" {
					t.Fatalf("потеряна причина failed: %+v", result.TurnError)
				}
			} else if result.TurnError != nil {
				t.Fatalf("ошибка чужого или успешного turn попала в результат: %+v", result.TurnError)
			}
			if strings.Count(trace.String(), "thread/start\n") != tc.creates || strings.Count(trace.String(), "turn/start\n") != tc.turns ||
				result.CreationAttempted != (tc.creates > 0) || result.TurnAttempted != (tc.turns > 0) || result.ThreadID != saved {
				t.Fatalf("потеря связи или повтор: %+v; сервер: %s", result, &trace)
			}
			var rpc *RPCError
			var interaction *InteractionRequired
			if strings.HasPrefix(tc.name, "error:") && !errors.As(err, &rpc) ||
				(tc.name == "approval" || tc.name == "unsupported-request") && !errors.As(err, &interaction) ||
				tc.name == "cancel" && !errors.Is(err, context.Canceled) || (tc.name == "save" || tc.name == "notify") && !errors.Is(err, failure) {
				t.Fatalf("потерян тип ошибки: %v", err)
			}
			if tc.name == "unsupported-request" && interaction.Event.Method != "account/chatgptAuthTokens/refresh" {
				t.Fatalf("потерян неподдерживаемый метод: %+v", interaction.Event)
			}
			if strings.HasPrefix(tc.name, "error:") {
				var data struct{ Retryable bool }
				if decodeErr := json.Unmarshal(rpc.Data, &data); decodeErr != nil || !data.Retryable {
					t.Fatalf("потеряны структурированные детали RPC: %s, %v", rpc.Data, decodeErr)
				}
				if strings.Contains(err.Error(), "retryable") {
					t.Fatalf("RPC data не должны автоматически попадать в лог: %v", err)
				}
			}
		})
	}
}

// TestProcessLifecycle проверяет, что фасад observability получает настоящий PID
// и подтверждённый exit дочернего App Server, не читая таблицу процессов извне.
func TestProcessLifecycle(t *testing.T) {
	t.Setenv("LAWA_TEST_CODEX_SERVER", "ok")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var events []ProcessEvent
	var trace bytes.Buffer
	result, err := Run(t.Context(), Command{
		Executable: binary, CWD: t.TempDir(), Text: "literal '$()`\\n", Sandbox: "read-only", Stderr: &trace,
		OnProcess: func(event ProcessEvent) error {
			events = append(events, event)
			return nil
		},
	})
	if err != nil || result.Status != "completed" {
		t.Fatalf("run не завершён: %+v, %v; server: %s", result, err, &trace)
	}
	if len(events) != 2 || events[0].Kind != "started" || events[0].PID <= 0 ||
		events[1].Kind != "exited" || events[1].PID != events[0].PID || events[1].ExitCode == nil || *events[1].ExitCode != 0 || events[1].Signal != "" {
		t.Fatalf("неверный lifecycle процесса: %+v", events)
	}
}

// TestContinue проверяет, что сохранённый чат сначала возобновляется, затем
// получает ровно один новый turn с текстом continue. Все три терминальных статуса
// сохраняются без создания нового thread и без интерпретации failed как RPC-сбоя.
func TestContinue(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			t.Setenv("LAWA_TEST_CODEX_SERVER", "continue:"+status)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			var trace bytes.Buffer
			turnID := ""
			result, err := Continue(t.Context(), "thread-1", Command{
				Executable: binary, CWD: t.TempDir(), Text: "continue", Sandbox: "read-only", Stderr: &trace,
				OnTurn: func(id string, _ func(context.Context) error) error { turnID = id; return nil },
			})
			if err != nil || result.ThreadID != "thread-1" || result.TurnID != "turn-1" || result.Status != status || turnID != "turn-1" {
				t.Fatalf("неверный результат continue: %+v, callback=%q, ошибка=%v; сервер: %s", result, turnID, err, &trace)
			}
			if strings.Count(trace.String(), "thread/resume\n") != 1 || strings.Count(trace.String(), "thread/start\n") != 0 || strings.Count(trace.String(), "turn/start\n") != 1 {
				t.Fatalf("continue создал новый чат или повторил turn: %s", &trace)
			}
		})
	}
}

// TestContinueRuntimeSettings проверяет, что resume и новый turn получают те же
// явные override. Иначе перезапуск Lawa мог бы незаметно вернуть настройки Codex.
func TestContinueRuntimeSettings(t *testing.T) {
	t.Setenv("LAWA_TEST_CODEX_SERVER", "settings-continue")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := Continue(t.Context(), "thread-1", Command{
		Executable: binary, CWD: t.TempDir(), Text: "continue", Sandbox: "read-only",
		Model: "gpt-test", Effort: "high", ServiceTier: "fast",
	})
	if err != nil || result.Status != "completed" || result.ThreadID != "thread-1" || result.TurnID != "turn-1" {
		t.Fatalf("настройки продолжения не прошли через протокол: %+v, %v", result, err)
	}
}

// TestRunInterruptUsesOwningSession воспроизводит живой конфликт active writer:
// пока исходный app-server выполняет turn, interrupt обязан пройти через его же
// stdio. Один initialize и отсутствие thread/resume доказывают, что второй
// процесс не запускался; терминальный interrupted приходит в исходный Run.
func TestRunInterruptUsesOwningSession(t *testing.T) {
	t.Setenv("LAWA_TEST_CODEX_SERVER", "interrupt")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var trace bytes.Buffer
	type outcome struct {
		result Result
		err    error
	}
	ready := make(chan func(context.Context) error, 1)
	done := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cwd := t.TempDir()
	go func() {
		result, runErr := Run(ctx, Command{
			Executable: binary, CWD: cwd, Text: "literal '$()`\\n", Sandbox: "read-only", Stderr: &trace,
			OnTurn: func(id string, interrupt func(context.Context) error) error {
				if id != "turn-1" {
					t.Errorf("неверный ID активного turn: %q", id)
				}
				ready <- interrupt
				return nil
			},
		})
		done <- outcome{result: result, err: runErr}
	}()
	var interrupt func(context.Context) error
	select {
	case interrupt = <-ready:
	case <-time.After(time.Second):
		t.Fatal("клиент не передал interrupt исходной сессии")
	}
	if err := interrupt(t.Context()); err != nil {
		t.Fatalf("interrupt исходной сессии: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.Status != "interrupted" || got.result.ThreadID != "thread-1" || got.result.TurnID != "turn-1" {
			t.Fatalf("исходный Run не получил interrupted: %+v, %v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("исходный Run не завершился после interrupt")
	}
	if strings.Count(trace.String(), "initialize\n") != 1 || strings.Count(trace.String(), "thread/start\n") != 1 ||
		strings.Count(trace.String(), "thread/resume\n") != 0 || strings.Count(trace.String(), "turn/interrupt\n") != 1 {
		t.Fatalf("interrupt открыл второй процесс или исказил RPC: %s", &trace)
	}
}

// TestIntegration запускается только явно: расходует запросы и оставляет
// видимый тестовый чат. Обычный go test использует только подставной сервер.
func TestIntegration(t *testing.T) {
	binary := os.Getenv("LAWA_CODEX_INTEGRATION_BIN")
	if binary == "" {
		t.Skip("нужно явное разрешение и LAWA_CODEX_INTEGRATION_BIN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Каталог Codex возвращает канонические пути: /var на macOS — симлинк.
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, ".agents", "skills", "lawa-client-probe", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: lawa-client-probe\ndescription: Integration probe\n---\nReply exactly LAWA_CLIENT_OK. You may read required instructions. Do not modify files or perform other actions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	answer := ""
	result, err := Run(ctx, Command{Executable: binary, CWD: cwd, Text: "Follow the selected skill. You may read required agent instructions and the selected skill. Do not modify files or perform other actions.",
		Title: "Lawa: тест Go-клиента", Sandbox: "read-only", Skill: &Skill{"lawa-client-probe", path},
		OnThread: func(id string) error { t.Logf("Тестовый чат: %s", id); return nil }, Notify: func(e Event) error {
			var p struct{ Item struct{ Type, Text string } }
			if e.Method == "item/completed" {
				if err := json.Unmarshal(e.Params, &p); err != nil {
					return err
				}
				if p.Item.Type == "agentMessage" {
					answer = p.Item.Text
				}
			}
			return nil
		}})
	if err != nil || result.Status != "completed" || result.ThreadID == "" || result.TurnID == "" || strings.TrimSpace(answer) != "LAWA_CLIENT_OK" {
		var interaction *InteractionRequired
		if errors.As(err, &interaction) {
			t.Logf("Запрос Codex без автоодобрения: %s %s", interaction.Event.Method, interaction.Event.Params)
		}
		t.Fatalf("реальный запуск: %+v, %v, ответ %q", result, err, answer)
	}
	t.Logf("Завершённый turn: %s", result.TurnID)
}

// TestInspect проверяет консервативное сопоставление истории и живого статуса.
// Особенно важен приоритет active: старый successful turn не должен открыть
// зависимости, если пользователь уже начал ручное продолжение того же чата.
func TestInspect(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		scenario string
		want     WorkStatus
		wantErr  bool
	}{
		{"inspect:completed", WorkCompleted, false},
		{"inspect:active", WorkRunning, false},
		{"inspect:waiting", WorkWaitingForApproval, false},
		{"inspect:user-input", WorkWaitingForApproval, false},
		{"inspect:failed", WorkFailed, false},
		{"inspect:interrupted", WorkInterrupted, false},
		{"inspect:empty", WorkUnknown, false},
		{"inspect:system-error", WorkFailed, false},
		{"inspect:wrong-thread", "", true},
		{"inspect:wrong-cwd", "", true},
		{"inspect:unknown-thread", "", true},
		{"inspect:unknown-turn", "", true},
		{"inspect:unknown-flag", "", true},
		{"error:thread/read", "", true},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			t.Setenv("LAWA_TEST_CODEX_SERVER", tc.scenario)
			observation, err := Inspect(t.Context(), Connection{Executable: binary, CWD: t.TempDir()}, "thread-1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("наблюдение: %+v, %v", observation, err)
			}
			if err == nil {
				status, statusErr := observation.Status()
				if statusErr != nil || status != tc.want || observation.ThreadID != "thread-1" {
					t.Fatalf("статус: %q, %v; наблюдение: %+v", status, statusErr, observation)
				}
			}
		})
	}
}

// TestObserverReusesSession доказывает R4 на настоящем stdio-транспорте теста:
// несколько thread/read идут через один initialize, то есть между проверками
// app-server не перезапускается. Активные turn этот Observer не создаёт.
func TestObserverReusesSession(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAWA_TEST_CODEX_SERVER", "inspect:multiple")
	trace := bytes.Buffer{}
	observer, err := OpenObserver(t.Context(), Connection{Executable: binary, CWD: t.TempDir(), Stderr: &trace})
	if err != nil {
		t.Fatal(err)
	}
	for _, threadID := range []string{"thread-1", "thread-2", "thread-3"} {
		observation, inspectErr := observer.Inspect(threadID)
		if inspectErr != nil || observation.LatestTurnStatus != "completed" {
			t.Fatalf("повторное чтение: %+v, %v", observation, inspectErr)
		}
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(trace.String(), "initialize\n"); got != 1 {
		t.Fatalf("наблюдение запустило %d сессий вместо одной; trace=%q", got, trace.String())
	}
	if got := strings.Count(trace.String(), "thread/read\n"); got != 3 {
		t.Fatalf("ожидались три чтения через общую сессию, получено %d; trace=%q", got, trace.String())
	}
}

// TestCheck доказывает, что preflight ограничивается рукопожатием и не создаёт
// chat/turn. Подставной сервер завершится после закрытия stdin клиентом.
func TestCheck(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAWA_TEST_CODEX_SERVER", "check")
	if err := Check(t.Context(), Connection{Executable: binary, CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

// TestInvalidInput запрещает побочные эффекты при неверном вводе и до отмены.
func TestInvalidInput(t *testing.T) {
	for _, command := range []Command{
		{CWD: t.TempDir()}, {CWD: t.TempDir(), Text: "\xff"}, {CWD: ".", Text: "test"},
		{CWD: filepath.Join(t.TempDir(), "missing"), Text: "test"},
		{CWD: t.TempDir(), Text: "test", Skill: &Skill{"bad name", "/test/SKILL.md"}},
		{CWD: t.TempDir(), Text: "test", Sandbox: "read-only", Permissions: &PermissionProfile{Name: "test", ReadPaths: []string{"/run"}, WritePaths: []string{"/run/own"}}},
		{CWD: t.TempDir(), Text: "test", Permissions: &PermissionProfile{Name: "bad.name", ReadPaths: []string{"/run"}, WritePaths: []string{"/run/own"}}},
		{CWD: t.TempDir(), Text: "test", Permissions: &PermissionProfile{Name: "test", ReadPaths: []string{"relative"}, WritePaths: []string{"/run/own"}}},
		{CWD: t.TempDir(), Text: "test", Model: "bad model"},
		{CWD: t.TempDir(), Text: "test", Effort: "very high"},
		{CWD: t.TempDir(), Text: "test", ServiceTier: "fast mode"},
		{CWD: t.TempDir(), Text: "test", DynamicTools: []DynamicTool{{Name: "tool", Description: "Описание", InputSchema: json.RawMessage(`{"type":"object"}`)}}},
		{CWD: t.TempDir(), Text: "test", DynamicTools: []DynamicTool{{Name: "tool", Description: "Описание", InputSchema: json.RawMessage(`{"type":"string"}`)}}, CallDynamicTool: func(context.Context, DynamicToolCall) (string, error) { return "ok", nil }},
	} {
		result, err := Run(context.Background(), command)
		if err == nil || result.CreationAttempted {
			t.Fatalf("запуск с неверным вводом: %+v, %v", result, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := Run(ctx, Command{CWD: t.TempDir(), Text: "test"})
	if !errors.Is(err, context.Canceled) || result.CreationAttempted {
		t.Fatalf("отменённый запуск: %+v, %v", result, err)
	}
}
