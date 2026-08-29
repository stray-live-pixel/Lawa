package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stray-live-pixel/flows-2/internal/codex"
	"github.com/stray-live-pixel/flows-2/internal/coordinator"
	"github.com/stray-live-pixel/flows-2/internal/runstore"
	"github.com/stray-live-pixel/flows-2/internal/scheduler"
	"github.com/stray-live-pixel/flows-2/internal/workflow"
)

// TestCLI проверяет справку без Codex, аргументы и валидацию настоящих файлов.
// Ошибочный ввод не должен печатать сообщение об успешной проверке.
func TestCLI(t *testing.T) {
	invalid := filepath.Join(t.TempDir(), "invalid.json")
	content := []byte(`{"id":"broken","steps":[]}`)
	if err := os.WriteFile(invalid, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"без аргументов", nil, "Команды:"},
		{"справка", []string{"help"}, "Команды:"},
		{"короткая справка", []string{"-h"}, "Команды:"},
		{"длинная справка", []string{"--help"}, "Команды:"},
		{"инструкция скилла", []string{"skill"}, "# Lawa: запуск workflow из чата Codex"},
		{"пример", []string{"validate", "../../examples/review.json"}, `Workflow "review" корректен; шагов: 4.`},
		{"неверный граф", []string{"validate", invalid}, ""},
		{"нет файла", []string{"validate", invalid + ".missing"}, ""},
		{"папка вместо файла", []string{"validate", filepath.Dir(invalid)}, ""},
		{"нет пути", []string{"validate"}, ""},
		{"лишний аргумент", []string{"validate", invalid, "extra"}, ""},
		{"несуществующая команда", []string{"unknown"}, ""},
		{"run не маскируется проверкой", []string{"run", invalid}, ""},
		{"лишний аргумент скилла", []string{"skill", "extra"}, ""},
		{"лишний аргумент справки", []string{"help", "extra"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := execute(tc.args, &out)
			if tc.want == "" {
				if err == nil || out.Len() != 0 {
					t.Fatalf("ожидалась только ошибка: %v, %q", err, out.String())
				}
			} else if err != nil || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("неожиданный результат: %v, %q", err, out.String())
			}
		})
	}
	if after, err := os.ReadFile(invalid); err != nil || !bytes.Equal(after, content) {
		t.Fatalf("проверка изменила исходный файл: %v", err)
	}
}

// failingWriter позволяет проверить ошибку вывода без реального закрытого канала.
type failingWriter struct{ err error }

// Write имитирует отказ приёмника до записи первого байта.
func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// TestOutputError не позволяет объявить успех, когда результат не удалось вывести.
func TestOutputError(t *testing.T) {
	failure := errors.New("ошибка вывода")
	for _, args := range [][]string{{"help"}, {"skill"}, {"validate", "../../examples/review.json"}} {
		if err := execute(args, failingWriter{failure}); !errors.Is(err, failure) {
			t.Fatalf("ошибка вывода потеряна: %v", err)
		}
	}
}

// TestPrintStatusEscapesTerminalControlCharacters закрывает R5: все внешние ID
// остаются узнаваемыми, но управляющие символы видны как текст и не меняют строки,
// цвет или положение курсора в терминале пользователя.
func TestPrintStatusEscapesTerminalControlCharacters(t *testing.T) {
	values := []string{
		"cube\nложный успех\r\x1b[2J",
		"thread\r\n",
		"codex\x1b[31m\n",
		"waiting\nподмена",
		"run\r\x1b[0m",
	}
	status := coordinator.Status{
		RunID: values[4],
		Steps: []coordinator.StepStatus{{
			ID: values[0], ThreadID: values[1], CodexThreadID: values[2], State: scheduler.Succeeded,
		}},
		Waiting:  []string{values[3]},
		Complete: true,
	}
	var out bytes.Buffer
	if err := printStatus(&out, status); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, value := range values {
		if quoted := strconv.QuoteToGraphic(value); !strings.Contains(got, quoted) {
			t.Errorf("ID не выведен безопасным литералом %q: %q", quoted, got)
		}
	}
	if strings.ContainsAny(got, "\r\x1b") || strings.Count(got, "\n") != 3 {
		t.Fatalf("управляющие символы изменили терминальный вывод: %q", got)
	}
}

