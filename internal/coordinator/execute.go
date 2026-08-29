package coordinator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/stray-live-pixel/flows-2/internal/codex"
	"github.com/stray-live-pixel/flows-2/internal/runstore"
	"github.com/stray-live-pixel/flows-2/internal/scheduler"
)

// ErrAmbiguousStart означает сохранённое намерение создать чат без его ID.
// Повтор мог бы создать второго исполнителя, поэтому автоматического лечения нет.
var ErrAmbiguousStart = errors.New("создание чата имеет неоднозначный результат")

// Client отделяет алгоритм workflow от транспорта Codex. ProductionClient ниже
// использует официальный app-server, а тесты подставляют детерминированный клиент.
type Client interface {
	Run(context.Context, codex.Command) (codex.Result, error)
	Inspect(context.Context, string, string) (codex.Observation, error)
}

// ProductionClient создаёт отдельные процессы app-server для новых turn и
// коротких thread/read. Параллельность задаёт координатор, собственных лимитов
// здесь нет. Диагностика Codex пишется в Stderr без накопления в памяти Lawa.
type ProductionClient struct {
	Executable string
	Stderr     io.Writer
}

// Run передаёт настройки процесса, не меняя подготовленный prompt и callbacks.
func (c ProductionClient) Run(ctx context.Context, command codex.Command) (codex.Result, error) {
	command.Executable, command.Stderr = c.Executable, c.Stderr
	return codex.Run(ctx, command)
}

// Inspect читает сохранённый чат в его исходной рабочей папке.
func (c ProductionClient) Inspect(ctx context.Context, cwd, threadID string) (codex.Observation, error) {
	return codex.Inspect(ctx, codex.Connection{Executable: c.Executable, CWD: cwd, Stderr: c.Stderr}, threadID)
}

// StepStatus — компактная строка отчёта без prompt, task.md и содержимого памяти.
type StepStatus struct {
	ID, ThreadID, CodexThreadID string
	State                       scheduler.State
}

// Status — целостный снимок для терминала или другого интерфейса. Waiting
// содержит только ещё не запущенные шаги, заблокированные зависимостями.
type Status struct {
	RunID    string
	Steps    []StepStatus
	Waiting  []string
	Complete bool
}

// Options задаёт зависимости длительного исполнения. PollInterval не является
// таймаутом работы: он определяет только частоту чтения ручных продолжений.
// Notify вызывается только при изменении видимого снимка и может остановить
// координатор ошибкой вывода, не запуская новые задачи после этого.
type Options struct {
	Root         string
	PollInterval time.Duration
	Client       Client
	Notify       func(Status) error
}

type launchResult struct {
	stepID string
	result codex.Result
	err    error
}

// Execute наблюдает сохранённый run до успеха всех шагов. Перед каждым новым
// запуском известные чаты сверяются с Codex; затем Prepare атомарно резервирует
// только готовую волну. Результаты независимых turn приходят асинхронно, поэтому
// зависимая ветка может стартовать, не дожидаясь другой долгой ветки графа.
//
// Отмена ctx прекращает опрос и новые запуски. Уже переданные Run получают
// context.WithoutCancel: Lawa не отправляет turn/interrupt и не превращает
// остановку наблюдателя в явную отмену задачи Codex. После завершения процесса
// актуальный статус всё равно восстанавливается через thread/read при resume.
func Execute(ctx context.Context, run *runstore.LockedRun, options Options) (err error) {
	if run == nil || options.Client == nil {
		return errors.New("координатор: нужны открытый запуск и клиент Codex")
	}
	if strings.TrimSpace(options.Root) == "" {
		return errors.New("координатор: нужна папка хранения root")
	}
	if options.PollInterval <= 0 {
		return errors.New("координатор: интервал опроса должен быть положительным")
	}
	initial, err := run.Load()
	if err != nil {
		return fmt.Errorf("координатор: прочитать запуск: %w", err)
	}
	// Буфер равен числу шагов: после остановки наблюдения каждый уже запущенный
	// turn сможет завершить свою горутину, даже если получатель больше не читает.
	results := make(chan launchResult, len(initial.Meta.Steps))
	active := map[string]bool{}
	// Любой выход — сигнал, ошибка вывода, хранения или интеграции — сначала
	// прекращает новые волны, затем сохраняет результаты уже переданных turn.
	defer func() {
		if len(active) != 0 {
			err = drainActive(err, run, active, results)
		}
	}()
	ticker := time.NewTicker(options.PollInterval)
	defer ticker.Stop()
	lastReport := ""

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := reconcile(ctx, run, options.Client, active); err != nil {
			return err
		}
		snapshot, err := run.Load()
		if err != nil {
			return fmt.Errorf("координатор: прочитать запуск: %w", err)
		}
		if err = rejectAmbiguous(snapshot, active); err != nil {
			return err
		}
		prepared, err := Prepare(run, options.Root)
		if err != nil {
			return err
		}
		for _, launch := range prepared.Launches {
			active[launch.StepID] = true
			startLaunch(run, options.Client, context.WithoutCancel(ctx), launch, results)
		}
		status, signature, err := currentStatus(run)
		if err != nil {
			return err
		}
		if signature != lastReport {
			if options.Notify != nil {
				if err = options.Notify(status); err != nil {
					return fmt.Errorf("координатор: сообщить статус: %w", err)
				}
			}
			lastReport = signature
		}
		if status.Complete && len(active) == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case completed := <-results:
			delete(active, completed.stepID)
			if err = saveLaunchResult(run, completed); err != nil {
				return err
			}
		case <-ticker.C:
		}
	}
}

