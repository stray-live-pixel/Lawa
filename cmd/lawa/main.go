// Команда lawa предоставляет единый фасад над Codex App Server: пользователь
// работает с run/status/logs, а запуск процессов, thread/turn и восстановление
// остаются внутренней ответственностью Lawa.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/buildinfo"
	"github.com/stray-live-pixel/Lawa/internal/capacity"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/dashboard"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/series"
	"github.com/stray-live-pixel/Lawa/internal/statusreport"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

const help = `Lawa — выполнение JSON-workflow через Codex App Server.

Команды:
  lawa run <workflow.json> --cwd <проект> (--task <текст> | --task-file <путь>)
      Создать run, запустить готовые кубики и наблюдать их до результата.
  lawa resume <run-id>
      Сверить сохранённые thread и продолжить interrupted-кубики.
  lawa status <run-id>
      Показать состояния, thread/turn, процесс и последнюю активность кубиков.
  lawa logs <run-id> [step-id] [--visit <visit-id>] [--follow]
      Показать журнал всего run, логического шага или точного посещения v2.
  lawa serve [--root <путь>] [--listen <адрес>]
      Запустить read-only dashboard; по умолчанию http://127.0.0.1:60800.
  lawa series-status <series-id>
      Показать режим, прогресс, текущий run и время следующего запуска.
  lawa series-stop <series-id>
      Запретить будущие run серии; уже работающий run спокойно завершается.
  lawa validate <workflow.json>
      Проверить поля, ссылки и отсутствие циклов без создания run.
  lawa skill
      Вывести готовый SKILL.md для установки скилла /lawa.
  lawa version
      Показать версию текущего бинарника.
  lawa update [--yes] [--install-plantuml] [--codex-home <путь>]
      Проверить GitHub Release и безопасно обновить бинарник и скилл.
  lawa help
      Показать справку (также -h и --help).

Параметры run:
  --cwd <путь>                 Существующая рабочая папка проекта; обязательно.
  --task <текст>               Финальная постановка задачи.
  --task-file <путь>           Безопасная альтернатива --task для многострочного текста.
  --comment <текст>            Комментарий пользователя; может быть пустым.
  --comment-file <путь>        Альтернатива --comment.
  --parent-run <run-id>        Необязательный родитель для дерева связанных workflow.
  --root <путь>                Хранилище run; по умолчанию ~/.light-ai-workflows.
  --codex <путь>               Исполняемый файл Codex; по умолчанию codex из PATH.
  --max-parallel <N>           Общий для root лимит активных turn; сохраняется.
  --repeat <режим>             immediate, after или cron.
  --repeat-delay <интервал>    Задержка after от завершения run, например 1h.
  --cron <расписание>          Стандартные 5 полей: minute hour day month weekday.
  --timezone <IANA-зона>       Явная зона cron, например Europe/Moscow.
  --max-runs <N>               Положительный лимит; без него серия бесконечна.

Параметры resume:
  --root <путь>                То же хранилище run.
  --codex <путь>               Исполняемый файл Codex.
  --max-parallel <N>           Задать или изменить общий лимит для root.

Параметры status/logs:
  --root <путь>                То же хранилище run.
  --visit <visit-id>           Для logs: одно точное посещение workflow v2;
                               взаимоисключающе с позиционным step-id.
  --follow                     Для logs: ждать новые события до завершения или сигнала.

Параметры serve:
  --root <путь>                То же хранилище run.
  --listen <host:port>         Адрес сервера; по умолчанию 127.0.0.1:60800.

Параметры update:
  --yes                        Не ждать подтверждения файлов Lawa и PATH.
  --install-plantuml           Отдельно разрешить системную установку PlantUML;
                               требует --yes.
  --codex-home <путь>          Корень скиллов; по умолчанию $CODEX_HOME или ~/.codex.

status, logs, serve, validate, skill, version, update и help не запускают агентов.
Коды выхода: 0 — успех; 2 — ошибка ввода/интеграции; 130 — SIGINT; 143 — SIGTERM.
После сигнала новые волны не стартуют, а активные turn получают turn/interrupt.
Сопутствующая ошибка сохранения остаётся видимой в stderr при коде 130 или 143.
Resume отправляет continue только interrupted-чатам; failed продолжите вручную.
Run и resume печатают краткую статистику и VS Code-ссылку не чаще раза в 5 минут;
первый и финальный снимки выводятся сразу. Подробный workflow-status.md и схема
обновляются локально при изменениях и не реже раза в минуту.
Max-parallel сохраняется для root и суммарно ограничивает отдельные процессы run
и resume; без сохранённого значения собственного лимита нет.
Кубики могут запускать дочерние workflow через встроенные run_child/run_children;
Lawa возвращает runId после сохранения и ждёт всё созданное дерево.
Для PNG нужна команда plantuml с поддержкой -pipe; её ошибка не останавливает workflow.

Lawa использует только Codex App Server и не создаёт нативные задачи Codex Desktop.
Причина: у Desktop нет публичного программного API для внешнего Go-процесса,
а управление через агента-посредника добавляет задержку, стоимость и узкое место.
`

