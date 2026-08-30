package coordinator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// DefaultRefreshInterval ограничивает паузу между повторными снимками активного
// workflow. Изменение состояния передаётся сразу и не ждёт этого интервала.
// Production использует периодический снимок для обновления локального Markdown
// и изображения, а частота коротких сообщений в чат задаётся отдельно в CLI.
const DefaultRefreshInterval = time.Minute

// ErrAmbiguousStart означает сохранённое намерение создать чат без его ID.
// Повтор мог бы создать второго исполнителя, поэтому автоматического лечения нет.
var ErrAmbiguousStart = errors.New("создание чата имеет неоднозначный результат")

// Client отделяет алгоритм workflow от транспорта Codex. ProductionClient ниже
// использует официальный app-server, а тесты подставляют детерминированный клиент.
type Client interface {
	Run(context.Context, codex.Command) (codex.Result, error)
	Continue(context.Context, string, codex.Command) (codex.Result, error)
	OpenObserver(context.Context, string) (Observer, error)
}

// Observer — read-only сессия для последовательной сверки сохранённых чатов.
// Close относится только к её процессу и не должен останавливать активные turn.
type Observer interface {
	Inspect(string) (codex.Observation, error)
	Close() error
}

// sharedObserver лениво открывает транспорт при первом известном неактивном чате.
// Новый run поэтому не получает лишнюю точку отказа до запуска первого turn, а
// последующие polling-циклы всё равно переиспользуют одну и ту же сессию.
type sharedObserver struct {
	ctx      context.Context
	client   Client
	cwd      string
	observer Observer
}

func (o *sharedObserver) Inspect(threadID string) (codex.Observation, error) {
	if o.observer == nil {
		observer, err := o.client.OpenObserver(o.ctx, o.cwd)
		if err != nil {
			return codex.Observation{}, fmt.Errorf("открыть наблюдение Codex: %w", err)
		}
		o.observer = observer
	}
	return o.observer.Inspect(threadID)
}

func (o *sharedObserver) Close() error {
	if o == nil || o.observer == nil {
		return nil
	}
	return o.observer.Close()
}

// ProductionClient создаёт отдельный app-server для каждого нового turn и одну
// наблюдающую сессию на Execute. Диагностика Codex пишется в Stderr без накопления
// в памяти Lawa.
type ProductionClient struct {
	Executable string
	Stderr     io.Writer
}

// Run передаёт настройки процесса, не меняя подготовленный prompt и callbacks.
func (c ProductionClient) Run(ctx context.Context, command codex.Command) (codex.Result, error) {
	command.Executable, command.Stderr = c.Executable, c.Stderr
	return codex.Run(ctx, command)
}

// Continue запускает один новый turn в уже сохранённом чате.
func (c ProductionClient) Continue(ctx context.Context, threadID string, command codex.Command) (codex.Result, error) {
	command.Executable, command.Stderr = c.Executable, c.Stderr
	return codex.Continue(ctx, threadID, command)
}

// OpenObserver открывает одну read-only сессию для всех polling-циклов Execute.
func (c ProductionClient) OpenObserver(ctx context.Context, cwd string) (Observer, error) {
	return codex.OpenObserver(ctx, codex.Connection{Executable: c.Executable, CWD: cwd, Stderr: c.Stderr})
}

// StepStatus — компактная строка отчёта без prompt, task.md и содержимого памяти.
type StepStatus struct {
	ID, ThreadID, CodexThreadID string
	State                       scheduler.State
	DependsOn                   []string
}

// Status — целостный снимок для терминала или другого интерфейса. Waiting
// содержит только ещё не запущенные шаги, заблокированные зависимостями.
type Status struct {
	RunID, WorkflowID string
	Steps             []StepStatus
	Waiting           []string
	Complete          bool
}

// Ticker скрывает реальное время за минимальной границей. Production использует
// time.Ticker, а тесты статуса вручную подают события без ожидания интервала.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// realTicker адаптирует стандартный time.Ticker к тестируемому интерфейсу.
type realTicker struct{ ticker *time.Ticker }

// C возвращает канал событий стандартного ticker без дополнительной горутины.
func (t realTicker) C() <-chan time.Time { return t.ticker.C }