// drainActive не наблюдает остальные сохранённые чаты и не запускает новые
// волны. Он только держит stdio уже запущенных app-server до терминального ответа,
// чтобы выход Lawa не оборвал их транспорт. Поэтому первый сигнал может вернуть
// управление не мгновенно: без собственного фонового daemon это сознательная цена
// гарантии «сигнал не отменяет задачу Codex».
func drainActive(cause error, run *runstore.LockedRun, active map[string]bool, results <-chan launchResult) error {
	for len(active) != 0 {
		completed := <-results
		delete(active, completed.stepID)
		if err := saveLaunchResult(run, completed); err != nil {
			cause = errors.Join(cause, err)
		}
	}
	return cause
}

// startLaunch сохраняет ID чата строго до title и turn/start. Состояние Running
// фиксируется только после turn/started; до него Unknown запрещает и повторный
// чат, и преждевременный запуск зависимостей.
func startLaunch(run *runstore.LockedRun, client Client, ctx context.Context, launch Launch, results chan<- launchResult) {
	go func() {
		command := launch.Command
		var threadID string
		command.OnThread = func(id string) error {
			threadID = id
			return run.Update(launch.StepID, scheduler.Unknown, id)
		}
		command.Notify = func(event codex.Event) error {
			if event.Method == "turn/started" {
				return run.Update(launch.StepID, scheduler.Running, threadID)
			}
			return nil
		}
		result, err := client.Run(ctx, command)
		results <- launchResult{stepID: launch.StepID, result: result, err: err}
	}()
}

// saveLaunchResult переводит внешний терминальный статус во внутреннее состояние.
// Ошибка запроса, требующего пользователя, оставляет тот же чат доступным для
// ручного продолжения. Иные ошибки интеграции останавливают новые запуски после
// сохранения Unknown, потому что их нельзя выдавать за ошибку самого агента.
func saveLaunchResult(run *runstore.LockedRun, completed launchResult) error {
	result, runErr := completed.result, completed.err
	if result.ThreadID == "" {
		if result.CreationAttempted {
			return fmt.Errorf("координатор: шаг %q: %w; ID чата не получен, повтор запрещён", completed.stepID, ErrAmbiguousStart)
		}
		return fmt.Errorf("координатор: шаг %q: Codex не начал создание чата: %w", completed.stepID, runErr)
	}
	state := scheduler.Unknown
	if runErr == nil {
		var err error
		state, err = stateFromResult(result)
		if err != nil {
			return fmt.Errorf("координатор: шаг %q: %w", completed.stepID, err)
		}
	} else {
		var interaction *codex.InteractionRequired
		if errors.As(runErr, &interaction) {
			state = scheduler.WaitingForApproval
		}
	}
	if err := run.Update(completed.stepID, state, result.ThreadID); err != nil {
		return fmt.Errorf("координатор: сохранить результат шага %q: %w", completed.stepID, err)
	}
	if runErr != nil && state != scheduler.WaitingForApproval {
		return fmt.Errorf("координатор: шаг %q, чат %q: %w", completed.stepID, result.ThreadID, runErr)
	}
	return nil
}

// stateFromResult принимает только терминальные значения одного нового turn.
func stateFromResult(result codex.Result) (scheduler.State, error) {
	switch result.Status {
	case "completed":
		return scheduler.Succeeded, nil
	case "failed":
		return scheduler.Failed, nil
	case "interrupted":
		return scheduler.Cancelled, nil
	default:
		return "", fmt.Errorf("неизвестный терминальный статус Codex %q", result.Status)
	}
}