// skillInstruction хранится отдельным SKILL.md, чтобы инструкцию можно было читать,
// редактировать и проверять как обычный скилл. Встраивание сохраняет автономность
// бинарника: команда не ищет файл во время работы и печатает снимок времени сборки.
//
//go:embed SKILL.md
var skillInstruction string

// main устанавливает обработчики только вокруг текущего процесса Lawa. Сигнал
// отменяет наблюдение и новые волны; координатор адресно прерывает активные turn.
// Точный код нужен вызывающему агенту чата-инициатора.
func main() {
	if codex.RunDirectoryHelper() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	var received atomic.Int32
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			received.Store(int32(sig.(syscall.Signal)))
			cancel()
		case <-done:
		}
	}()
	err := executeContext(ctx, os.Args[1:], os.Stdout, os.Stderr, productionDependencies())
	signal.Stop(signals)
	close(done)
	cancel()
	code := reportExit(os.Stderr, err, received.Load())
	if code != 0 {
		os.Exit(code)
	}
}

// reportExit сохраняет код полученного сигнала, но скрывает только чистую отмену.
// Координатор объединяет context.Canceled с ошибками interrupt и сохранения через
// errors.Join. Проверка одного errors.Is поэтому теряет важную диагностику: она
// истинна и для context.Canceled + EIO. Дополнительная причина всегда печатается,
// чтобы управляющий агент не пытался продолжить run с незамеченным сбоем состояния.
func reportExit(stderr io.Writer, err error, received int32) int {
	code := exitCode(err, received)
	if err != nil && (received == 0 || !isCancellationOnly(err)) {
		fmt.Fprintln(stderr, "lawa:", runstore.SafeTerminalText(err.Error()))
	}
	return code
}

// isCancellationOnly обходит как обычные обёртки с Unwrap() error, так и
// составные errors.Join с Unwrap() []error. Ошибка считается чистой отменой,
// только когда каждый лист дерева равен context.Canceled. errors.Is здесь
// намеренно недостаточен: он отвечает, есть ли отмена, но не замечает соседний EIO.
func isCancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	switch current := err.(type) {
	case interface{ Unwrap() []error }:
		causes := current.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !isCancellationOnly(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return isCancellationOnly(current.Unwrap())
	default:
		return err == context.Canceled
	}
}

// exitCode отделяет пользовательские ошибки от двух поддерживаемых сигналов.
// Сигнал имеет приоритет: вызывающий агент должен понять, что остановку попросил
// пользователь, даже если при сохранении последнего статуса возникла ещё ошибка.
func exitCode(err error, received int32) int {
	if received != 0 {
		return 128 + int(received)
	}
	if err != nil {
		return 2
	}
	return 0
}

// dependencies содержит заменяемые границы CLI. Production использует настоящий
// app-server, тесты — клиент без модели и изолированное временное хранилище.
type dependencies struct {
	check            func(context.Context, codex.Connection) error
	client           func(string, io.Writer, *codex.Directory) coordinator.Client
	pollInterval     time.Duration
	logsPollInterval time.Duration
	refreshInterval  time.Duration
	chatInterval     time.Duration
	now              func() time.Time
	waitUntil        func(context.Context, time.Time, func() time.Time, func() (bool, error)) error
	renderer         statusreport.Renderer
	userHomeDir      func() (string, error)
	update           updateDependencies
}