// Stop освобождает системный timer при любом выходе из Execute.
func (t realTicker) Stop() { t.ticker.Stop() }

// Options задаёт зависимости длительного исполнения. PollInterval не является
// таймаутом работы: он определяет только частоту чтения ручных продолжений.
// RefreshInterval задаёт максимальную паузу между одинаковыми снимками; изменение
// состояния по-прежнему публикуется сразу. NewTicker существует для управляемых
// часов в тестах. Ошибка Notify означает отказ пользовательского интерфейса и
// останавливает координатор, не запуская новые задачи после этого.
type Options struct {
	Root                string
	PollInterval        time.Duration
	RefreshInterval     time.Duration
	Client              Client
	Notify              func(Status) error
	ContinueInterrupted bool
	NewTicker           func(time.Duration) Ticker
}

type launchResult struct {
	stepID string
	result codex.Result
	err    error
}

// activeExecution связывает локальную горутину с точными ID внешнего turn.
// Колбэки клиента пишут ID параллельно главному циклу, поэтому поля защищены.
// ready закрывается либо после получения turnId, либо при раннем завершении:
// остановка не ждёт ID бесконечно и не отправляет interrupt наугад.
type activeExecution struct {
	mu               sync.Mutex
	cancel           context.CancelFunc
	interrupt        func(context.Context) error
	threadID, turnID string
	ready            chan struct{}
	readyOnce        sync.Once
	done             bool
}

func newActiveExecution(cancel context.CancelFunc, threadID string) *activeExecution {
	return &activeExecution{cancel: cancel, threadID: threadID, ready: make(chan struct{})}
}

func (a *activeExecution) setThread(id string) {
	a.mu.Lock()
	a.threadID = id
	a.mu.Unlock()
}

// setTurn атомарно сохраняет ID и функцию interrupt исходной stdio-сессии.
// ready закрывается только после обоих значений: обработчик сигнала не увидит
// turnId без способа адресно остановить именно этот turn.
func (a *activeExecution) setTurn(id string, interrupt func(context.Context) error) {
	a.mu.Lock()
	a.turnID = id
	a.interrupt = interrupt
	a.mu.Unlock()
	a.readyOnce.Do(func() { close(a.ready) })
}

func (a *activeExecution) finish() {
	a.mu.Lock()
	a.done = true
	a.mu.Unlock()
	a.readyOnce.Do(func() { close(a.ready) })
}

func (a *activeExecution) snapshot() (threadID, turnID string, interrupt func(context.Context) error, done bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.threadID, a.turnID, a.interrupt, a.done
}

