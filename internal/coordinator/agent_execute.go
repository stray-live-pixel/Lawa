package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// agentLaunchResult адресуется только visitId. StepId намеренно отсутствует в
// ключе: последующие итерации одного кубика являются разными исполнениями.
type agentLaunchResult struct {
	visitID string
	result  codex.Result
	err     error
}

// executeAgentGraph исполняет workflow v2 поверх append-only meta v4; допустимые
// формы графа и пределы повторных посещений остаются ответственностью planner.
// На каждой итерации сначала сверяются уже известные чаты, затем одним durable
// commit применяется чистый план и лишь после этого резервируется новая работа.
// Такой порядок не позволяет crash-resume повторно материализовать маршрут или
// запустить Pending, если более раннее решение уже завершило весь run.
func executeAgentGraph(ctx context.Context, run *runstore.LockedRun, options Options, initial runstore.Snapshot) (outcome Outcome, err error) {
	observer := &sharedObserver{ctx: ctx, client: options.Client, cwd: initial.Meta.CWD}
	defer func() { err = errors.Join(err, observer.Close()) }()

	// В корректной metadata одновременно активно не больше одного visit каждого
	// step, поэтому числа кубиков достаточно, чтобы завершение горутин никогда не
	// зависело от читающего coordinator во время аварийного drain.
	results := make(chan agentLaunchResult, max(1, len(initial.Workflow.Steps)))
	active := map[string]*activeExecution{}
	continued := map[string]bool{}
	defer func() {
		if cause := ctx.Err(); len(active) != 0 && (cause != nil || err != nil) {
			err = interruptAgentActive(errors.Join(err, cause), run, active, results)
		} else if len(active) != 0 {
			err = drainAgentActive(err, run, active, results)
		}
		// Любая ошибка сначала останавливает и сохраняет все локальные turn. После
		// этого callbacks уже не могут добавить новый poison, поэтому recovery
		// детерминированно предпочитает durable conflict исходной transport-ошибке.
		if err == nil || outcome.Terminal {
			return
		}
		snapshot, loadErr := run.Load()
		if loadErr != nil || !agentSnapshotPoisoned(snapshot) {
			err = errors.Join(err, loadErr)
			return
		}
		advanced, advanceErr := run.AdvanceAgentGraph()
		if advanceErr != nil || advanced.Snapshot.Meta.RunState == runstore.RunRunning {
			err = errors.Join(err, advanceErr, errors.New("координатор agent-graph: не удалось завершить сохранённый конфликт после остановки turn"))
			return
		}
		terminalOutcome, terminalErr := finishAgentGraph(run, options, active, results)
		outcome, err = terminalOutcome, errors.Join(err, terminalErr)
	}()

	pollTicker := options.NewTicker(options.PollInterval)
	refreshTicker := options.NewTicker(options.RefreshInterval)
	defer pollTicker.Stop()
	defer refreshTicker.Stop()
	lastStatus, periodicRefreshDue := "", false

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return outcome, ctxErr
		}
		snapshot, loadErr := run.Load()
		if loadErr != nil {
			return outcome, fmt.Errorf("координатор agent-graph: прочитать запуск: %w", loadErr)
		}
		if snapshot.Meta.RunState != runstore.RunRunning {
			return finishAgentGraph(run, options, active, results)
		}
		// Poison имеет приоритет над неопределённым Starting после crash. Он уже
		// доказывает failed всего run, поэтому повторять незавершённое создание не
		// требуется: planner сначала публикует terminal и запрещает любую сеть.
		if agentSnapshotPoisoned(snapshot) {
			advanced, advanceErr := run.AdvanceAgentGraph()
			if advanceErr != nil {
				return outcome, fmt.Errorf("координатор agent-graph: завершить сохранённый конфликт: %w", advanceErr)
			}
			if advanced.Snapshot.Meta.RunState == runstore.RunRunning {
				return outcome, errors.New("координатор agent-graph: planner не завершил сохранённый конфликт")
			}
			return finishAgentGraph(run, options, active, results)
		}
		if rejectErr := rejectAgentAmbiguous(snapshot, active); rejectErr != nil {
			return outcome, rejectErr
		}
		if reconcileErr := reconcileAgentVisits(run, observer, active, continued); reconcileErr != nil {
			return outcome, reconcileErr
		}

		advanced, advanceErr := run.AdvanceAgentGraph()
		if advanceErr != nil {
			return outcome, fmt.Errorf("координатор agent-graph: продвинуть граф: %w", advanceErr)
		}
		if advanced.Snapshot.Meta.RunState != runstore.RunRunning {
			return finishAgentGraph(run, options, active, results)
		}
		prepared, prepareErr := prepareAgentVisits(
			run, options.Root, options.Capacity, options.ContinueInterrupted, continued, options.ConfigureCommand,
		)
		if prepareErr != nil {
			var terminalPrepare *agentTerminalPreparationError
			if errors.As(prepareErr, &terminalPrepare) {
				terminalOutcome, terminalErr := finishAgentGraph(run, options, active, results)
				return terminalOutcome, errors.Join(terminalErr, terminalPrepare.Cleanup)
			}
			return outcome, prepareErr
		}
		for _, work := range prepared.Work {
			turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			execution := newActiveExecution(cancel, work.ThreadID, work.lease)
			active[work.VisitID] = execution
			if work.kind == agentWorkContinuation {
				// Флаг публикуется только после регистрации active: с этого места
				// lease и результат принадлежат общему shutdown-протоколу.
				continued[work.VisitID] = true
			}
			startAgentWork(run, options.Client, turnCtx, work, execution, results)
		}
		// Все запросы одной уже зарезервированной волны должны пройти границу
		// OnTurn (либо завершиться раньше), прежде чем coordinator применит finish
		// из быстрого соседа. Иначе RunState успел бы стать terminal, а поздний
		// SetVisitTurn параллельного visit уже нельзя было бы сохранить и адресно
		// прервать. Это не сериализует сами turn: ожидание идёт после их запуска.
		if readyErr := waitAgentExecutionsReady(ctx, active); readyErr != nil {
			return outcome, readyErr
		}

		status, signature, statusErr := currentStatus(run)
		if statusErr != nil {
			return outcome, statusErr
		}
		status.WaitingForCapacity = append([]string(nil), prepared.WaitingForCapacity...)
		signature += ";capacity=" + strings.Join(status.WaitingForCapacity, ",")
		if signature != lastStatus || periodicRefreshDue {
			if options.Notify != nil {
				if notifyErr := options.Notify(status); notifyErr != nil {
					return outcome, fmt.Errorf("координатор agent-graph: сообщить статус: %w", notifyErr)
				}
			}
			lastStatus, periodicRefreshDue = signature, false
		}

		select {
		case <-ctx.Done():
			return outcome, ctx.Err()
		case completed := <-results:
			releaseErr := finishAgentExecution(active, completed.visitID)
			saveErr := saveAgentLaunchResult(run, completed)
			if resultErr := errors.Join(releaseErr, saveErr); resultErr != nil {
				return outcome, fmt.Errorf("координатор agent-graph: завершить посещение %q: %w", completed.visitID, resultErr)
			}
		case <-pollTicker.C():
		case <-refreshTicker.C():
			periodicRefreshDue = true
		}
	}
}

