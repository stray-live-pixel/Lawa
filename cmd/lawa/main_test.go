package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/series"
	"github.com/stray-live-pixel/Lawa/internal/statusreport"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
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
		{"версия", []string{"version"}, "dev"},
		{"пример", []string{"validate", "../../examples/review.json"}, `Workflow "review" корректен; шагов: 4.`},
		{"неверный граф", []string{"validate", invalid}, ""},
		{"нет файла", []string{"validate", invalid + ".missing"}, ""},
		{"папка вместо файла", []string{"validate", filepath.Dir(invalid)}, ""},
		{"нет пути", []string{"validate"}, ""},
		{"лишний аргумент", []string{"validate", invalid, "extra"}, ""},
		{"несуществующая команда", []string{"unknown"}, ""},
		{"run не маскируется проверкой", []string{"run", invalid}, ""},
		{"лишний аргумент скилла", []string{"skill", "extra"}, ""},
		{"лишний аргумент версии", []string{"version", "extra"}, ""},
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

// TestProductionDependenciesSeparateLocalAndChatIntervals не позволяет снова
// связать частое локальное обновление с дорогой публикацией в чат.
func TestProductionDependenciesSeparateLocalAndChatIntervals(t *testing.T) {
	dependencies := productionDependencies()
	if dependencies.refreshInterval != time.Minute {
		t.Fatalf("интервал локального отчёта: %s, ожидалось 1m", dependencies.refreshInterval)
	}
	if dependencies.chatInterval != 5*time.Minute {
		t.Fatalf("интервал чат-сводки: %s, ожидалось 5m", dependencies.chatInterval)
	}
}

// failingWriter позволяет проверить ошибку вывода без реального закрытого канала.
type failingWriter struct{ err error }

// Write имитирует отказ приёмника до записи первого байта.
func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// failOnWriteWriter пропускает служебные сообщения до заданной записи. Он нужен
// для воспроизведения отказа после публикации seriesId и создания первого run.
type failOnWriteWriter struct {
	writes, failAt int
	err            error
}

// Write возвращает настроенную ошибку ровно на выбранной операции вывода.
func (w *failOnWriteWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.err
	}
	return len(p), nil
}

// failOnTextWriter имитирует отказ только на сообщении с заданным текстом. Так
// тест финальной сводки не зависит от числа промежуточных heartbeat-записей.
type failOnTextWriter struct {
	text string
	err  error
}

// Write пропускает служебный вывод и отказывает на выбранной сводке.
func (w failOnTextWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.text) {
		return 0, w.err
	}
	return len(p), nil
}

// cliRendererFunc подставляет renderer в сквозные CLI-тесты без внешнего PlantUML.
type cliRendererFunc func(context.Context, []byte) ([]byte, error)

// Render реализует интерфейс statusreport.Renderer тестовой функцией.
func (f cliRendererFunc) Render(ctx context.Context, source []byte) ([]byte, error) {
	return f(ctx, source)
}

// successfulCLIRenderer возвращает минимальный результат с корректной PNG-сигнатурой.
func successfulCLIRenderer() statusreport.Renderer {
	return cliRendererFunc(func(context.Context, []byte) ([]byte, error) {
		return []byte("\x89PNG\r\n\x1a\ntest"), nil
	})
}

// TestOutputError не позволяет объявить успех, когда результат не удалось вывести.
func TestOutputError(t *testing.T) {
	failure := errors.New("ошибка вывода")
	for _, args := range [][]string{{"help"}, {"skill"}, {"validate", "../../examples/review.json"}} {
		if err := execute(args, failingWriter{failure}); !errors.Is(err, failure) {
			t.Fatalf("ошибка вывода потеряна: %v", err)
		}
	}
}