// Execute наблюдает сохранённый run до успеха всех шагов. Перед каждым новым
// запуском известные чаты сверяются с Codex; затем Prepare атомарно резервирует
// только готовую волну. Результаты независимых turn приходят асинхронно, поэтому
// зависимая ветка может стартовать, не дожидаясь другой долгой ветки графа.
//
// Отмена ctx прекращает опрос и новые запуски, затем адресно отправляет
// turn/interrupt всем уже начатым turn. Их stdio-сессии получают терминальный
// status=interrupted и сохраняют Cancelled. Следующий resume запускает ровно один
// turn с текстом continue только для такого состояния; failed и succeeded не
// повторяются автоматически.
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
	if options.RefreshInterval <= 0 {
		// Внутренние вызовы до появления периодического отчёта не задавали это
		// поле. Единый default сохраняет минутное обновление локальных файлов.
		options.RefreshInterval = DefaultRefreshInterval
	}
	if options.NewTicker == nil {
		options.NewTicker = func(interval time.Duration) Ticker {
			return realTicker{ticker: time.NewTicker(interval)}
		}
	}
	initial, err := run.Load()
	if err != nil {
		return fmt.Errorf("координатор: прочитать запуск: %w", err)
	}
	observer := &sharedObserver{ctx: ctx, client: options.Client, cwd: initial.Meta.CWD}
	// Закрытие наблюдения регистрируется до defer активных turn ниже. На Ctrl+C
	// координатор поэтому сначала отправит адресные interrupt через владеющие
	// сессии и только затем закроет независимый read-only процесс. Если известных
	// чатов ещё нет, процесс не запускается вовсе.
	defer func() { err = errors.Join(err, observer.Close()) }()
	// Буфер равен числу шагов: после остановки наблюдения каждый уже запущенный
	// turn сможет завершить свою горутину, даже если получатель больше не читает.
	results := make(chan launchResult, len(initial.Meta.Steps))
	active := map[string]*activeExecution{}
	continued := map[string]bool{}
	// Любой выход — сигнал, ошибка вывода, хранения или интеграции — сначала
	// прекращает новые волны. Сигнал явно прерывает turn; внутренняя ошибка по-
	// прежнему сохраняет уже получаемые результаты, не подменяя первичную причину.
	defer func() {
		if len(active) != 0 {
			if cause := ctx.Err(); cause != nil {
				err = interruptActive(errors.Join(err, cause), run, active, results)
			} else {
				err = drainActive(err, run, active, results)
			}
		}
	}()
	pollTicker := options.NewTicker(options.PollInterval)
	refreshTicker := options.NewTicker(options.RefreshInterval)
	defer pollTicker.Stop()
	defer refreshTicker.Stop()
	lastSnapshot := ""
	periodicRefreshDue := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := reconcile(run, observer, active); err != nil {
			return err
		}
		snapshot, err := run.Load()
		if err != nil {
			return fmt.Errorf("координатор: прочитать запуск: %w", err)
		}
		if err = rejectAmbiguous(snapshot, active); err != nil {
			return err
		}
		continuations, err := prepareContinuations(snapshot, options.Root, options.ContinueInterrupted, continued)
		if err != nil {
			return err
		}
		prepared, err := Prepare(run, options.Root)
		if err != nil {
			return err
		}
		for _, continuation := range continuations {
			continued[continuation.StepID] = true
			turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			execution := newActiveExecution(cancel, continuation.ThreadID)
			active[continuation.StepID] = execution
			startContinuation(run, options.Client, turnCtx, continuation, execution, results)
		}
		for _, launch := range prepared.Launches {
			turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			execution := newActiveExecution(cancel, "")
			active[launch.StepID] = execution
			startLaunch(run, options.Client, turnCtx, launch, execution, results)
		}
		status, signature, err := currentStatus(run)
		if err != nil {
			return err
		}
		if signature != lastSnapshot || periodicRefreshDue {
			if options.Notify != nil {
				if err = options.Notify(status); err != nil {
					return fmt.Errorf("координатор: сообщить статус: %w", err)
				}
			}
			lastSnapshot = signature
			periodicRefreshDue = false
		}
		if status.Complete && len(active) == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case completed := <-results:
			finishExecution(active, completed.stepID)
			if err = saveLaunchResult(run, completed); err != nil {
				return err
			}
		case <-pollTicker.C():
		case <-refreshTicker.C():
			periodicRefreshDue = true
		}
	}
}

// drainActive не наблюдает остальные сохранённые чаты и не запускает новые волны.
// Для внутренних ошибок он сохраняет терминальные ответы уже переданных turn.
func drainActive(cause error, run *runstore.LockedRun, active map[string]*activeExecution, results <-chan launchResult) error {
	for len(active) != 0 {
		completed := <-results
		finishExecution(active, completed.stepID)
		if err := saveLaunchResult(run, completed); err != nil {
			cause = errors.Join(cause, err)
		}
	}
	return cause
}

const interruptGracePeriod = 5 * time.Second

type interruptResult struct {
	stepID string
	err    error
}