// productionDependencies собирает стандартное окружение команд run/resume.
func productionDependencies() dependencies {
	return dependencies{
		check: codex.Check,
		client: func(executable string, stderr io.Writer, directory *codex.Directory) coordinator.Client {
			return coordinator.ProductionClient{Executable: executable, Stderr: stderr, Directory: directory}
		},
		// Одна read-only app-server-сессия обслуживает все сверки текущего запуска.
		// Пять секунд сохраняют отзывчивость ручного продолжения и не создают
		// лишний поток thread/read-запросов в ожидании действий пользователя.
		pollInterval:     5 * time.Second,
		logsPollInterval: time.Second,
		refreshInterval:  coordinator.DefaultRefreshInterval,
		chatInterval:     defaultChatInterval,
		now:              time.Now,
		waitUntil:        series.WaitUntil,
		// Pipe-режим не даёт renderer доступ к путям run. Отсутствующий или
		// сломанный PlantUML станет видимой диагностикой, но не остановит workflow.
		renderer:    statusreport.CommandRenderer{Executable: "plantuml", Timeout: 30 * time.Second},
		userHomeDir: os.UserHomeDir,
		update:      productionUpdateDependencies(),
	}
}

// execute сохраняет прежний простой интерфейс unit-тестов help/skill/validate.
// Исполнительные тесты вызывают executeContext с подставным клиентом и root.
func execute(args []string, out io.Writer) error {
	return executeContext(context.Background(), args, out, io.Discard, productionDependencies())
}

// executeContext разбирает только выбранную команду. Проверка workflow и cwd
// завершается до Codex preflight, а новый run создаётся только после успешного
// рукопожатия app-server: неверный ввод и недоступный Codex не оставляют папку.
func executeContext(ctx context.Context, args []string, out, stderr io.Writer, deps dependencies) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")) {
		_, err := io.WriteString(out, help)
		return err
	}
	if args[0] == "skill" {
		if len(args) != 1 {
			return fmt.Errorf("использование: lawa skill")
		}
		_, err := io.WriteString(out, skillInstruction)
		return err
	}
	if args[0] == "version" {
		if len(args) != 1 {
			return fmt.Errorf("использование: lawa version")
		}
		_, err := fmt.Fprintln(out, buildinfo.Version)
		return err
	}
	switch args[0] {
	case "validate":
		return validateCommand(args[1:], out)
	case "run":
		return runCommand(ctx, args[1:], out, stderr, deps)
	case "resume":
		return resumeCommand(ctx, args[1:], out, stderr, deps)
	case "status":
		return statusCommand(args[1:], out, deps)
	case "logs":
		return logsCommand(ctx, args[1:], out, deps)
	case "serve":
		return serveCommand(ctx, args[1:], out, stderr, deps)
	case "series-status":
		return seriesStatusCommand(args[1:], out, deps)
	case "series-stop":
		return seriesStopCommand(args[1:], out, deps)
	case "update":
		return updateCommand(ctx, args[1:], out, stderr, deps.update)
	default:
		return fmt.Errorf("неизвестная команда %q; см. lawa help", args[0])
	}
}

// validateCommand читает один файл и не выполняет Codex preflight.
func validateCommand(args []string, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("использование: lawa validate <workflow.json>")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return fmt.Errorf("открыть workflow: %w", err)
	}
	defer f.Close()
	w, err := workflow.Decode(f)
	if err != nil {
		return fmt.Errorf("проверить %q: %w", args[0], err)
	}
	// %q экранирует управляющие символы в пользовательском идентификаторе.
	_, err = fmt.Fprintf(out, "Workflow %q корректен; шагов: %d.\n", w.ID, len(w.Steps))
	return err
}

// runArguments хранит уже разобранные, но ещё не нормализованные параметры run.
type runArguments struct {
	workflow, cwd, task, taskFile, comment, commentFile, parentRun, root, executable string
	maxParallel, repeat, repeatDelay, cron, timezone, maxRuns                        string
}

// resumeArguments не содержит cwd: продолжение обязано использовать сохранённый.
type resumeArguments struct{ runID, root, executable, maxParallel string }

// serveArguments хранит независимые от Codex параметры локального HTTP-сервера.
type serveArguments struct{ root, address string }