// TestStatusPublisherUpdatesFilesButThrottlesChat проверяет оба независимых
// интерфейса: каждый входной снимок обновляет Markdown и PNG, но в stdout попадают
// только первая, пятиминутная и финальная краткие сводки без деталей и картинки.
func TestStatusPublisherUpdatesFilesButThrottlesChat(t *testing.T) {
	runDir := t.TempDir()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	renders := 0
	renderer := cliRendererFunc(func(context.Context, []byte) ([]byte, error) {
		renders++
		return []byte("\x89PNG\r\n\x1a\ntest"), nil
	})
	var out bytes.Buffer
	publisher := newStatusPublisher(t.Context(), &out, runDir, renderer, 5*time.Minute, func() time.Time { return now })
	status := coordinator.Status{
		RunID: "run-1", WorkflowID: "flow",
		Steps: []coordinator.StepStatus{{ID: "cube", CodexThreadID: "chat-cube", State: scheduler.Starting}},
	}
	if err := publisher.Publish(status); err != nil {
		t.Fatal(err)
	}
	firstOutput := out.String()
	if !strings.Contains(firstOutput, "Всего: 1, готово: 0, работает: 1, ожидают: 0") || !strings.Contains(firstOutput, "vscode://file/") {
		t.Fatalf("первая краткая сводка неполна: %q", firstOutput)
	}

	now = now.Add(time.Minute)
	status.Steps[0].State = scheduler.Running
	if err := publisher.Publish(status); err != nil {
		t.Fatal(err)
	}
	if out.String() != firstOutput {
		t.Fatalf("изменение состояния преждевременно попало в чат: %q", out.String())
	}
	report, err := os.ReadFile(filepath.Join(runDir, statusreport.ReportFilename))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(report); !strings.Contains(text, "[cube](codex://threads/chat-cube) — running") || !strings.Contains(text, "![Текущая схема workflow](workflow-status.png)") {
		t.Fatalf("локальный подробный отчёт не обновлён: %q", text)
	}
	if renders != 2 {
		t.Fatalf("визуализация обновлена %d раз, ожидалось 2", renders)
	}

	now = now.Add(4 * time.Minute)
	if err := publisher.Publish(status); err != nil {
		t.Fatal(err)
	}
	status.Steps[0].State = scheduler.Succeeded
	status.Complete = true
	now = now.Add(time.Second)
	if err := publisher.Publish(status); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "Всего:") != 3 || !strings.Contains(got, "Run run-1 успешно завершён") {
		t.Fatalf("пятиминутная или финальная сводка потеряна: %q", got)
	}
	for _, forbidden := range []string{"cube —", "codex://threads/", "workflow-status.png", "PlantUML image"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("в чат попала лишняя деталь %q: %q", forbidden, got)
		}
	}
}