// TestSkillInstruction фиксирует обязательные части пользовательского сценария.
// Это защищает инструкцию от незаметного превращения в общий обзор: она должна
// оставаться пригодной для запуска, наблюдения и безопасного resume из одного чата.
func TestSkillInstruction(t *testing.T) {
	// Отдельный файл является исходником инструкции, а команда должна печатать его
	// побайтно. Проверка защищает CLI от случайной обработки, обрезки или добавления
	// служебного текста вокруг готового SKILL.md.
	want, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := execute([]string{"skill"}, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Error("lawa skill изменяет содержимое встроенного SKILL.md")
	}
	// Проектная установка делает /lawa доступным в этом репозитории без ручного
	// шага. Точное равенство не позволяет установленной и встроенной версиям
	// незаметно разойтись при изменении пользовательского контракта.
	installed, err := os.ReadFile(filepath.Join("..", "..", ".agents", "skills", "lawa", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installed, want) {
		t.Error("проектный скилл /lawa расходится с выводом lawa skill")
	}

	// Метаданные идут первыми, чтобы stdout можно было без ручной обработки
	// сохранить в SKILL.md. Точное сравнение не пропустит текст перед frontmatter
	// или потерю обязательных полей, из-за которых Codex не распознает скилл.
	const metadata = "---\nname: lawa\ndescription: \"Запуск и продолжение workflow Lawa из чата Codex.\"\n---\n\n"
	if !strings.HasPrefix(skillInstruction, metadata) {
		t.Errorf("инструкция не начинается с обязательных метаданных SKILL.md: %q", skillInstruction)
	}
	// Пример проверяет тот же production-валидатор, что и команда validate. Так
	// документация не сможет предлагать неизвестные поля, потерянные зависимости
	// или цикл после будущего изменения контракта workflow.
	const exampleStart, exampleEnd = "~~~json\n", "\n~~~"
	example := strings.SplitN(skillInstruction, exampleStart, 2)
	if len(example) != 2 {
		t.Fatal("в инструкции отсутствует JSON-пример workflow")
	}
	example = strings.SplitN(example[1], exampleEnd, 2)
	if len(example) != 2 {
		t.Fatal("JSON-пример workflow не завершён")
	}
	if _, err := workflow.Decode(strings.NewReader(example[0])); err != nil {
		t.Errorf("инструкция содержит невалидный пример workflow: %v", err)
	}
	for _, fragment := range []string{
		"есть только бинарник lawa",
		"явной просьбы пользователя",
		"lawa validate <workflow.json>",
		`"dependsOn": ["architecture", "security"]`,
		"Не добавляй неизвестные поля",
		"прямые и\nкосвенные циклы",
		"lawa run <workflow.json>",
		"--task-file <файл-постановки>",
		"--initiator-thread-id <id-этого-чата>",
		"не интерполируя пользовательский текст в shell",
		"lawa resume <run-id>",
		"Resume сам отправляет один turn `continue`",
		"Failed-чат пользователь продолжает",
		"прерывают активные turn через Codex",
		"ID\nтекущего чата недоступен",
		"memory/<threadId>.md",
		"изменять только собственный",
		"https://github.com/stray-live-pixel/flows-2",
		"https://raw.githubusercontent.com/stray-live-pixel/flows-2/main/product/1.md",
		"Если версия\nизвестна из источника установки",
		"Если нет — не угадывай",
		"его SHA-256 и полный вывод lawa help",
		"получи явное разрешение перед публичной",
		"https://github.com/stray-live-pixel/flows-2/issues/new",
	} {
		if !strings.Contains(skillInstruction, fragment) {
			t.Errorf("в инструкции отсутствует обязательный фрагмент %q", fragment)
		}
	}
}

type cliFakeClient struct {
	mu        sync.Mutex
	runs      map[string]int
	continues map[string]int
	inspect   map[string]codex.WorkStatus
}

type cliFakeObserver struct{ client *cliFakeClient }

func newCLIFakeClient() *cliFakeClient {
	return &cliFakeClient{runs: map[string]int{}, continues: map[string]int{}, inspect: map[string]codex.WorkStatus{}}
}

func (c *cliFakeClient) Run(_ context.Context, command codex.Command) (codex.Result, error) {
	parts := strings.SplitN(command.Title, " / ", 2)
	stepID := strings.SplitN(parts[1], " [", 2)[0]
	threadID := "chat-" + stepID
	c.mu.Lock()
	c.runs[stepID]++
	c.mu.Unlock()
	if err := command.OnThread(threadID); err != nil {
		return codex.Result{ThreadID: threadID, CreationAttempted: true}, err
	}
	if command.OnTurn != nil {
		command.OnTurn("turn-"+stepID, func(context.Context) error { return nil })
	}
	if err := command.Notify(codex.Event{Method: "turn/started"}); err != nil {
		return codex.Result{ThreadID: threadID, CreationAttempted: true, TurnAttempted: true}, err
	}
	return codex.Result{ThreadID: threadID, TurnID: "turn-" + stepID, Status: "completed", CreationAttempted: true, TurnAttempted: true}, nil
}

func (c *cliFakeClient) Continue(_ context.Context, threadID string, command codex.Command) (codex.Result, error) {
	if command.Text != "continue" {
		return codex.Result{ThreadID: threadID}, errors.New("resume передал неверный текст")
	}
	c.mu.Lock()
	c.continues[threadID]++
	c.inspect[threadID] = codex.WorkCompleted
	c.mu.Unlock()
	if command.OnTurn != nil {
		command.OnTurn("continued-turn", func(context.Context) error { return nil })
	}
	if err := command.Notify(codex.Event{Method: "turn/started"}); err != nil {
		return codex.Result{ThreadID: threadID, TurnID: "continued-turn", TurnAttempted: true}, err
	}
	return codex.Result{ThreadID: threadID, TurnID: "continued-turn", Status: "completed", TurnAttempted: true}, nil
}

func (c *cliFakeClient) OpenObserver(_ context.Context, _ string) (coordinator.Observer, error) {
	return &cliFakeObserver{client: c}, nil
}

func (o *cliFakeObserver) Inspect(threadID string) (codex.Observation, error) {
	c := o.client
	c.mu.Lock()
	status := c.inspect[threadID]
	c.mu.Unlock()
	if status == "" {
		status = codex.WorkCompleted
	}
	observation := codex.Observation{ThreadID: threadID, ThreadStatus: "idle", LatestTurnID: "turn-1"}
	switch status {
	case codex.WorkCompleted:
		observation.LatestTurnStatus = "completed"
	case codex.WorkFailed:
		observation.LatestTurnStatus = "failed"
	case codex.WorkInterrupted:
		observation.LatestTurnStatus = "interrupted"
	}
	return observation, nil
}

func (o *cliFakeObserver) Close() error { return nil }

func cliTestDependencies(client coordinator.Client, check func(context.Context, codex.Connection) error) dependencies {
	return dependencies{
		check:        check,
		client:       func(string, io.Writer) coordinator.Client { return client },
		pollInterval: time.Millisecond,
		userHomeDir: func() (string, error) {
			return "", errors.New("home не должен использоваться при --root")
		},
	}
}

// TestRunCommand проверяет полный локальный путь CLI: preflight предшествует
// созданию run, вход сохраняется, шаг запускается, а пользователь видит ID и итог.
func TestRunCommand(t *testing.T) {
	root, cwd := filepath.Join(t.TempDir(), "runs"), t.TempDir()
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	taskPath, commentPath := filepath.Join(t.TempDir(), "task.md"), filepath.Join(t.TempDir(), "comment.md")
	workflowJSON := []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"Сделай","dependsOn":[]}]}`)
	if err := os.WriteFile(workflowPath, workflowJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskPath, []byte("Финальная задача\nс `$()`"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commentPath, []byte("Срочно"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newCLIFakeClient()
	checks := 0
	deps := cliTestDependencies(client, func(_ context.Context, connection codex.Connection) error {
		checks++
		if connection.CWD != cwd || connection.Executable != "/test/codex" {
			t.Fatalf("искажён preflight: %+v", connection)
		}
		return nil
	})
	var out bytes.Buffer
	err := executeContext(t.Context(), []string{
		"run", workflowPath, "--cwd", cwd, "--task-file", taskPath, "--comment-file", commentPath,
		"--initiator-thread-id", "initiator", "--root", root, "--codex", "/test/codex",
	}, &out, io.Discard, deps)
	if err != nil {
		t.Fatal(err)
	}
	if checks != 1 || !strings.Contains(out.String(), "runId:") || !strings.Contains(out.String(), "успешно завершён") {
		t.Fatalf("нет preflight или понятного результата: checks=%d, out=%q", checks, out.String())
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ожидался один run: %v, %v", entries, err)
	}
	snapshot, err := runstore.Load(root, entries[0].Name())
	if err != nil || snapshot.Meta.InitiatorThreadID != "initiator" || snapshot.Meta.Steps[0].State != scheduler.Succeeded ||
		snapshot.Meta.Steps[0].CodexThreadID != "chat-step" || !strings.Contains(snapshot.Task, "Финальная задача") || !strings.Contains(snapshot.Task, "Срочно") {
		t.Fatalf("неверно сохранён run: %+v, %v", snapshot, err)
	}
}

// TestRunPreflightFailureLeavesNoRun защищает порядок побочных эффектов: при
// недоступном app-server пользователь может повторить run без мусора и дублей.
func TestRunPreflightFailureLeavesNoRun(t *testing.T) {
	parent, cwd := t.TempDir(), t.TempDir()
	root := filepath.Join(parent, "runs")
	workflowPath := filepath.Join(parent, "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"Сделай","dependsOn":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("Codex unavailable")
	deps := cliTestDependencies(newCLIFakeClient(), func(context.Context, codex.Connection) error { return failure })
	err := executeContext(t.Context(), []string{
		"run", workflowPath, "--cwd", cwd, "--task", "Задача", "--initiator-thread-id", "initiator", "--root", root,
	}, io.Discard, io.Discard, deps)
	if !errors.Is(err, failure) {
		t.Fatalf("потеряна ошибка preflight: %v", err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("до успешного preflight создано хранилище: %v", statErr)
	}
}

// TestResumeCommand использует сохранённый interrupted-чат, отправляет ему один
// continue и после успеха запускает зависимый Pending-шаг без нового чата родителя.
func TestResumeCommand(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{"id":"chain","steps":[{"id":"child","type":"agent","prompt":"Итог","dependsOn":["parent"]},{"id":"parent","type":"agent","prompt":"Факты","dependsOn":[]}]}`),
		Task:         "Задача", CWD: cwd, InitiatorThreadID: "initiator",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Reserve([]string{"parent"}); err == nil {
		err = run.Update("parent", scheduler.Cancelled, "chat-parent")
	}
	if closeErr := run.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	client := newCLIFakeClient()
	client.inspect["chat-parent"] = codex.WorkInterrupted
	checks := 0
	deps := cliTestDependencies(client, func(context.Context, codex.Connection) error { checks++; return nil })
	var out bytes.Buffer
	if err = executeContext(t.Context(), []string{"resume", snapshot.Meta.RunID, "--root", root}, &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if checks != 1 || client.runs["parent"] != 0 || client.continues["chat-parent"] != 1 || client.runs["child"] != 1 || !strings.Contains(out.String(), "успешно завершён") {
		t.Fatalf("resume неверно продолжил чат: checks=%d, runs=%v, continues=%v, out=%q", checks, client.runs, client.continues, out.String())
	}
}