// runCommand валидирует весь ввод и подключение до создания нового run. После
// публикации runId дальнейшая ошибка оставляет его пригодным для resume.
func runCommand(ctx context.Context, args []string, out, stderr io.Writer, deps dependencies) (err error) {
	parsed, err := parseRunArguments(args)
	if err != nil {
		return err
	}
	if parsed.root, err = resolveRoot(parsed.root, deps.userHomeDir); err != nil {
		return err
	}
	parsed.cwd, err = filepath.Abs(parsed.cwd)
	if err != nil {
		return fmt.Errorf("определить cwd: %w", err)
	}
	workflowJSON, err := os.ReadFile(parsed.workflow)
	if err != nil {
		return fmt.Errorf("открыть workflow: %w", err)
	}
	definition, err := workflow.Decode(strings.NewReader(string(workflowJSON)))
	if err != nil {
		return fmt.Errorf("проверить %q: %w", parsed.workflow, err)
	}
	if parsed.taskFile != "" {
		if parsed.task, err = readTextArgument(parsed.taskFile, "постановку"); err != nil {
			return err
		}
	}
	if parsed.commentFile != "" {
		if parsed.comment, err = readTextArgument(parsed.commentFile, "комментарий"); err != nil {
			return err
		}
	}
	if !utf8.ValidString(parsed.task+parsed.comment) || strings.TrimSpace(parsed.task) == "" {
		return errors.New("постановка должна быть непустым текстом UTF-8; комментарий также должен быть UTF-8")
	}
	var config series.Config
	var schedule series.Schedule
	if parsed.repeat != "" {
		config, schedule, err = series.ParseConfig(parsed.repeat, parsed.repeatDelay, parsed.cron, parsed.timezone, parsed.maxRuns)
		if err != nil {
			return err
		}
	}
	connection := codex.Connection{Executable: parsed.executable, CWD: parsed.cwd, Stderr: stderr}
	if err = deps.check(ctx, connection); err != nil {
		return fmt.Errorf("проверить подключение Codex: %w", err)
	}
	pool, err := capacity.Configure(parsed.root, parsed.maxParallel)
	if err != nil {
		return fmt.Errorf("настроить общий лимит параллельности: %w", err)
	}
	input := runstore.Input{
		WorkflowJSON: workflowJSON,
		Task:         parsed.task, Comment: parsed.comment, CWD: parsed.cwd,
		ParentRunID: parsed.parentRun,
	}
	if parsed.repeat != "" {
		return runSeries(ctx, parsed.root, parsed.executable, input, definition.ID, config, schedule, pool, out, stderr, deps)
	}
	snapshot, err := runstore.Create(parsed.root, input)
	if err != nil {
		return fmt.Errorf("создать запуск: %w", err)
	}
	if _, err = fmt.Fprintf(out, "runId: %s\n", snapshot.Meta.RunID); err != nil {
		return err
	}
	return coordinate(ctx, parsed.root, snapshot.Meta.RunID, parsed.executable, pool, out, stderr, deps, false, false)
}

// resumeCommand никогда не создаёт замену отсутствующему или повреждённому run.
func resumeCommand(ctx context.Context, args []string, out, stderr io.Writer, deps dependencies) error {
	parsed, err := parseResumeArguments(args)
	if err != nil {
		return err
	}
	if parsed.root, err = resolveRoot(parsed.root, deps.userHomeDir); err != nil {
		return err
	}
	pool, err := capacity.Configure(parsed.root, parsed.maxParallel)
	if err != nil {
		return fmt.Errorf("настроить общий лимит параллельности: %w", err)
	}
	return coordinate(ctx, parsed.root, parsed.runID, parsed.executable, pool, out, stderr, deps, true, false)
}