// TestStatusPublisherKeepsWorkflowAliveWithoutPlantUML фиксирует границу отказа:
// renderer может отсутствовать, но подробный Markdown и короткая чат-сводка
// остаются доступны; координатору возвращается только ошибка самого stdout.
func TestStatusPublisherKeepsWorkflowAliveWithoutPlantUML(t *testing.T) {
	runDir := t.TempDir()
	status := coordinator.Status{
		WorkflowID: "flow",
		Steps:      []coordinator.StepStatus{{ID: "cube", State: scheduler.Running}},
	}
	renderFailure := errors.New("plantuml не установлен")
	brokenRenderer := cliRendererFunc(func(context.Context, []byte) ([]byte, error) {
		return nil, renderFailure
	})
	var out bytes.Buffer
	publisher := newStatusPublisher(t.Context(), &out, runDir, brokenRenderer, 5*time.Minute, nil)
	if err := publisher.Publish(status); err != nil {
		t.Fatalf("ошибка визуализации остановила статус: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Всего: 1") || !strings.Contains(got, renderFailure.Error()) || strings.Contains(got, "cube — running") {
		t.Fatalf("краткая диагностика renderer неверна: %q", got)
	}
	if _, err := os.Stat(filepath.Join(runDir, statusreport.SourceFilename)); err != nil {
		t.Fatalf("source не сохранён при отказе renderer: %v", err)
	}
	report, err := os.ReadFile(filepath.Join(runDir, statusreport.ReportFilename))
	if err != nil || !strings.Contains(string(report), "cube — running") || !strings.Contains(string(report), "Схема PlantUML не обновлена") {
		t.Fatalf("подробный Markdown потерян при отказе renderer: %v, %q", err, report)
	}
	outputFailure := errors.New("stdout unavailable")
	brokenOutput := newStatusPublisher(t.Context(), failingWriter{outputFailure}, runDir, statusreport.Renderer(brokenRenderer), 5*time.Minute, nil)
	if err := brokenOutput.Publish(status); !errors.Is(err, outputFailure) {
		t.Fatalf("ошибка пользовательского вывода не остановила координатор: %v", err)
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
		"не оставляй команду без наблюдения дольше 60 секунд",
		"не реже раза в минуту",
		"не чаще раза в 5 минут",
		"не прикладывай PNG в чат",
		"Не отправляй ради статуса новые turn",
		"ведёт в точный чат кубика",
		"`vscode://file/<percent-encoded-absolute-run-dir>`",
		"`workflow-status.md`",
		"`workflow-status.puml` и `workflow-status.png`",
		"`plantuml -pipe`",
		"короткую диагностику в ближайшую чат-сводку",
		"lawa resume <run-id>",
		"Resume сам отправляет один turn `continue`",
		"Failed-чат пользователь продолжает",
		"прерывают активные turn через Codex",
		"ID\nтекущего чата недоступен",
		"memory/<threadId>.md",
		"изменять только собственный",
		"https://github.com/stray-live-pixel/Lawa",
		"https://raw.githubusercontent.com/stray-live-pixel/Lawa/main/product/1.md",
		"lawa version",
		"Значение\n   dev означает локальную сборку",
		"его SHA-256 и полный вывод `lawa help`",
		"явное\nразрешение перед публичной",
		"https://github.com/stray-live-pixel/Lawa/issues/new",
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
	onRun     func()
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
	if c.onRun != nil {
		c.onRun()
	}
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
		check:           check,
		client:          func(string, io.Writer) coordinator.Client { return client },
		pollInterval:    time.Millisecond,
		refreshInterval: coordinator.DefaultRefreshInterval,
		chatInterval:    defaultChatInterval,
		now:             time.Now,
		waitUntil:       series.WaitUntil,
		renderer:        successfulCLIRenderer(),
		userHomeDir: func() (string, error) {
			return "", errors.New("home не должен использоваться при --root")
		},
	}
}

// TestRecurringRunModesWithControlledClock проходит публичный CLI для всех
// режимов. Управляемые часы доказывают точный лимит, отсчёт after от завершения
// и отсутствие очереди cron-точек, пропущенных во время долгого run.
func TestRecurringRunModesWithControlledClock(t *testing.T) {
	cases := []struct {
		name        string
		flags       []string
		start       time.Time
		runDuration time.Duration
		wantTargets []time.Time
	}{
		{
			name: "immediate", flags: []string{"--repeat", "immediate", "--max-runs", "3"},
			start:       time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC),
			wantTargets: []time.Time{time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)},
		},
		{
			name: "after", flags: []string{"--repeat", "after", "--repeat-delay", "1h", "--max-runs", "2"},
			start: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC), runDuration: 15 * time.Minute,
			wantTargets: []time.Time{time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 11, 15, 0, 0, time.UTC)},
		},
		{
			name: "cron", flags: []string{"--repeat", "cron", "--cron", "0 10 * * *", "--timezone", "Europe/Moscow", "--max-runs", "2"},
			start: time.Date(2026, 8, 31, 6, 59, 0, 0, time.UTC), runDuration: 2 * time.Hour,
			wantTargets: []time.Time{time.Date(2026, 8, 31, 7, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, cwd := filepath.Join(t.TempDir(), "runs"), t.TempDir()
			workflowPath := filepath.Join(t.TempDir(), "workflow.json")
			if err := os.WriteFile(workflowPath, []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"Сделай","dependsOn":[]}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var clockMu sync.Mutex
			current := tc.start
			now := func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return current
			}
			client := newCLIFakeClient()
			client.onRun = func() {
				clockMu.Lock()
				current = current.Add(tc.runDuration)
				clockMu.Unlock()
			}
			deps := cliTestDependencies(client, func(context.Context, codex.Connection) error { return nil })
			deps.now = now
			var targets []time.Time
			deps.waitUntil = func(_ context.Context, target time.Time, _ func() time.Time, _ func() (bool, error)) error {
				targets = append(targets, target)
				clockMu.Lock()
				current = target
				clockMu.Unlock()
				return nil
			}
			args := []string{"run", workflowPath, "--cwd", cwd, "--task", "Задача", "--initiator-thread-id", "initiator", "--root", root}
			args = append(args, tc.flags...)
			var out bytes.Buffer
			if err := executeContext(t.Context(), args, &out, io.Discard, deps); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(targets, tc.wantTargets) {
				t.Fatalf("точки запуска: %v; ожидались %v", targets, tc.wantTargets)
			}
			if got := strings.Count(out.String(), "runId:"); got != len(tc.wantTargets) {
				t.Fatalf("создано %d run вместо %d: %q", got, len(tc.wantTargets), out.String())
			}
			seriesEntries, err := os.ReadDir(filepath.Join(root, "series"))
			if err != nil || len(seriesEntries) != 1 {
				t.Fatalf("не найдена одна серия: %v, %v", seriesEntries, err)
			}
			snapshot, err := series.Load(root, seriesEntries[0].Name())
			if err != nil || snapshot.State != series.Completed || snapshot.RunsStarted != len(tc.wantTargets) || snapshot.RunsFinished != len(tc.wantTargets) {
				t.Fatalf("неверный прогресс серии: %+v, %v", snapshot, err)
			}
		})
	}
}