// agentSnapshotPoisoned распознаёт только уже durable конфликт tool calls.
func agentSnapshotPoisoned(snapshot runstore.Snapshot) bool {
	for _, visit := range snapshot.Meta.Visits {
		if visit.Decision != nil && visit.Decision.Error != "" {
			return true
		}
	}
	return false
}

// waitAgentExecutionsReady закрывает гонку между двумя уже запущенными запросами:
// возврат означает, что каждый из них либо сохранил turnId, либо уже дал Result.
func waitAgentExecutionsReady(ctx context.Context, active map[string]*activeExecution) error {
	for _, execution := range active {
		select {
		case <-execution.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// finishAgentGraph прекращает оставшиеся параллельные turn после уже durable
// terminal outcome. Их Cancelled/terminal результаты сохраняются для аудита,
// но не могут изменить RunState или породить новые маршруты.
func finishAgentGraph(run *runstore.LockedRun, options Options, active map[string]*activeExecution, results <-chan agentLaunchResult) (Outcome, error) {
	snapshot, err := run.Load()
	if err != nil {
		return Outcome{}, fmt.Errorf("координатор agent-graph: прочитать terminal run: %w", err)
	}
	outcome := Outcome{Terminal: true, Successful: snapshot.Meta.RunState == runstore.RunSucceeded}
	if len(active) != 0 {
		err = interruptAgentActive(err, run, active, results)
	}
	status, _, statusErr := currentStatus(run)
	err = errors.Join(err, statusErr)
	if statusErr == nil && options.Notify != nil {
		err = errors.Join(err, options.Notify(status))
	}
	if !outcome.Successful {
		err = errors.Join(err, ErrRunUnsuccessful)
	}
	return outcome, err
}

// rejectAgentAmbiguous распознаёт только потерянный ответ создания после
// рестарта. Локальная горутина остаётся в active и сама докажет через Result,
// можно ли безопасно снять резервирование.
func rejectAgentAmbiguous(snapshot runstore.Snapshot, active map[string]*activeExecution) error {
	for _, visit := range snapshot.Meta.Visits {
		if visit.State == scheduler.Starting && visit.CodexThreadID == "" && active[visit.VisitID] == nil {
			return fmt.Errorf("координатор agent-graph: посещение %q шага %q: %w; сохранено Starting без codexThreadId — "+
				"Codex мог создать чат, поэтому автоматический повтор запрещён; сообщите runId и visitId для диагностики",
				visit.VisitID, visit.StepID, ErrAmbiguousStart)
		}
	}
	return nil
}

// reconcileAgentVisits читает только неактивные и ещё не терминальные чаты.
// Новый turn сначала сохраняется через SetVisitTurn и лишь затем его состояние:
// crash между commit сохраняет Unknown/Cancelled, но никогда Succeeded без
// доказанного turnId. Обнаруженный внешний turn считается продолжением этого
// процесса и не разрешает вслед за ним отправить второй автоматический continue.
func reconcileAgentVisits(run *runstore.LockedRun, observer Observer, active map[string]*activeExecution, continued map[string]bool) error {
	snapshot, err := run.Load()
	if err != nil {
		return fmt.Errorf("координатор agent-graph: прочитать запуск перед сверкой: %w", err)
	}
	for _, visit := range snapshot.Meta.Visits {
		if visit.CodexThreadID == "" || active[visit.VisitID] != nil || visit.State == scheduler.Succeeded || visit.State == scheduler.Failed ||
			visit.Decision != nil && visit.Decision.Error != "" {
			continue
		}
		observation, inspectErr := observer.Inspect(visit.CodexThreadID)
		if inspectErr != nil {
			return fmt.Errorf("координатор agent-graph: прочитать чат посещения %q: %w", visit.VisitID, inspectErr)
		}
		workStatus, statusErr := observation.Status()
		if statusErr != nil {
			return fmt.Errorf("координатор agent-graph: прочитать статус посещения %q: %w", visit.VisitID, statusErr)
		}
		state, stateErr := stateFromObservation(workStatus)
		if stateErr != nil {
			return fmt.Errorf("координатор agent-graph: посещение %q: %w", visit.VisitID, stateErr)
		}
		oldState := visit.State
		if oldState == scheduler.Starting {
			if err = run.UpdateVisit(visit.VisitID, scheduler.Unknown, visit.CodexThreadID, ""); err != nil {
				return fmt.Errorf("координатор agent-graph: восстановить связь посещения %q: %w", visit.VisitID, err)
			}
			oldState = scheduler.Unknown
		}
		turnChanged := observation.LatestTurnID != "" && observation.LatestTurnID != visit.TurnID
		if turnChanged {
			if err = run.SetVisitTurn(visit.VisitID, observation.LatestTurnID); err != nil {
				return fmt.Errorf("координатор agent-graph: сохранить внешний turn посещения %q: %w", visit.VisitID, err)
			}
			continued[visit.VisitID] = true
		}
		if observation.LatestTurnID == "" && (state == scheduler.Running || state == scheduler.WaitingForApproval || state == scheduler.Succeeded) {
			return fmt.Errorf("координатор agent-graph: чат посещения %q сообщил %s без turnId", visit.VisitID, state)
		}
		diagnostic := agentTechnicalError(observation.LatestTurnError, nil)
		if state != scheduler.Unknown && state != scheduler.Failed && state != scheduler.Cancelled {
			diagnostic = ""
		}
		if state != oldState || turnChanged || diagnostic != visit.TechnicalError {
			if err = run.UpdateVisit(visit.VisitID, state, visit.CodexThreadID, diagnostic); err != nil {
				return fmt.Errorf("координатор agent-graph: сохранить сверку посещения %q: %w", visit.VisitID, err)
			}
			if err = run.AppendEvent(runstore.RuntimeEvent{
				VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: visit.CodexThreadID,
				TurnID: observation.LatestTurnID, Kind: "thread_reconciled", State: string(state), Message: diagnostic,
			}); err != nil {
				return fmt.Errorf("координатор agent-graph: сохранить событие сверки посещения %q: %w", visit.VisitID, err)
			}
		}
	}
	return nil
}

// startAgentWork устанавливает callbacks одинаково для нового чата и continue.
// Durable thread связывается до turn, durable turn — до Running и результата.
func startAgentWork(run *runstore.LockedRun, client Client, ctx context.Context, work agentWork, execution *activeExecution, results chan<- agentLaunchResult) {
	go func() {
		defer execution.finish()
		command := work.Command
		threadID, turnID := work.ThreadID, ""
		command.OnProcess = func(process codex.ProcessEvent) error {
			return appendAgentProcessEvent(run, work.VisitID, work.StepID, threadID, turnID, process)
		}
		if work.kind == agentWorkLaunch {
			command.OnThread = func(id string) error {
				threadID = id
				if err := run.UpdateVisit(work.VisitID, scheduler.Unknown, id, ""); err != nil {
					return err
				}
				execution.setThread(id)
				return run.AppendEvent(runstore.RuntimeEvent{VisitID: work.VisitID, StepID: work.StepID, ThreadID: id, Kind: "thread_started"})
			}
		}
		command.OnTurn = func(id string, interrupt func(context.Context) error) error {
			turnID = id
			if err := run.SetVisitTurn(work.VisitID, id); err != nil {
				return err
			}
			execution.setTurn(id, interrupt)
			return run.AppendEvent(runstore.RuntimeEvent{VisitID: work.VisitID, StepID: work.StepID, ThreadID: threadID, TurnID: id, Kind: "turn_bound"})
		}
		command.Notify = func(event codex.Event) error {
			if event.Method == "turn/started" {
				if err := run.UpdateVisit(work.VisitID, scheduler.Running, threadID, ""); err != nil {
					return err
				}
			}
			return appendAgentCodexEvent(run, work.VisitID, work.StepID, threadID, turnID, event)
		}
		var result codex.Result
		var err error
		if work.kind == agentWorkContinuation {
			result, err = client.Continue(ctx, work.ThreadID, command)
		} else {
			result, err = client.Run(ctx, command)
		}
		// ID из callback надёжнее частично заполненного результата с ошибкой и
		// сохраняет durable-связь даже у упрощённого тестового транспорта.
		if result.ThreadID == "" {
			result.ThreadID = threadID
		}
		if result.TurnID == "" {
			result.TurnID = turnID
		}
		results <- agentLaunchResult{visitID: work.VisitID, result: result, err: err}
	}()
}

// saveAgentLaunchResult сохраняет известные IDs до состояния. Единственная
// безопасная отмена Starting — локально подтверждённый CreationAttempted=false;
// после потери ответа thread/start повтор остаётся неоднозначным.
func saveAgentLaunchResult(run *runstore.LockedRun, completed agentLaunchResult) error {
	result, runErr := completed.result, completed.err
	if result.ThreadID == "" {
		if result.CreationAttempted {
			return fmt.Errorf("координатор agent-graph: посещение %q: %w; Codex мог создать чат, но не вернул его ID", completed.visitID, ErrAmbiguousStart)
		}
		if runErr == nil {
			runErr = errors.New("Codex завершил операцию без результата и без причины")
		}
		failure := fmt.Errorf("координатор agent-graph: посещение %q: Codex не начал создание чата: %w", completed.visitID, runErr)
		if releaseErr := run.ReleaseUnattemptedVisit(completed.visitID); releaseErr != nil {
			return errors.Join(failure, fmt.Errorf("вернуть посещение в Pending: %w; автоматический повтор запрещён", releaseErr))
		}
		return fmt.Errorf("%w; посещение возвращено в Pending — повторите явный resume", failure)
	}

	snapshot, err := run.Load()
	if err != nil {
		return fmt.Errorf("координатор agent-graph: прочитать посещение %q перед результатом: %w", completed.visitID, err)
	}
	visit, exists := agentVisitByID(snapshot, completed.visitID)
	if !exists {
		return fmt.Errorf("координатор agent-graph: нет посещения %q", completed.visitID)
	}
	if visit.CodexThreadID == "" {
		if err = run.UpdateVisit(completed.visitID, scheduler.Unknown, result.ThreadID, ""); err != nil {
			return fmt.Errorf("координатор agent-graph: сохранить чат посещения %q: %w", completed.visitID, err)
		}
		visit.CodexThreadID = result.ThreadID
	}
	if result.TurnID != "" && result.TurnID != visit.TurnID {
		if err = run.SetVisitTurn(completed.visitID, result.TurnID); err != nil {
			return fmt.Errorf("координатор agent-graph: сохранить turn посещения %q: %w", completed.visitID, err)
		}
		visit.TurnID = result.TurnID
	}

	state := scheduler.Unknown
	if runErr == nil {
		state, err = stateFromResult(result)
		if err != nil {
			runErr = err
			state = scheduler.Unknown
		}
	} else {
		var interaction *codex.InteractionRequired
		if errors.As(runErr, &interaction) {
			state = scheduler.WaitingForApproval
		}
	}
	if visit.TurnID == "" && (state == scheduler.Running || state == scheduler.WaitingForApproval || state == scheduler.Succeeded || state == scheduler.Failed || state == scheduler.Cancelled) {
		state = scheduler.Unknown
		if runErr == nil {
			runErr = errors.New("Codex вернул terminal status без turnId")
		}
	}
	diagnostic := agentTechnicalError(result.TurnError, runErr)
	if state != scheduler.Unknown && state != scheduler.Failed && state != scheduler.Cancelled {
		diagnostic = ""
	}
	if err = run.UpdateVisit(completed.visitID, state, result.ThreadID, diagnostic); err != nil {
		return fmt.Errorf("координатор agent-graph: сохранить результат посещения %q: %w", completed.visitID, err)
	}
	event := runstore.RuntimeEvent{
		VisitID: completed.visitID, StepID: visit.StepID, ThreadID: result.ThreadID, TurnID: visit.TurnID,
		Kind: "visit_state", State: string(state), Message: diagnostic,
	}
	if err = run.AppendEvent(event); err != nil {
		return fmt.Errorf("координатор agent-graph: сохранить событие результата посещения %q: %w", completed.visitID, err)
	}
	if runErr != nil && state != scheduler.WaitingForApproval {
		return fmt.Errorf("координатор agent-graph: посещение %q, чат %q: %w", completed.visitID, result.ThreadID, runErr)
	}
	return nil
}

// agentVisitByID ищет точное посещение в append-only порядке snapshot.
func agentVisitByID(snapshot runstore.Snapshot, visitID string) (runstore.Visit, bool) {
	for _, visit := range snapshot.Meta.Visits {
		if visit.VisitID == visitID {
			return visit, true
		}
	}
	return runstore.Visit{}, false
}

// agentTechnicalError превращает внешнюю диагностику в однострочное безопасное
// поле meta. Raw codexErrorInfo не сохраняется: оно расширяемо и может содержать
// произвольный payload. Рунная обрезка сохраняет корректный UTF-8.
func agentTechnicalError(turnError *codex.TurnError, cause error) string {
	parts := make([]string, 0, 3)
	if turnError != nil {
		parts = append(parts, turnError.Message)
		if turnError.AdditionalDetails != nil {
			parts = append(parts, *turnError.AdditionalDetails)
		}
	}
	if cause != nil {
		parts = append(parts, cause.Error())
	}
	var safe strings.Builder
	for _, character := range strings.Join(parts, ": ") {
		if unicode.IsControl(character) {
			fmt.Fprintf(&safe, `\u%04X`, character)
		} else {
			safe.WriteRune(character)
		}
	}
	value := strings.TrimSpace(safe.String())
	const limit, suffix = 4096, "…"
	if len(value) <= limit {
		return value
	}
	cut := limit - len(suffix)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + suffix
}

// finishAgentExecution единожды снимает локальное владение turn и его lease.
// Повторный поздний результат для уже удалённого ID не освобождает чужой slot.
func finishAgentExecution(active map[string]*activeExecution, visitID string) error {
	if execution := active[visitID]; execution != nil {
		execution.cancel()
		delete(active, visitID)
		return execution.lease.Release()
	}
	return nil
}

// drainAgentActive сохраняет все уже запущенные результаты при внутренней ошибке,
// не наблюдая чаты и не планируя новые переходы.
func drainAgentActive(cause error, run *runstore.LockedRun, active map[string]*activeExecution, results <-chan agentLaunchResult) error {
	for len(active) != 0 {
		completed := <-results
		cause = errors.Join(cause, finishAgentExecution(active, completed.visitID))
		cause = errors.Join(cause, saveAgentLaunchResult(run, completed))
	}
	return cause
}

// interruptAgentActive повторяет проверенный legacy shutdown-протокол, но все
// карты и ошибки адресует visitId. Lease освобождается только при удалении из
// active, поэтому результат и timeout не могут вернуть один slot дважды.
func interruptAgentActive(cause error, run *runstore.LockedRun, active map[string]*activeExecution, results <-chan agentLaunchResult) error {
	shutdown, cancel := context.WithTimeout(context.Background(), interruptGracePeriod)
	defer cancel()
	interrupts := make(chan interruptResult, len(active))
	for visitID, execution := range active {
		go func(visitID string, execution *activeExecution) {
			select {
			case <-execution.ready:
				threadID, turnID, interrupt, done := execution.snapshot()
				if done {
					interrupts <- interruptResult{stepID: visitID}
					return
				}
				if threadID == "" || turnID == "" || interrupt == nil {
					interrupts <- interruptResult{stepID: visitID, err: errors.New("активный turn не передал interrupt исходной сессии")}
					return
				}
				interrupts <- interruptResult{stepID: visitID, err: interrupt(shutdown)}
			case <-shutdown.Done():
				interrupts <- interruptResult{stepID: visitID, err: shutdown.Err()}
			}
		}(visitID, execution)
	}

	interruptErrors := make(map[string]error, len(active))
	for len(active) != 0 {
		select {
		case completed := <-results:
			cause = errors.Join(cause, finishAgentExecution(active, completed.visitID))
			delete(interruptErrors, completed.visitID)
			cause = errors.Join(cause, saveAgentLaunchResult(run, completed))
		case interrupted := <-interrupts:
			if interrupted.err != nil && active[interrupted.stepID] != nil {
				interruptErrors[interrupted.stepID] = interrupted.err
			}
		case <-shutdown.Done():
			cause = errors.Join(cause, fmt.Errorf("координатор agent-graph: остановить активные turn: %w", shutdown.Err()))
			for visitID, execution := range active {
				if interruptErr := interruptErrors[visitID]; interruptErr != nil {
					cause = errors.Join(cause, fmt.Errorf("координатор agent-graph: отменить turn посещения %q: %w", visitID, interruptErr))
				}
				execution.cancel()
			}
			return drainAgentActive(cause, run, active, results)
		}
	}
	return cause
}