// serveCommand не открывает Codex и не создаёт хранилище. Loopback безопасен по
// умолчанию; явная публикация на другом интерфейсе остаётся возможной, но видимой.
// Предупреждение отдельно называет live-вывод, потому что read-only HTTP не делает
// команды и ответы агента публично безопасными.
func serveCommand(ctx context.Context, args []string, out, stderr io.Writer, deps dependencies) error {
	parsed, err := parseServeArguments(args)
	if err != nil {
		return err
	}
	if parsed.root, err = resolveRoot(parsed.root, deps.userHomeDir); err != nil {
		return err
	}
	if !dashboard.IsLoopbackAddress(parsed.address) {
		if _, err = fmt.Fprintf(stderr, "lawa: предупреждение: dashboard доступен не только с этого компьютера; live-вывод может содержать секреты: %s\n", parsed.address); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintf(out, "Dashboard: http://%s\nPreview: http://%s/preview\n", parsed.address, parsed.address); err != nil {
		return err
	}
	return dashboard.Serve(ctx, parsed.root, parsed.address)
}

// runSeries последовательно создаёт обычные run. Блокирующий coordinate служит
// главным барьером параллельности: следующий run нельзя запланировать, пока
// предыдущий не вернул терминальный успех или явную неуспешность.
func runSeries(ctx context.Context, root, executable string, input runstore.Input, workflowID string, config series.Config, schedule series.Schedule, pool *capacity.Pool, out, stderr io.Writer, deps dependencies) (err error) {
	owner, err := series.Create(root, config, workflowID)
	if err != nil {
		return fmt.Errorf("создать серию: %w", err)
	}
	defer func() { err = errors.Join(err, owner.Close()) }()
	if _, err = fmt.Fprintf(out, "seriesId: %s\n", owner.Snapshot().SeriesID); err != nil {
		return err
	}
	for {
		meta := owner.Snapshot()
		if config.MaxRuns > 0 && meta.RunsStarted >= config.MaxRuns {
			return owner.FinishSeries(series.Completed)
		}
		if stopped, checkErr := owner.StopRequested(); checkErr != nil {
			return checkErr
		} else if stopped {
			return owner.FinishSeries(series.Stopped)
		}
		next := schedule.Next(deps.now(), meta.RunsStarted)
		if err = owner.SetNext(next); err != nil {
			return fmt.Errorf("сохранить следующий запуск серии: %w", err)
		}
		if err = deps.waitUntil(ctx, next, deps.now, owner.StopRequested); err != nil {
			if errors.Is(err, series.ErrStopped) {
				return owner.FinishSeries(series.Stopped)
			}
			return errors.Join(err, owner.FinishSeries(series.Stopped))
		}
		var snapshot runstore.Snapshot
		started, startErr := owner.StartRun(func() (string, error) {
			var createErr error
			snapshot, createErr = runstore.Create(root, input)
			return snapshot.Meta.RunID, createErr
		}, func(runID string) error {
			return runstore.RemoveUnstarted(root, runID)
		})
		if startErr != nil {
			return fmt.Errorf("создать run серии: %w", startErr)
		}
		if !started {
			return owner.FinishSeries(series.Stopped)
		}
		if _, err = fmt.Fprintf(out, "runId: %s\n", snapshot.Meta.RunID); err != nil {
			return errors.Join(err, owner.FailRunControl(err))
		}
		outcome, runErr := coordinateWithOutcome(ctx, root, snapshot.Meta.RunID, executable, pool, out, stderr, deps, false, true)
		// Терминальность берём из сохранённого состояния, а не выводим из ошибки.
		// Поэтому отказ финальной сводки останавливает серию и остаётся видимым,
		// но уже успешный run всё равно учитывается и больше не предлагается resume.
		if outcome.Terminal {
			if finishErr := owner.FinishRun(runErr); finishErr != nil {
				return errors.Join(runErr, fmt.Errorf("сохранить завершение run серии: %w", finishErr))
			}
		} else if controlErr := owner.FailRunControl(runErr); controlErr != nil {
			return errors.Join(runErr, fmt.Errorf("сохранить ошибку управления серией: %w", controlErr))
		}
		if runErr != nil {
			return runErr
		}
	}
}

// seriesStatusCommand — read-only диагностика, пригодная и после завершения процесса.
func seriesStatusCommand(args []string, out io.Writer, deps dependencies) error {
	seriesID, root, err := parseSeriesArguments(args, deps)
	if err != nil {
		return err
	}
	snapshot, err := series.Load(root, seriesID)
	if err != nil {
		return fmt.Errorf("прочитать серию %q: %w", seriesID, err)
	}
	next, current := "-", snapshot.CurrentRunID
	if snapshot.NextRunAt != nil {
		next = snapshot.NextRunAt.Format(time.RFC3339)
	}
	if current == "" {
		current = "-"
	}
	lastError := ""
	if snapshot.LastError != "" {
		lastError = "последняя ошибка: " + runstore.SafeTerminalText(snapshot.LastError) + "\n"
	}
	workflowID := snapshot.WorkflowID
	if workflowID == "" {
		workflowID = "-"
	}
	_, err = fmt.Fprintf(out, "seriesId: %s\nworkflow: %s\nрежим: %s\nсостояние: %s\nзапусков: %d начато, %d завершено\ncurrentRunId: %s\nnextRunAt: %s\nstopRequested: %t\n%s", snapshot.SeriesID, workflowID, snapshot.Config.Mode, snapshot.State, snapshot.RunsStarted, snapshot.RunsFinished, current, next, snapshot.StopRequested, lastError)
	return err
}

// seriesStopCommand публикует идемпотентный stop-маркер. Текущий run намеренно
// не прерывается: это не оставляет interrupted-чаты и соответствует безопасной остановке.
func seriesStopCommand(args []string, out io.Writer, deps dependencies) error {
	seriesID, root, err := parseSeriesArguments(args, deps)
	if err != nil {
		return err
	}
	if err = series.RequestStop(root, seriesID); err != nil {
		return fmt.Errorf("остановить серию %q: %w", seriesID, err)
	}
	_, err = fmt.Fprintf(out, "Серия %s остановится после текущего run; новые run не создаются.\n", seriesID)
	return err
}

func parseSeriesArguments(args []string, deps dependencies) (string, string, error) {
	positionals, values, err := parseOptions(args, map[string]bool{"root": true})
	if err != nil || len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		if err != nil {
			return "", "", err
		}
		return "", "", errors.New("использование: lawa series-status|series-stop <series-id> [--root <путь>]")
	}
	root, err := resolveRoot(values["root"], deps.userHomeDir)
	return positionals[0], root, err
}