// interruptActive даёт каждому turn ограниченное время на явный interrupt и
// терминальное событие. Если app-server не подтверждает остановку, локальная
// stdio-сессия отменяется как аварийный fallback: Ctrl+C всё равно обязан вернуть
// управление, а следующий resume сверит фактический статус через thread/read.
func interruptActive(cause error, run *runstore.LockedRun, active map[string]*activeExecution, results <-chan launchResult) error {
	shutdown, cancel := context.WithTimeout(context.Background(), interruptGracePeriod)
	defer cancel()
	interrupts := make(chan interruptResult, len(active))
	for stepID, execution := range active {
		go func(stepID string, execution *activeExecution) {
			select {
			case <-execution.ready:
				threadID, turnID, interrupt, done := execution.snapshot()
				if done {
					interrupts <- interruptResult{stepID: stepID}
					return
				}
				if threadID == "" || turnID == "" || interrupt == nil {
					interrupts <- interruptResult{stepID: stepID, err: errors.New("активный turn не передал interrupt исходной сессии")}
					return
				}
				interrupts <- interruptResult{stepID: stepID, err: interrupt(shutdown)}
			case <-shutdown.Done():
				interrupts <- interruptResult{stepID: stepID, err: shutdown.Err()}
			}
		}(stepID, execution)
	}

	interruptErrors := make(map[string]error, len(active))
	for len(active) != 0 {
		select {
		case completed := <-results:
			finishExecution(active, completed.stepID)
			delete(interruptErrors, completed.stepID)
			if err := saveLaunchResult(run, completed); err != nil {
				cause = errors.Join(cause, err)
			}
		case interrupted := <-interrupts:
			if interrupted.err != nil && active[interrupted.stepID] != nil {
				// Ошибка может означать, что turn успел завершиться прямо перед RPC.
				// Не убиваем исходную сессию сразу: её терминальное событие надёжнее
				// ответа конкурентной отмены. Ошибка станет значимой только по timeout.
				interruptErrors[interrupted.stepID] = interrupted.err
			}
		case <-shutdown.Done():
			cause = errors.Join(cause, fmt.Errorf("координатор: остановить активные turn: %w", shutdown.Err()))
			for stepID, execution := range active {
				if interruptErr := interruptErrors[stepID]; interruptErr != nil {
					cause = errors.Join(cause, fmt.Errorf("координатор: отменить turn шага %q: %w", stepID, interruptErr))
				}
				execution.cancel()
			}
			return drainActive(cause, run, active, results)
		}
	}
	return cause
}

func finishExecution(active map[string]*activeExecution, stepID string) {
	if execution := active[stepID]; execution != nil {
		execution.cancel()
		delete(active, stepID)
	}
}