// reconcile параллельно читает все известные неактивные чаты. Активные вызовы
// Run уже получают события из собственного app-server; второй thread/read для
// них только создавал бы гонку между двумя источниками одного статуса.
func reconcile(ctx context.Context, run *runstore.LockedRun, client Client, active map[string]bool) error {
	snapshot, err := run.Load()
	if err != nil {
		return fmt.Errorf("координатор: прочитать запуск перед сверкой: %w", err)
	}
	type inspected struct {
		step        runstore.Step
		observation codex.Observation
		err         error
	}
	count := 0
	results := make(chan inspected, len(snapshot.Meta.Steps))
	for _, step := range snapshot.Meta.Steps {
		if step.CodexThreadID == "" || active[step.ID] {
			continue
		}
		count++
		go func(step runstore.Step) {
			observation, inspectErr := client.Inspect(ctx, snapshot.Meta.CWD, step.CodexThreadID)
			results <- inspected{step: step, observation: observation, err: inspectErr}
		}(step)
	}
	for range count {
		item := <-results
		if item.err != nil {
			return fmt.Errorf("координатор: прочитать чат шага %q: %w", item.step.ID, item.err)
		}
		workStatus, statusErr := item.observation.Status()
		if statusErr != nil {
			return fmt.Errorf("координатор: прочитать статус шага %q: %w", item.step.ID, statusErr)
		}
		state, statusErr := stateFromObservation(workStatus)
		if statusErr != nil {
			return fmt.Errorf("координатор: шаг %q: %w", item.step.ID, statusErr)
		}
		if state != item.step.State {
			if err := run.Update(item.step.ID, state, item.step.CodexThreadID); err != nil {
				return fmt.Errorf("координатор: сохранить статус шага %q: %w", item.step.ID, err)
			}
		}
	}
	return nil
}

// stateFromObservation преобразует уже проверенный снимок существующего чата.
func stateFromObservation(status codex.WorkStatus) (scheduler.State, error) {
	switch status {
	case codex.WorkUnknown:
		return scheduler.Unknown, nil
	case codex.WorkRunning:
		return scheduler.Running, nil
	case codex.WorkWaitingForApproval:
		return scheduler.WaitingForApproval, nil
	case codex.WorkFailed:
		return scheduler.Failed, nil
	case codex.WorkInterrupted:
		return scheduler.Cancelled, nil
	case codex.WorkCompleted:
		return scheduler.Succeeded, nil
	default:
		return "", fmt.Errorf("неизвестный результат наблюдения %q", status)
	}
}

// rejectAmbiguous запрещает любые новые запросы, если прежний thread/start мог
// состояться, но постоянная связь с его результатом отсутствует.
func rejectAmbiguous(snapshot runstore.Snapshot, active map[string]bool) error {
	for _, step := range snapshot.Meta.Steps {
		if step.State == scheduler.Starting && step.CodexThreadID == "" && !active[step.ID] {
			return fmt.Errorf("координатор: шаг %q: %w; сохранено Starting без codexThreadId", step.ID, ErrAmbiguousStart)
		}
	}
	return nil
}

// currentStatus строит отчёт из уже сохранённого снимка. Signature включает
// состояния и связи, поэтому периодический опрос без изменений не засоряет вывод.
func currentStatus(run *runstore.LockedRun) (Status, string, error) {
	snapshot, err := run.Load()
	if err != nil {
		return Status{}, "", fmt.Errorf("координатор: прочитать статус запуска: %w", err)
	}
	states := make(map[string]scheduler.State, len(snapshot.Meta.Steps))
	status := Status{RunID: snapshot.Meta.RunID}
	var signature strings.Builder
	for _, step := range snapshot.Meta.Steps {
		states[step.ID] = step.State
		status.Steps = append(status.Steps, StepStatus{
			ID: step.ID, ThreadID: step.ThreadID, CodexThreadID: step.CodexThreadID, State: step.State,
		})
		fmt.Fprintf(&signature, "%s=%s:%s;", step.ID, step.State, step.CodexThreadID)
	}
	plan, err := scheduler.Evaluate(snapshot.Workflow, states)
	if err != nil {
		return Status{}, "", fmt.Errorf("координатор: построить статус: %w", err)
	}
	status.Waiting, status.Complete = plan.Waiting, plan.Complete
	fmt.Fprintf(&signature, "waiting=%s;complete=%t", strings.Join(plan.Waiting, ","), plan.Complete)
	return status, signature.String(), nil
}