// coordinate удерживает lock на протяжении preflight, сверки и исполнения.
// Любая ошибка закрывает только владельца хранилища; сохранённые чаты не удаляются.
// resume отличает явное продолжение от первого run: только resume имеет право
// автоматически отправлять continue в interrupted-чаты.
func coordinate(ctx context.Context, root, runID, executable string, pool *capacity.Pool, out, stderr io.Writer, deps dependencies, resume, returnOnFailure bool) (err error) {
	_, err = coordinateWithOutcome(ctx, root, runID, executable, pool, out, stderr, deps, resume, returnOnFailure)
	return err
}

// coordinateWithOutcome сохраняет результат Execute отдельно от ошибок открытия,
// вывода и закрытия хранилища. Терминальный Outcome остаётся действительным, если
// ошибка управления произошла уже после надёжного сохранения состояния run.
func coordinateWithOutcome(ctx context.Context, root, runID, executable string, pool *capacity.Pool, out, stderr io.Writer, deps dependencies, resume, returnOnFailure bool) (outcome coordinator.Outcome, err error) {
	snapshot, err := runstore.Load(root, runID)
	if err != nil {
		return coordinator.Outcome{}, fmt.Errorf("прочитать cwd запуска %q: %w", runID, err)
	}
	directory, err := codex.OpenDirectory(snapshot.Meta.CWD)
	if err != nil {
		return coordinator.Outcome{}, fmt.Errorf("открыть cwd запуска %q: %w", runID, err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	manager := newChildRunManager(ctx, root, executable, pool, stderr, deps)
	outcome, err = coordinateRunWithOutcome(ctx, root, runID, executable, pool, out, stderr, deps, resume, returnOnFailure, manager, directory)
	return outcome, errors.Join(err, manager.wait())
}

// coordinateRunWithOutcome выполняет ровно один сохранённый run. Верхний вызов
// владеет manager и ждёт всё дерево, а дочерние вызовы переиспользуют его без
// рекурсивного ожидания самих себя.
func coordinateRunWithOutcome(ctx context.Context, root, runID, executable string, pool *capacity.Pool, out, stderr io.Writer, deps dependencies, resume, returnOnFailure bool, manager *childRunManager, directory *codex.Directory) (outcome coordinator.Outcome, err error) {
	run, err := runstore.OpenLocked(root, runID)
	if err != nil {
		return coordinator.Outcome{}, fmt.Errorf("открыть запуск %q: %w", runID, err)
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	snapshot, err := run.Load()
	if err != nil {
		return coordinator.Outcome{}, fmt.Errorf("прочитать запуск %q: %w", runID, err)
	}
	if resume {
		connectionCWD := snapshot.Meta.CWD
		if directory != nil {
			connectionCWD = directory.Path()
		}
		if err = deps.check(ctx, codex.Connection{Executable: executable, CWD: connectionCWD, Stderr: stderr, Directory: directory}); err != nil {
			return coordinator.Outcome{}, fmt.Errorf("проверить подключение Codex: %w", err)
		}
	}
	if _, err = fmt.Fprintf(out, "Наблюдение за run %s.\n", runID); err != nil {
		return coordinator.Outcome{}, err
	}
	publisher := newStatusPublisher(ctx, out, filepath.Join(root, runID), deps.renderer, deps.chatInterval, deps.now)
	return coordinator.ExecuteWithOutcome(ctx, run, coordinator.Options{
		Root: root, PollInterval: deps.pollInterval, RefreshInterval: deps.refreshInterval,
		Client: deps.client(executable, stderr, directory), ContinueInterrupted: resume,
		ReturnOnFailure:  returnOnFailure,
		Notify:           publisher.Publish,
		Capacity:         pool,
		ConfigureCommand: manager.configure,
	})
}

// parseRunArguments проверяет обязательные значения и взаимоисключающие формы
// передачи текста, но чтение файлов и UTF-8 оставляет runCommand.
func parseRunArguments(args []string) (runArguments, error) {
	var parsed runArguments
	positionals, values, err := parseOptions(args, map[string]bool{
		"cwd": true, "task": true, "task-file": true, "comment": true, "comment-file": true,
		"parent-run": true, "root": true, "codex": true, "max-parallel": true, "repeat": true,
		"repeat-delay": true, "cron": true, "timezone": true, "max-runs": true,
	})
	if err != nil || len(positionals) != 1 {
		if err != nil {
			return parsed, err
		}
		return parsed, errors.New("использование: lawa run <workflow.json> --cwd <проект> (--task <текст> | --task-file <путь>)")
	}
	parsed.workflow, parsed.cwd, parsed.task = positionals[0], values["cwd"], values["task"]
	parsed.taskFile, parsed.comment = values["task-file"], values["comment"]
	parsed.commentFile = values["comment-file"]
	parsed.parentRun, parsed.root, parsed.executable = values["parent-run"], values["root"], values["codex"]
	parsed.maxParallel = values["max-parallel"]
	parsed.repeat, parsed.repeatDelay, parsed.cron = values["repeat"], values["repeat-delay"], values["cron"]
	parsed.timezone, parsed.maxRuns = values["timezone"], values["max-runs"]
	_, hasTask := values["task"]
	_, hasTaskFile := values["task-file"]
	_, hasComment := values["comment"]
	_, hasCommentFile := values["comment-file"]
	_, hasRepeat := values["repeat"]
	// Взаимоисключение относится к выбранным способам передачи, а не к тексту.
	// Пустой --comment= допустим сам по себе, но вместе с --comment-file он уже
	// неоднозначен. Проверка значений пропускала такую пару как будто флага не было.
	if (hasTask && hasTaskFile) || (hasComment && hasCommentFile) {
		return runArguments{}, errors.New("используйте только один из --task/--task-file и --comment/--comment-file")
	}
	if hasRepeat && strings.TrimSpace(parsed.repeat) == "" {
		return runArguments{}, errors.New("--repeat требует режим immediate, after или cron")
	}
	if value, present := values["parent-run"]; present && strings.TrimSpace(value) == "" {
		return runArguments{}, errors.New("--parent-run требует непустой run-id")
	}
	for _, name := range []string{"max-parallel", "repeat-delay", "cron", "timezone", "max-runs"} {
		if value, present := values[name]; present && strings.TrimSpace(value) == "" {
			return runArguments{}, fmt.Errorf("--%s требует непустое значение", name)
		}
	}
	if strings.TrimSpace(parsed.cwd) == "" || strings.TrimSpace(parsed.task) == "" && parsed.taskFile == "" {
		return runArguments{}, errors.New("run требует --cwd и один из --task/--task-file")
	}
	if err := capacity.Validate(parsed.maxParallel); err != nil {
		return runArguments{}, err
	}
	if parsed.repeat == "" && (parsed.repeatDelay != "" || parsed.cron != "" || parsed.timezone != "" || parsed.maxRuns != "") {
		return runArguments{}, errors.New("--repeat-delay, --cron, --timezone и --max-runs требуют --repeat")
	}
	return parsed, nil
}

// parseResumeArguments разрешает только постоянный runId, настройки транспорта
// и общий root-level лимит, который одинаково действует для run и resume.
func parseResumeArguments(args []string) (resumeArguments, error) {
	var parsed resumeArguments
	positionals, values, err := parseOptions(args, map[string]bool{"root": true, "codex": true, "max-parallel": true})
	if err != nil || len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		if err != nil {
			return parsed, err
		}
		return parsed, errors.New("использование: lawa resume <run-id> [--root <путь>] [--codex <путь>] [--max-parallel <N>]")
	}
	parsed.runID, parsed.root, parsed.executable = positionals[0], values["root"], values["codex"]
	parsed.maxParallel = values["max-parallel"]
	if value, present := values["max-parallel"]; present && strings.TrimSpace(value) == "" {
		return resumeArguments{}, errors.New("--max-parallel требует непустое значение")
	}
	if err = capacity.Validate(parsed.maxParallel); err != nil {
		return resumeArguments{}, err
	}
	return parsed, nil
}

// parseServeArguments сохраняет безопасный loopback default и не разрешает
// позиционные значения, которые можно ошибочно принять за root или адрес.
func parseServeArguments(args []string) (serveArguments, error) {
	parsed := serveArguments{address: dashboard.DefaultAddress}
	positionals, values, err := parseOptions(args, map[string]bool{"root": true, "listen": true})
	if err != nil || len(positionals) != 0 {
		if err != nil {
			return serveArguments{}, err
		}
		return serveArguments{}, errors.New("использование: lawa serve [--root <путь>] [--listen <host:port>]")
	}
	parsed.root = values["root"]
	if listen, exists := values["listen"]; exists {
		if strings.TrimSpace(listen) == "" {
			return serveArguments{}, errors.New("--listen требует непустой host:port")
		}
		parsed.address = listen
	}
	return parsed, nil
}

// parseOptions — минимальный строгий разборщик строковых флагов. Стандартный
// flag.FlagSet останавливается на первом позиционном аргументе и не принимает
// продуктовую форму `run workflow.json --cwd ...` без дополнительной перестановки.
func parseOptions(args []string, allowed map[string]bool) ([]string, map[string]string, error) {
	values := map[string]string{}
	var positionals []string
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "--") {
			positionals = append(positionals, argument)
			continue
		}
		nameValue := strings.SplitN(strings.TrimPrefix(argument, "--"), "=", 2)
		name := nameValue[0]
		if !allowed[name] {
			return nil, nil, fmt.Errorf("неизвестный параметр --%s", name)
		}
		if _, repeated := values[name]; repeated {
			return nil, nil, fmt.Errorf("параметр --%s повторён", name)
		}
		value := ""
		if len(nameValue) == 2 {
			value = nameValue[1]
		} else {
			index++
			if index >= len(args) {
				return nil, nil, fmt.Errorf("параметру --%s нужно значение", name)
			}
			value = args[index]
		}
		values[name] = value
	}
	return positionals, values, nil
}

// resolveRoot выбирает приватное хранилище пользователя и возвращает абсолютный
// путь, одинаковый для Create, OpenLocked и абсолютных путей памяти в prompt.
func resolveRoot(root string, userHomeDir func() (string, error)) (string, error) {
	if root == "" {
		home, err := userHomeDir()
		if err != nil {
			return "", fmt.Errorf("найти домашнюю папку: %w", err)
		}
		root = filepath.Join(home, ".light-ai-workflows")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("определить root: %w", err)
	}
	return root, nil
}

// readTextArgument не интерпретирует содержимое как shell или флаги. Файловая
// форма предназначена для многострочной постановки и текста с `$()`, кавычками
// и другими символами, которые опасно подставлять в командную строку оболочки.
func readTextArgument(path, label string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("прочитать %s из %q: %w", label, path, err)
	}
	return string(data), nil
}