// startLaunch сохраняет ID чата строго до title и turn/start. Состояние Running
// фиксируется только после turn/started; до него Unknown запрещает и повторный
// чат, и преждевременный запуск зависимостей.
func startLaunch(run *runstore.LockedRun, client Client, ctx context.Context, launch Launch, execution *activeExecution, results chan<- launchResult) {
	go func() {
		defer execution.finish()
		command := launch.Command
		var threadID string
		command.OnThread = func(id string) error {
			threadID = id
			if err := run.Update(launch.StepID, scheduler.Unknown, id); err != nil {
				return err
			}
			execution.setThread(id)
			return nil
		}
		command.OnTurn = execution.setTurn
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

// startContinuation использует прежний thread и не создаёт новый чат. Running
// сохраняется только после turn/started; ошибка до него останется Unknown и будет
// сверена по истории при следующем resume, а не повторена в текущем процессе.
func startContinuation(run *runstore.LockedRun, client Client, ctx context.Context, continuation Continuation, execution *activeExecution, results chan<- launchResult) {
	go func() {
		defer execution.finish()
		command := continuation.Command
		command.OnTurn = execution.setTurn
		command.Notify = func(event codex.Event) error {
			if event.Method == "turn/started" {
				return run.Update(continuation.StepID, scheduler.Running, continuation.ThreadID)
			}
			return nil
		}
		result, err := client.Continue(ctx, continuation.ThreadID, command)
		results <- launchResult{stepID: continuation.StepID, result: result, err: err}
	}()
}

// saveLaunchResult переводит внешний терминальный статус во внутреннее состояние.
// Ошибка запроса, требующего пользователя, оставляет тот же чат доступным для
// ручного продолжения. Иные ошибки интеграции останавливают новые запуски после
// сохранения Unknown, потому что их нельзя выдавать за ошибку самого агента.
// Единственное исключение — достоверный отказ до thread/start: чата ещё нет,
// поэтому резервирование безопасно снимается для следующего явного resume.
func saveLaunchResult(run *runstore.LockedRun, completed launchResult) error {
	result, runErr := completed.result, completed.err
	if result.ThreadID == "" {
		if result.CreationAttempted {
			return fmt.Errorf("координатор: шаг %q: %w; Codex мог создать чат, но не вернул его ID; "+
				"автоматический повтор запрещён из-за риска дубликата — не запускайте новый run, "+
				"сообщите пользователю runId, ID шага и эту ошибку для диагностики Codex", completed.stepID, ErrAmbiguousStart)
		}
		if runErr == nil {
			runErr = errors.New("Codex завершил операцию без результата и без причины")
		}
		codexErr := fmt.Errorf("координатор: шаг %q: Codex не начал создание чата: %w", completed.stepID, runErr)
		if releaseErr := run.ReleaseUnattempted(completed.stepID); releaseErr != nil {
			return errors.Join(codexErr, fmt.Errorf("вернуть шаг в Pending: %w; не повторяйте создание автоматически, "+
				"сообщите пользователю runId, ID шага и обе ошибки", releaseErr))
		}
		return fmt.Errorf("%w; чат не создан, шаг возвращён в Pending — устраните ошибку Codex и повторите "+
			"`lawa resume <runId>` с ранее напечатанным runId", codexErr)
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

// reconcile последовательно читает все известные неактивные чаты через одну
// наблюдающую сессию. Один Decoder нельзя читать конкурентно; последовательность
// сохраняет простую маршрутизацию RPC и убирает процесс на каждый чат. Активные
// Run получают события из собственного app-server и здесь не опрашиваются.
func reconcile(run *runstore.LockedRun, observer Observer, active map[string]*activeExecution) error {
	snapshot, err := run.Load()
	if err != nil {
		return fmt.Errorf("координатор: прочитать запуск перед сверкой: %w", err)
	}
	for _, step := range snapshot.Meta.Steps {
		if step.CodexThreadID == "" || active[step.ID] != nil {
			continue
		}
		observation, inspectErr := observer.Inspect(step.CodexThreadID)
		if inspectErr != nil {
			return fmt.Errorf("координатор: прочитать чат шага %q: %w", step.ID, inspectErr)
		}
		workStatus, statusErr := observation.Status()
		if statusErr != nil {
			return fmt.Errorf("координатор: прочитать статус шага %q: %w", step.ID, statusErr)
		}
		state, statusErr := stateFromObservation(workStatus)
		if statusErr != nil {
			return fmt.Errorf("координатор: шаг %q: %w", step.ID, statusErr)
		}
		if state != step.State {
			if err := run.Update(step.ID, state, step.CodexThreadID); err != nil {
				return fmt.Errorf("координатор: сохранить статус шага %q: %w", step.ID, err)
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
func rejectAmbiguous(snapshot runstore.Snapshot, active map[string]*activeExecution) error {
	for _, step := range snapshot.Meta.Steps {
		if step.State == scheduler.Starting && step.CodexThreadID == "" && active[step.ID] == nil {
			return fmt.Errorf("координатор: шаг %q: %w; сохранено Starting без codexThreadId — Codex мог создать чат, "+
				"поэтому автоматический повтор запрещён из-за риска дубликата; не запускайте новый run, "+
				"сообщите пользователю runId, ID шага и эту ошибку для диагностики Codex", step.ID, ErrAmbiguousStart)
		}
	}
	return nil
}

// currentStatus строит отчёт из одного уже сохранённого снимка. Список и порядок
// шагов берутся из meta.json, а зависимости — из неизменяемого workflow.json того
// же Snapshot. Поэтому пользовательский текст и визуализация не могут разойтись
// из-за изменения meta.json между двумя чтениями.
func currentStatus(run *runstore.LockedRun) (Status, string, error) {
	snapshot, err := run.Load()
	if err != nil {
		return Status{}, "", fmt.Errorf("координатор: прочитать статус запуска: %w", err)
	}
	states := make(map[string]scheduler.State, len(snapshot.Meta.Steps))
	status := Status{RunID: snapshot.Meta.RunID, WorkflowID: snapshot.Workflow.ID}
	dependencies := make(map[string][]string, len(snapshot.Workflow.Steps))
	for _, step := range snapshot.Workflow.Steps {
		dependencies[step.ID] = step.DependsOn
	}
	var signature strings.Builder
	for _, step := range snapshot.Meta.Steps {
		states[step.ID] = step.State
		status.Steps = append(status.Steps, StepStatus{
			ID: step.ID, ThreadID: step.ThreadID, CodexThreadID: step.CodexThreadID, State: step.State,
			DependsOn: append([]string(nil), dependencies[step.ID]...),
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