// TestRecurringRunWithoutLimitStopsExplicitly проверяет публичный бесконечный
// режим: отсутствие --max-runs не завершает серию само, а series-stop перед
// третьей итерацией оставляет ровно два законченных обычных run.
func TestRecurringRunWithoutLimitStopsExplicitly(t *testing.T) {
	parent, cwd := t.TempDir(), t.TempDir()
	root, workflowPath := filepath.Join(parent, "runs"), filepath.Join(parent, "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"Сделай","dependsOn":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := cliTestDependencies(newCLIFakeClient(), func(context.Context, codex.Connection) error { return nil })
	waits := 0
	deps.waitUntil = func(ctx context.Context, _ time.Time, _ func() time.Time, _ func() (bool, error)) error {
		waits++
		if waits != 3 {
			return nil
		}
		entries, err := os.ReadDir(filepath.Join(root, "series"))
		if err != nil {
			return fmt.Errorf("найти серию для остановки: %w", err)
		}
		if len(entries) != 1 {
			return fmt.Errorf("для остановки ожидалась одна серия, найдено %d", len(entries))
		}
		return executeContext(ctx, []string{"series-stop", entries[0].Name(), "--root", root}, io.Discard, io.Discard, deps)
	}
	var out bytes.Buffer
	err := executeContext(t.Context(), []string{
		"run", workflowPath, "--cwd", cwd, "--task", "Задача", "--initiator-thread-id", "initiator",
		"--root", root, "--repeat", "immediate",
	}, &out, io.Discard, deps)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "series"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("не найдена остановленная серия: %v, %v", entries, err)
	}
	snapshot, err := series.Load(root, entries[0].Name())
	if err != nil || snapshot.State != series.Stopped || snapshot.Config.MaxRuns != 0 || snapshot.RunsStarted != 2 || snapshot.RunsFinished != 2 || !snapshot.StopRequested || strings.Count(out.String(), "runId:") != 2 {
		t.Fatalf("серия без лимита завершилась не по явному stop: %+v, waits=%d out=%q err=%v", snapshot, waits, out.String(), err)
	}
}