func TestArgumentParsingAndExitCodes(t *testing.T) {
	for _, args := range [][]string{
		{"workflow.json", "--cwd"},
		{"workflow.json", "--cwd", "/tmp", "--cwd", "/tmp", "--task", "x", "--initiator-thread-id", "i"},
		{"workflow.json", "--unknown", "x"},
		{"workflow.json", "--cwd", "/tmp", "--task", "x", "--task-file", "/tmp/task", "--initiator-thread-id", "i"},
		{"first", "second", "--cwd", "/tmp", "--task", "x", "--initiator-thread-id", "i"},
	} {
		if _, err := parseRunArguments(args); err == nil {
			t.Errorf("приняты неверные аргументы: %v", args)
		}
	}
	if parsed, err := parseRunArguments([]string{"--task=x", "workflow.json", "--initiator-thread-id=i", "--cwd=/tmp"}); err != nil || parsed.workflow != "workflow.json" {
		t.Fatalf("не приняты флаги до пути или --name=value: %+v, %v", parsed, err)
	}
	if exitCode(nil, 0) != 0 || exitCode(errors.New("x"), 0) != 2 || exitCode(context.Canceled, 2) != 130 || exitCode(context.Canceled, 15) != 143 {
		t.Fatal("неверные коды завершения")
	}
}
