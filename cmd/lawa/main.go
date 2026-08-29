// Команда lawa проверяет, запускает и продолжает workflow через Codex App Server.
// Чат-инициатор остаётся пользовательским интерфейсом, а это CLI сохраняет run,
// координирует зависимости и печатает только компактные изменения статусов.
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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/stray-live-pixel/flows-2/internal/codex"
	"github.com/stray-live-pixel/flows-2/internal/coordinator"
	"github.com/stray-live-pixel/flows-2/internal/runstore"
	"github.com/stray-live-pixel/flows-2/internal/workflow"
)

const help = `Lawa — выполнение JSON-workflow через отдельные задачи Codex.

Команды:
  lawa run <workflow.json> --cwd <проект> (--task <текст> | --task-file <путь>) --initiator-thread-id <id>
      Создать run, запустить готовые кубики и наблюдать до общего успеха.
  lawa resume <run-id>
      Продолжить сохранённый run и учесть ручную работу в прежних чатах.
  lawa validate <workflow.json>
      Проверить поля, ссылки и отсутствие циклов без создания run.
  lawa skill
      Вывести готовый SKILL.md для установки скилла /lawa.
  lawa help
      Показать справку (также -h и --help).

Параметры run:
  --cwd <путь>                 Существующая рабочая папка проекта; обязательно.
  --task <текст>               Финальная постановка задачи.
  --task-file <путь>           Безопасная альтернатива --task для многострочного текста.
  --comment <текст>            Комментарий пользователя; может быть пустым.
  --comment-file <путь>        Альтернатива --comment.
  --initiator-thread-id <id>   ID чата, из которого вызван /lawa; обязательно.
  --root <путь>                Хранилище run; по умолчанию ~/.light-ai-workflows.
  --codex <путь>               Исполняемый файл Codex; по умолчанию codex из PATH.

Параметры resume:
  --root <путь>                То же хранилище run.
  --codex <путь>               Исполняемый файл Codex.

validate, skill и help не запускают агентов и не требуют подключения к Codex.
Коды выхода: 0 — успех; 2 — ошибка ввода/интеграции; 130 — SIGINT; 143 — SIGTERM.
После сигнала новые волны не стартуют, а активные turn получают turn/interrupt.
Resume отправляет continue только interrupted-чатам; failed продолжите вручную.
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
	code := exitCode(err, received.Load())
	if code == 130 || code == 143 {
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "lawa:", err)
		}
		os.Exit(code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lawa:", err)
	}
	if code != 0 {
		os.Exit(code)
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
	check        func(context.Context, codex.Connection) error
	client       func(string, io.Writer) coordinator.Client
	pollInterval time.Duration
	userHomeDir  func() (string, error)
}

// productionDependencies собирает стандартное окружение команд run/resume.
func productionDependencies() dependencies {
	return dependencies{
		check: codex.Check,
		client: func(executable string, stderr io.Writer) coordinator.Client {
			return coordinator.ProductionClient{Executable: executable, Stderr: stderr}
		},
		// Одна read-only app-server-сессия обслуживает все сверки текущего запуска.
		// Пять секунд сохраняют отзывчивость ручного продолжения и не создают
		// лишний поток thread/read-запросов в ожидании действий пользователя.
		pollInterval: 5 * time.Second,
		userHomeDir:  os.UserHomeDir,
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
	switch args[0] {
	case "validate":
		return validateCommand(args[1:], out)
	case "run":
		return runCommand(ctx, args[1:], out, stderr, deps)
	case "resume":
		return resumeCommand(ctx, args[1:], out, stderr, deps)
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
	workflow, cwd, task, taskFile, comment, commentFile, initiator, root, executable string
}

// resumeArguments не содержит cwd: продолжение обязано использовать сохранённый.
type resumeArguments struct{ runID, root, executable string }

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
	if _, err = workflow.Decode(strings.NewReader(string(workflowJSON))); err != nil {
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
	if !utf8.ValidString(parsed.task+parsed.comment+parsed.initiator) || strings.TrimSpace(parsed.task) == "" || strings.TrimSpace(parsed.initiator) == "" {
		return errors.New("постановка и ID чата должны быть непустым текстом UTF-8; комментарий также должен быть UTF-8")
	}
	connection := codex.Connection{Executable: parsed.executable, CWD: parsed.cwd, Stderr: stderr}
	if err = deps.check(ctx, connection); err != nil {
		return fmt.Errorf("проверить подключение Codex: %w", err)
	}
	snapshot, err := runstore.Create(parsed.root, runstore.Input{
		WorkflowJSON: workflowJSON,
		Task:         parsed.task, Comment: parsed.comment, CWD: parsed.cwd, InitiatorThreadID: parsed.initiator,
	})
	if err != nil {
		return fmt.Errorf("создать запуск: %w", err)
	}
	if _, err = fmt.Fprintf(out, "runId: %s\n", snapshot.Meta.RunID); err != nil {
		return err
	}
	return coordinate(ctx, parsed.root, snapshot.Meta.RunID, parsed.executable, out, stderr, deps, false)
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
	return coordinate(ctx, parsed.root, parsed.runID, parsed.executable, out, stderr, deps, true)
}

// coordinate удерживает lock на протяжении preflight, сверки и исполнения.
// Любая ошибка закрывает только владельца хранилища; сохранённые чаты не удаляются.
// resume отличает явное продолжение от первого run: только resume имеет право
// автоматически отправлять continue в interrupted-чаты.
func coordinate(ctx context.Context, root, runID, executable string, out, stderr io.Writer, deps dependencies, resume bool) (err error) {
	run, err := runstore.OpenLocked(root, runID)
	if err != nil {
		return fmt.Errorf("открыть запуск %q: %w", runID, err)
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	snapshot, err := run.Load()
	if err != nil {
		return fmt.Errorf("прочитать запуск %q: %w", runID, err)
	}
	if resume {
		if err = deps.check(ctx, codex.Connection{Executable: executable, CWD: snapshot.Meta.CWD, Stderr: stderr}); err != nil {
			return fmt.Errorf("проверить подключение Codex: %w", err)
		}
	}
	if _, err = fmt.Fprintf(out, "Наблюдение за run %s.\n", runID); err != nil {
		return err
	}
	return coordinator.Execute(ctx, run, coordinator.Options{
		Root: root, PollInterval: deps.pollInterval, Client: deps.client(executable, stderr), ContinueInterrupted: resume,
		Notify: func(status coordinator.Status) error { return printStatus(out, status) },
	})
}

// printStatus выводит полный новый снимок. Execute вызывает функцию только при
// изменении состояния или связи, поэтому одинаковые результаты polling не шумят.
// Все ID кодируются как строковые литералы: workflow и внешний Codex не могут
// добавить ложную строку статуса, возврат каретки или ANSI-команду терминала.
func printStatus(out io.Writer, status coordinator.Status) error {
	for _, step := range status.Steps {
		chat := "—"
		if step.CodexThreadID != "" {
			chat = step.CodexThreadID
		}
		if _, err := fmt.Fprintf(out, "%s: %s; threadId=%s; codexThreadId=%s\n",
			strconv.QuoteToGraphic(step.ID), step.State, strconv.QuoteToGraphic(step.ThreadID), strconv.QuoteToGraphic(chat)); err != nil {
			return err
		}
	}
	if len(status.Waiting) != 0 {
		waiting := make([]string, len(status.Waiting))
		for index, stepID := range status.Waiting {
			waiting[index] = strconv.QuoteToGraphic(stepID)
		}
		if _, err := fmt.Fprintf(out, "Ждут зависимостей: %s.\n", strings.Join(waiting, ", ")); err != nil {
			return err
		}
	}
	if status.Complete {
		_, err := fmt.Fprintf(out, "Run %s успешно завершён.\n", strconv.QuoteToGraphic(status.RunID))
		return err
	}
	return nil
}

// parseRunArguments проверяет обязательные значения и взаимоисключающие формы
// передачи текста, но чтение файлов и UTF-8 оставляет runCommand.
func parseRunArguments(args []string) (runArguments, error) {
	var parsed runArguments
	positionals, values, err := parseOptions(args, map[string]bool{
		"cwd": true, "task": true, "task-file": true, "comment": true, "comment-file": true,
		"initiator-thread-id": true, "root": true, "codex": true,
	})
	if err != nil || len(positionals) != 1 {
		if err != nil {
			return parsed, err
		}
		return parsed, errors.New("использование: lawa run <workflow.json> --cwd <проект> (--task <текст> | --task-file <путь>) --initiator-thread-id <id>")
	}
	parsed.workflow, parsed.cwd, parsed.task = positionals[0], values["cwd"], values["task"]
	parsed.taskFile, parsed.comment = values["task-file"], values["comment"]
	parsed.commentFile, parsed.initiator = values["comment-file"], values["initiator-thread-id"]
	parsed.root, parsed.executable = values["root"], values["codex"]
	if parsed.task != "" && parsed.taskFile != "" || parsed.comment != "" && parsed.commentFile != "" {
		return runArguments{}, errors.New("используйте только один из --task/--task-file и --comment/--comment-file")
	}
	if strings.TrimSpace(parsed.cwd) == "" || strings.TrimSpace(parsed.initiator) == "" || strings.TrimSpace(parsed.task) == "" && parsed.taskFile == "" {
		return runArguments{}, errors.New("run требует --cwd, один из --task/--task-file и --initiator-thread-id")
	}
	return parsed, nil
}

// parseResumeArguments разрешает только постоянный runId и настройки транспорта.
func parseResumeArguments(args []string) (resumeArguments, error) {
	var parsed resumeArguments
	positionals, values, err := parseOptions(args, map[string]bool{"root": true, "codex": true})
	if err != nil || len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		if err != nil {
			return parsed, err
		}
		return parsed, errors.New("использование: lawa resume <run-id> [--root <путь>] [--codex <путь>]")
	}
	parsed.runID, parsed.root, parsed.executable = positionals[0], values["root"], values["codex"]
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