// TestRecurringRunOutputFailureKeepsCurrentRun воспроизводит R1 через публичный
// CLI: run уже создан, но его ID не удалось вывести. Серия должна показать этот
// незавершённый run оператору, а не засчитать ложный терминал.
func TestRecurringRunOutputFailureKeepsCurrentRun(t *testing.T) {
	parent, cwd := t.TempDir(), t.TempDir()
	root := filepath.Join(parent, "runs")
	workflowPath := filepath.Join(parent, "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"Сделай","dependsOn":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("stdout закрыт")
	out := &failOnWriteWriter{failAt: 2, err: failure}
	deps := cliTestDependencies(newCLIFakeClient(), func(context.Context, codex.Connection) error { return nil })
	err := executeContext(t.Context(), []string{
		"run", workflowPath, "--cwd", cwd, "--task", "Задача", "--initiator-thread-id", "initiator",
		"--root", root, "--repeat", "immediate", "--max-runs", "1",
	}, out, io.Discard, deps)
	if !errors.Is(err, failure) {
		t.Fatalf("ошибка вывода потеряна: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "series"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("не найдена созданная серия: %v, %v", entries, err)
	}
	snapshot, err := series.Load(root, entries[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != series.Failed || snapshot.RunsStarted != 1 || snapshot.RunsFinished != 0 || snapshot.CurrentRunID == "" || snapshot.LastError != failure.Error() {
		t.Fatalf("ошибка вывода ложно завершила run: %+v", snapshot)
	}
	run, err := runstore.Load(root, snapshot.CurrentRunID)
	if err != nil || run.Meta.Steps[0].State != scheduler.Pending {
		t.Fatalf("series-status ссылается не на незавершённый run: %+v, %v", run.Meta.Steps, err)
	}
}

// TestRecurringRunFinalOutputFailureFinishesCurrentRun закрывает N1 через
// публичный CLI. Оператор видит ошибку финального stdout, но series-status больше
// не предлагает resume для run, каждый шаг которого уже сохранён как Succeeded.
func TestRecurringRunFinalOutputFailureFinishesCurrentRun(t *testing.T) {
	parent, cwd := t.TempDir(), t.TempDir()
	root := filepath.Join(parent, "runs")
	workflowPath := filepath.Join(parent, "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"Сделай","dependsOn":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("финальный stdout закрыт")
	deps := cliTestDependencies(newCLIFakeClient(), func(context.Context, codex.Connection) error { return nil })
	err := executeContext(t.Context(), []string{
		"run", workflowPath, "--cwd", cwd, "--task", "Задача", "--initiator-thread-id", "initiator",
		"--root", root, "--repeat", "immediate", "--max-runs", "1",
	}, failOnTextWriter{text: "успешно завершён", err: failure}, io.Discard, deps)
	if !errors.Is(err, failure) {
		t.Fatalf("ошибка финального вывода потеряна: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "series"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("не найдена созданная серия: %v, %v", entries, err)
	}
	snapshot, err := series.Load(root, entries[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != series.Failed || snapshot.RunsStarted != 1 || snapshot.RunsFinished != 1 || snapshot.CurrentRunID != "" || !strings.Contains(snapshot.LastError, failure.Error()) {
		t.Fatalf("терминальный run не учтён после ошибки вывода: %+v", snapshot)
	}
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var runID string
	for _, entry := range rootEntries {
		if entry.IsDir() && entry.Name() != "series" {
			if runID != "" {
				t.Fatalf("после одного повтора найдено несколько run: %q и %q", runID, entry.Name())
			}
			runID = entry.Name()
		}
	}
	if runID == "" {
		t.Fatal("обычный run не найден")
	}
	run, err := runstore.Load(root, runID)
	if err != nil || len(run.Meta.Steps) != 1 || run.Meta.Steps[0].State != scheduler.Succeeded {
		t.Fatalf("обычный run потерял успешный терминал: %+v, %v", run.Meta.Steps, err)
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
		{"workflow.json", "--cwd", "/tmp", "--task", "x", "--initiator-thread-id", "i", "--repeat="},
		{"workflow.json", "--cwd", "/tmp", "--task", "x", "--initiator-thread-id", "i", "--repeat", "after", "--repeat-delay="},
		{"workflow.json", "--cwd", "/tmp", "--task", "x", "--initiator-thread-id", "i", "--max-runs", "2"},
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

// TestParseRunArgumentsRejectsMutuallyExclusiveEmptyFlags закрывает R7: явный
// пустой флаг всё равно означает выбранный способ передачи текста. При этом пустой
// inline-комментарий без файловой формы остаётся разрешённым контрактом CLI.
func TestParseRunArgumentsRejectsMutuallyExclusiveEmptyFlags(t *testing.T) {
	for _, args := range [][]string{
		{"workflow.json", "--cwd", "/tmp", "--task=", "--task-file", "/tmp/task", "--initiator-thread-id", "i"},
		{"workflow.json", "--cwd", "/tmp", "--task", "x", "--comment=", "--comment-file", "/tmp/comment", "--initiator-thread-id", "i"},
	} {
		if _, err := parseRunArguments(args); err == nil {
			t.Errorf("приняты взаимоисключающие флаги: %v", args)
		}
	}
	parsed, err := parseRunArguments([]string{
		"workflow.json", "--cwd", "/tmp", "--task", "x", "--comment=", "--initiator-thread-id", "i",
	})
	if err != nil || parsed.comment != "" || parsed.commentFile != "" {
		t.Fatalf("одиночный пустой --comment= должен быть допустим: %+v, %v", parsed, err)
	}
}

// TestReportExitDistinguishesCancellationFromStorageFailure закрывает R6.
// Координатор именно так объединяет сигнал с ошибкой run.Update: код выхода должен
// по-прежнему сообщать Ctrl+C, но управляющий агент обязан увидеть сбой записи.
func TestReportExitDistinguishesCancellationFromStorageFailure(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		signal     syscall.Signal
		wantCode   int
		wantOutput []string
	}{
		{
			name:     "чистая отмена остаётся тихой",
			err:      errors.Join(context.Canceled, fmt.Errorf("turn остановлен: %w", context.Canceled)),
			signal:   syscall.SIGINT,
			wantCode: 130,
		},
		{
			name:       "ошибка записи при отмене видна",
			err:        errors.Join(context.Canceled, fmt.Errorf("координатор: сохранить результат шага %q: %w", "cube-1", syscall.EIO)),
			signal:     syscall.SIGTERM,
			wantCode:   143,
			wantOutput: []string{"сохранить результат", "input/output error"},
		},
		{
			name:       "обычная ошибка по-прежнему видна",
			err:        errors.New("Codex unavailable"),
			wantCode:   2,
			wantOutput: []string{"Codex unavailable"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := reportExit(&stderr, test.err, int32(test.signal)); code != test.wantCode {
				t.Fatalf("неверный код выхода: %d, нужен %d", code, test.wantCode)
			}
			if got := stderr.String(); len(test.wantOutput) == 0 && got != "" {
				t.Fatalf("чистая отмена напечатала ошибку: %q", got)
			} else {
				for _, fragment := range test.wantOutput {
					if !strings.Contains(got, fragment) {
						t.Fatalf("потеряна диагностика %q: %q", fragment, got)
					}
				}
			}
		})
	}
}
