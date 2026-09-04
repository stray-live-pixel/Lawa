package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/capacity"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// agentExecutionClient моделирует границы Codex по visitId, извлечённому из
// служебного prompt. Это принципиально для тестов v4: stepId может повториться,
// а thread/turn и счётчики должны оставаться раздельными.
type agentExecutionClient struct {
	mu                                    sync.Mutex
	statuses, choices, secondChoices      map[string]string
	turnErrors                            map[string]*codex.TurnError
	resultErrors                          map[string]error
	beforeCreate                          map[string][]error
	releases, beforeTurn                  map[string]chan struct{}
	released, interrupted                 map[string]bool
	observations                          map[string]codex.Observation
	runs, continues, interrupts, toolSets map[string]int
	started                               chan string
}

func newAgentExecutionClient() *agentExecutionClient {
	return &agentExecutionClient{
		statuses: map[string]string{}, choices: map[string]string{}, secondChoices: map[string]string{}, turnErrors: map[string]*codex.TurnError{}, resultErrors: map[string]error{},
		beforeCreate: map[string][]error{}, releases: map[string]chan struct{}{}, beforeTurn: map[string]chan struct{}{}, released: map[string]bool{},
		interrupted: map[string]bool{}, observations: map[string]codex.Observation{}, runs: map[string]int{},
		continues: map[string]int{}, interrupts: map[string]int{}, toolSets: map[string]int{}, started: make(chan string, 32),
	}
}

func (c *agentExecutionClient) Run(ctx context.Context, command codex.Command) (codex.Result, error) {
	return c.execute(ctx, "", command, true)
}

func (c *agentExecutionClient) Continue(ctx context.Context, threadID string, command codex.Command) (codex.Result, error) {
	return c.execute(ctx, threadID, command, false)
}

func (c *agentExecutionClient) execute(ctx context.Context, threadID string, command codex.Command, create bool) (codex.Result, error) {
	visitID, stepID := agentPromptField(command.Text, "visitId"), agentPromptField(command.Text, "stepId")
	c.mu.Lock()
	if create {
		c.runs[stepID]++
	} else {
		c.continues[stepID]++
	}
	attempt := c.runs[stepID] + c.continues[stepID]
	before := c.beforeCreate[stepID]
	if create && len(before) != 0 {
		c.beforeCreate[stepID] = before[1:]
		c.mu.Unlock()
		return codex.Result{}, before[0]
	}
	if threadID == "" {
		threadID = "chat-" + visitID
	}
	turnID := fmt.Sprintf("turn-%s-%d", visitID, attempt)
	status := c.statuses[stepID]
	if status == "" {
		status = "completed"
	}
	choice, secondChoice, turnError, resultError := c.choices[stepID], c.secondChoices[stepID], c.turnErrors[stepID], c.resultErrors[stepID]
	release, beforeTurn := c.releases[stepID], c.beforeTurn[stepID]
	c.toolSets[stepID] += len(command.DynamicTools)
	c.mu.Unlock()

	result := codex.Result{ThreadID: threadID, TurnID: turnID, CreationAttempted: create, TurnAttempted: true}
	if create && command.OnThread != nil {
		if err := command.OnThread(threadID); err != nil {
			return result, err
		}
	}
	if beforeTurn != nil {
		select {
		case <-beforeTurn:
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	if command.OnTurn != nil {
		if err := command.OnTurn(turnID, func(interruptCtx context.Context) error {
			if err := interruptCtx.Err(); err != nil {
				return err
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			c.interrupts[stepID]++
			c.interrupted[stepID] = true
			if release != nil && !c.released[stepID] {
				close(release)
				c.released[stepID] = true
			}
			return nil
		}); err != nil {
			return result, err
		}
	}
	if command.Notify != nil {
		if err := command.Notify(codex.Event{Method: "turn/started"}); err != nil {
			return result, err
		}
	}
	if choice != "" {
		if command.CallDynamicTool == nil {
			return result, errors.New("choose_decision не настроен")
		}
		arguments, _ := json.Marshal(map[string]string{"decision": choice, "explanation": "выбрано тестом"})
		if _, err := command.CallDynamicTool(ctx, codex.DynamicToolCall{
			ThreadID: threadID, TurnID: turnID, CallID: "call-" + visitID, Tool: chooseDecisionToolName, Arguments: arguments,
		}); err != nil {
			return result, err
		}
		if secondChoice != "" {
			arguments, _ = json.Marshal(map[string]string{"decision": secondChoice, "explanation": "повторный выбор"})
			_, _ = command.CallDynamicTool(ctx, codex.DynamicToolCall{
				ThreadID: threadID, TurnID: turnID, CallID: "call-second-" + visitID, Tool: chooseDecisionToolName, Arguments: arguments,
			})
		}
	}
	c.started <- stepID
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	c.mu.Lock()
	if c.interrupted[stepID] {
		status = "interrupted"
	}
	c.observations[threadID] = agentObservation(threadID, turnID, status, turnError)
	c.mu.Unlock()
	result.Status, result.TurnError = status, turnError
	return result, resultError
}

func agentPromptField(prompt, field string) string {
	prefix := "ID "
	if field == "visitId" {
		prefix += "посещения (visitId): "
	} else {
		prefix += "логического кубика (stepId): "
	}
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func agentObservation(threadID, turnID, status string, turnError *codex.TurnError) codex.Observation {
	result := codex.Observation{ThreadID: threadID, ThreadStatus: "idle", LatestTurnID: turnID, LatestTurnError: turnError}
	switch status {
	case "completed":
		result.LatestTurnStatus = "completed"
	case "failed":
		result.LatestTurnStatus = "failed"
	case "interrupted":
		result.LatestTurnStatus = "interrupted"
	case "running":
		result.ThreadStatus, result.LatestTurnStatus = "active", "inProgress"
	}
	return result
}

type agentExecutionObserver struct{ client *agentExecutionClient }

func (c *agentExecutionClient) OpenObserver(context.Context, string) (Observer, error) {
	return &agentExecutionObserver{client: c}, nil
}

func (o *agentExecutionObserver) Inspect(threadID string) (codex.Observation, error) {
	o.client.mu.Lock()
	defer o.client.mu.Unlock()
	if observation, exists := o.client.observations[threadID]; exists {
		return observation, nil
	}
	return codex.Observation{ThreadID: threadID, ThreadStatus: "idle"}, nil
}

func (*agentExecutionObserver) Close() error { return nil }

func agentExecutionOptions(root string, client Client) Options {
	return Options{Root: root, PollInterval: time.Millisecond, Client: client, ContinueInterrupted: true}
}

// TestExecuteAgentGraphRoutesFanoutAndAfter проходит полный DAG: decision
// материализует две ветки, after ждёт обе, а каждый запрос и событие сохраняют
// собственный visitId. Финальный статус показывает применённое решение.
func TestExecuteAgentGraphRoutesFanoutAndAfter(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"route","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{"go":{"to":["left","right"]}}},
    {"id":"left","type":"agent","prompt":"Лево","after":[]},
    {"id":"right","type":"agent","prompt":"Право","after":[]},
    {"id":"join","type":"agent","prompt":"Собери","after":["left","right"]}
  ]}`)
	client := newAgentExecutionClient()
	client.choices["choice"] = "go"
	var last Status
	outcome, err := ExecuteWithOutcome(t.Context(), run, Options{
		Root: root, PollInterval: time.Millisecond, Client: client,
		Notify: func(status Status) error { last = status; return nil },
	})
	if err != nil || !outcome.Terminal || !outcome.Successful {
		t.Fatalf("agent-graph не достиг успеха: outcome=%+v err=%v", outcome, err)
	}
	snapshot, err := run.Load()
	if err != nil || snapshot.Meta.RunState != runstore.RunSucceeded || len(snapshot.Meta.Visits) != 4 {
		t.Fatalf("маршрут сохранён неверно: %+v, %v", snapshot.Meta, err)
	}
	if snapshot.Meta.Visits[0].Decision == nil || !snapshot.Meta.Visits[0].Decision.Applied || snapshot.Meta.Visits[0].VisitID != initial.Meta.Visits[0].VisitID {
		t.Fatalf("decision commit не применён: %+v", snapshot.Meta.Visits[0])
	}
	client.mu.Lock()
	for _, stepID := range []string{"choice", "left", "right", "join"} {
		if client.runs[stepID] != 1 {
			t.Errorf("кубик %q запущен %d раз", stepID, client.runs[stepID])
		}
	}
	client.mu.Unlock()
	if !last.Terminal || last.RunState != runstore.RunSucceeded || len(last.Steps) != 4 || last.Steps[0].VisitID == "" {
		t.Fatalf("visit-aware статус неполон: %+v", last)
	}
	events, err := runstore.ReadEvents(root, snapshot.Meta.RunID)
	if err != nil || len(events) == 0 {
		t.Fatalf("нет журнала v4: %v", err)
	}
	for _, event := range events {
		if event.VisitID == "" || event.StepID == "" {
			t.Fatalf("событие потеряло visit scope: %+v", event)
		}
	}
}

// TestExecuteAgentGraphIfElseSkipsBranchAndRunsJoin проходит пользовательский
// сценарий целиком через coordinator. Невыбранная ветка получает настоящий
// terminal Skipped без Codex-запуска, но остаётся причинным токеном для after;
// поэтому join выполняется один раз и run достигает естественного успеха.
func TestExecuteAgentGraphIfElseSkipsBranchAndRunsJoin(t *testing.T) {
	root, _, run := createAgentPreparationRun(t, `{
  "version":2,"id":"if-else","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "left":{"to":["left"]},"right":{"to":["right"]}}},
    {"id":"left","type":"agent","prompt":"Лево","after":[]},
    {"id":"right","type":"agent","prompt":"Право","after":[]},
    {"id":"join","type":"agent","prompt":"Собери","after":["left","right"]}
  ]}`)
	client := newAgentExecutionClient()
	client.choices["choice"] = "left"
	outcome, err := ExecuteWithOutcome(t.Context(), run, agentExecutionOptions(root, client))
	if err != nil || !outcome.Terminal || !outcome.Successful {
		t.Fatalf("if/else с общим join не завершился успешно: outcome=%+v err=%v", outcome, err)
	}

	snapshot, err := run.Load()
	if err != nil || snapshot.Meta.RunState != runstore.RunSucceeded || len(snapshot.Meta.Visits) != 4 {
		t.Fatalf("if/else сохранил неполную историю: %+v, %v", snapshot.Meta, err)
	}
	states := make(map[string]scheduler.State, len(snapshot.Meta.Visits))
	for _, visit := range snapshot.Meta.Visits {
		states[visit.StepID] = visit.State
	}
	if states["choice"] != scheduler.Succeeded || states["left"] != scheduler.Succeeded ||
		states["right"] != scheduler.Skipped || states["join"] != scheduler.Succeeded {
		t.Fatalf("ветки и join получили неверные состояния: %v", states)
	}
	client.mu.Lock()
	runs := map[string]int{
		"choice": client.runs["choice"], "left": client.runs["left"],
		"right": client.runs["right"], "join": client.runs["join"],
	}
	client.mu.Unlock()
	if runs["choice"] != 1 || runs["left"] != 1 || runs["right"] != 0 || runs["join"] != 1 {
		t.Fatalf("coordinator запустил не тот набор агентов: %v", runs)
	}
}

// TestExecuteAgentGraphFailedDecisionUsesAfter фиксирует различие технического
// Failed и выбора route: target не запускается, но failed остаётся допустимым
// terminal-токеном для after-проверяющего и весь граф может завершиться успешно.
func TestExecuteAgentGraphFailedDecisionUsesAfter(t *testing.T) {
	root, _, run := createAgentPreparationRun(t, `{
  "version":2,"id":"recover","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{"go":{"to":["target"]}}},
    {"id":"target","type":"agent","prompt":"Не запускать","after":[]},
    {"id":"recover","type":"agent","prompt":"Разбери сбой","after":["choice"]}
  ]}`)
	client := newAgentExecutionClient()
	client.statuses["choice"] = "failed"
	details := "код 503\nповтор позже"
	client.turnErrors["choice"] = &codex.TurnError{Message: "backend\tunavailable", AdditionalDetails: &details}
	outcome, err := ExecuteWithOutcome(t.Context(), run, agentExecutionOptions(root, client))
	if err != nil || !outcome.Successful {
		t.Fatalf("after не обработал technical Failed: %+v, %v", outcome, err)
	}
	client.mu.Lock()
	targetRuns, recoverRuns := client.runs["target"], client.runs["recover"]
	client.mu.Unlock()
	if targetRuns != 0 || recoverRuns != 1 {
		t.Fatalf("Failed ошибочно выбрал route: target=%d recover=%d", targetRuns, recoverRuns)
	}
	snapshot, _ := run.Load()
	if diagnostic := snapshot.Meta.Visits[0].TechnicalError; strings.ContainsAny(diagnostic, "\n\t") || !strings.Contains(diagnostic, `\u0009`) {
		t.Fatalf("диагностика не очищена: %q", diagnostic)
	}
}

// TestExecuteAgentGraphMissingDecisionFails сохраняет понятный failed run вместо
// молчаливого продолжения, если decision-agent завершил turn без инструмента.
func TestExecuteAgentGraphMissingDecisionFails(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"missing","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{"go":{"to":["target"]}}},
    {"id":"target","type":"agent","prompt":"Цель","after":[]}
  ]}`)
	client := newAgentExecutionClient()
	outcome, err := ExecuteWithOutcome(t.Context(), run, agentExecutionOptions(root, client))
	if !errors.Is(err, ErrRunUnsuccessful) || !outcome.Terminal || outcome.Successful {
		t.Fatalf("отсутствующий выбор не завершил run ошибкой: %+v, %v", outcome, err)
	}
	snapshot, _ := run.Load()
	if snapshot.Meta.RunState != runstore.RunFailed || snapshot.Meta.StopVisitID != initial.Meta.Visits[0].VisitID || !strings.Contains(snapshot.Meta.StopReason, "без choose_decision") {
		t.Fatalf("причина failed не доказана посещением: %+v", snapshot.Meta)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.runs["target"] != 0 {
		t.Fatalf("после missing decision запущен target: %v", client.runs)
	}
}

// TestExecuteAgentGraphPoisonedDecisionFails синхронизирует второй tool call
// между Advance и Reserve: terminal commit обязан запретить новый launch,
// прервать первый turn и вернуть оба root-wide capacity slot.
func TestExecuteAgentGraphPoisonedDecisionFails(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"poison","start":["choice","helper","work"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{"left":{"to":["target"]},"stop":{"finish":"failed"}}},
    {"id":"helper","type":"agent","prompt":"Освободи slot","after":[]},
    {"id":"work","type":"agent","prompt":"Не запускать","after":[]},
    {"id":"target","type":"agent","prompt":"Цель","after":[]}
  ]}`)
	client := newAgentExecutionClient()
	client.choices["choice"], client.releases["choice"] = "left", make(chan struct{})
	pool, err := capacity.Configure(root, "2")
	if err != nil {
		t.Fatal(err)
	}
	configure := func(snapshot runstore.Snapshot, command *codex.Command) {
		if agentPromptField(command.Text, "stepId") != "work" || snapshot.Meta.Visits[0].Decision == nil {
			return
		}
		visit := snapshot.Meta.Visits[0]
		_, _ = run.CommitDecision(visit.VisitID, visit.CodexThreadID, visit.TurnID, "stop", "конфликт", "call-conflict")
	}
	outcome, err := ExecuteWithOutcome(t.Context(), run, Options{
		Root: root, PollInterval: time.Millisecond, Client: client, ContinueInterrupted: true,
		Capacity: pool, ConfigureCommand: configure,
	})
	if !errors.Is(err, ErrRunUnsuccessful) || !outcome.Terminal || outcome.Successful {
		t.Fatalf("poisoned decision не завершил run: %+v, %v", outcome, err)
	}
	snapshot, _ := run.Load()
	decision := snapshot.Meta.Visits[0].Decision
	if snapshot.Meta.StopVisitID != initial.Meta.Visits[0].VisitID || decision == nil || decision.Error == "" || decision.Applied {
		t.Fatalf("конфликт не сохранён как неприменённая причина: %+v", snapshot.Meta)
	}
	client.mu.Lock()
	workRuns, targetRuns, interrupts := client.runs["work"], client.runs["target"], client.interrupts["choice"]
	client.mu.Unlock()
	if workRuns != 0 || targetRuns != 0 || interrupts != 1 {
		t.Fatalf("poison пропустил работу или не остановил turn: work=%d target=%d interrupts=%d", workRuns, targetRuns, interrupts)
	}
	for slot := 0; slot < 2; slot++ {
		lease, available, acquireErr := pool.TryAcquire()
		if acquireErr != nil || !available {
			t.Fatalf("capacity slot %d не освобождён: available=%v, %v", slot, available, acquireErr)
		}
		defer lease.Release()
	}
}

// TestExecuteAgentGraphBindsParallelTurnBeforePoisonFinish задерживает OnTurn
// соседа, затем возвращает transport error: durable poison всё равно должен
// прервать первый turn и завершить run после сохранения обоих turnId.
func TestExecuteAgentGraphBindsParallelTurnBeforePoisonFinish(t *testing.T) {
	root, _, run := createAgentPreparationRun(t, `{
  "version":2,"id":"poison-wave","start":["choice","slow"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "go":{"to":["target"]},"stop":{"finish":"failed"}}},
    {"id":"slow","type":"agent","prompt":"Долго","after":[]},
    {"id":"target","type":"agent","prompt":"Цель","after":[]}
  ]}`)
	client := newAgentExecutionClient()
	client.choices["choice"], client.secondChoices["choice"] = "go", "stop"
	client.releases["choice"], client.beforeTurn["slow"] = make(chan struct{}), make(chan struct{})
	transportFailure := errors.New("stdio оборвался")
	client.resultErrors["slow"] = transportFailure
	type executionResult struct {
		outcome Outcome
		err     error
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	finished := make(chan executionResult, 1)
	go func() {
		outcome, err := ExecuteWithOutcome(ctx, run, agentExecutionOptions(root, client))
		finished <- executionResult{outcome: outcome, err: err}
	}()
	for {
		snapshot, err := run.Load()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Meta.Visits[0].Decision != nil && snapshot.Meta.Visits[0].Decision.Error != "" {
			break
		}
		select {
		case result := <-finished:
			t.Fatalf("execution завершился до durable poison: %+v, %v", result.outcome, result.err)
		case <-time.After(time.Millisecond):
		}
	}
	close(client.beforeTurn["slow"])
	result := <-finished
	if !errors.Is(result.err, ErrRunUnsuccessful) || !errors.Is(result.err, transportFailure) || !result.outcome.Terminal || result.outcome.Successful {
		t.Fatalf("poison wave не завершила run: %+v, %v", result.outcome, result.err)
	}
	snapshot, _ := run.Load()
	client.mu.Lock()
	interrupts := client.interrupts["choice"]
	client.mu.Unlock()
	if snapshot.Meta.Visits[0].TurnID == "" || snapshot.Meta.Visits[1].TurnID == "" || snapshot.Meta.Visits[0].State != scheduler.Cancelled || interrupts != 1 {
		t.Fatalf("параллельная волна потеряла turn или interrupt: %+v", snapshot.Meta.Visits)
	}
}

// TestExecuteAgentGraphPrioritizesPoisonOverAmbiguousStart фиксирует resume
// после crash между Reserve и OnThread: durable poison важнее Starting без ID,
// поэтому run завершается без повторного сетевого запроса.
func TestExecuteAgentGraphPrioritizesPoisonOverAmbiguousStart(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"poison-crash","start":["choice","slow"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "go":{"finish":"succeeded"},"stop":{"finish":"failed"}}},
    {"id":"slow","type":"agent","prompt":"Не повторять","after":[]}
  ]}`)
	choiceID := initial.Meta.Visits[0].VisitID
	if err := run.ReserveVisits([]string{choiceID, initial.Meta.Visits[1].VisitID}); err == nil {
		err = run.UpdateVisit(choiceID, scheduler.Unknown, "chat-choice", "")
		if err == nil {
			err = run.SetVisitTurn(choiceID, "turn-choice")
		}
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
	if _, err := run.CommitDecision(choiceID, "chat-choice", "turn-choice", "go", "первый", "call-first"); err != nil {
		t.Fatal(err)
	}
	_, _ = run.CommitDecision(choiceID, "chat-choice", "turn-choice", "stop", "конфликт", "call-second")
	client := newAgentExecutionClient()
	outcome, err := ExecuteWithOutcome(t.Context(), run, agentExecutionOptions(root, client))
	snapshot, _ := run.Load()
	if !errors.Is(err, ErrRunUnsuccessful) || !outcome.Terminal || snapshot.Meta.RunState != runstore.RunFailed || len(client.runs) != 0 {
		t.Fatalf("resume предпочёл ambiguous повтор durable poison: %+v, runs=%v, %v", snapshot.Meta, client.runs, err)
	}
}

// TestExecuteAgentGraphReconcilesExternalTurn проверяет crash-resume: внешний
// completed turn сначала увеличивает Attempt, затем завершает исходный visit,
// не вызывая ни Run, ни Continue повторно.
func TestExecuteAgentGraphReconcilesExternalTurn(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"resume","start":["work"],"steps":[{"id":"work","type":"agent","prompt":"Работа","after":[]}]}`)
	visitID := initial.Meta.Visits[0].VisitID
	if err := run.ReserveVisits([]string{visitID}); err == nil {
		err = run.UpdateVisit(visitID, scheduler.Unknown, "chat-existing", "")
	} else {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(visitID, "turn-old"); err != nil {
		t.Fatal(err)
	}
	client := newAgentExecutionClient()
	client.observations["chat-existing"] = agentObservation("chat-existing", "turn-external", "completed", nil)
	outcome, err := ExecuteWithOutcome(t.Context(), run, agentExecutionOptions(root, client))
	if err != nil || !outcome.Successful {
		t.Fatalf("внешний turn не восстановлен: %+v, %v", outcome, err)
	}
	snapshot, _ := run.Load()
	visit := snapshot.Meta.Visits[0]
	client.mu.Lock()
	runs, continues := client.runs["work"], client.continues["work"]
	client.mu.Unlock()
	if visit.Attempt != 2 || visit.TurnID != "turn-external" || visit.State != scheduler.Succeeded || runs != 0 || continues != 0 {
		t.Fatalf("reconcile создал дубль или нарушил порядок: visit=%+v runs=%d continues=%d", visit, runs, continues)
	}
}

// TestExecuteAgentGraphKeepsCommittedChoiceOnCancelledResume доказывает, что
// повторный turn не получает choose_decision и один процесс не продолжает снова
// посещение, которое второй раз завершилось interrupted.
func TestExecuteAgentGraphKeepsCommittedChoiceOnCancelledResume(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"choice-resume","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{"done":{"finish":"succeeded"}}}
  ]}`)
	visitID := initial.Meta.Visits[0].VisitID
	if err := run.ReserveVisits([]string{visitID}); err == nil {
		err = run.UpdateVisit(visitID, scheduler.Unknown, "chat-choice", "")
	} else {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(visitID, "turn-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := run.CommitDecision(visitID, "chat-choice", "turn-old", "done", "готово", "call-old"); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visitID, scheduler.Cancelled, "chat-choice", "остановлен"); err != nil {
		t.Fatal(err)
	}
	client := newAgentExecutionClient()
	client.statuses["choice"] = "interrupted"
	client.observations["chat-choice"] = agentObservation("chat-choice", "turn-old", "interrupted", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Millisecond)
	defer cancel()
	err := Execute(ctx, run, agentExecutionOptions(root, client))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ожидалась остановка polling после одного continue: %v", err)
	}
	client.mu.Lock()
	continues, tools := client.continues["choice"], client.toolSets["choice"]
	client.mu.Unlock()
	if continues != 1 || tools != 0 {
		t.Fatalf("сохранённый выбор переигран или continue повторён: continues=%d tools=%d", continues, tools)
	}
}

// TestExecuteAgentGraphFinishInterruptsParallelVisit проверяет атомарный finish:
// решение завершает run сразу, а уже активная соседняя ветка адресно прерывается
// и не удерживает capacity lease после возврата.
func TestExecuteAgentGraphFinishInterruptsParallelVisit(t *testing.T) {
	root, _, run := createAgentPreparationRun(t, `{
  "version":2,"id":"finish","start":["choice","long"],"steps":[
    {"id":"choice","type":"agent","prompt":"Реши","after":[],"decisions":{"done":{"finish":"succeeded"}}},
    {"id":"long","type":"agent","prompt":"Долго","after":[]}
  ]}`)
	client := newAgentExecutionClient()
	client.choices["choice"] = "done"
	client.releases["long"] = make(chan struct{})
	outcome, err := ExecuteWithOutcome(t.Context(), run, agentExecutionOptions(root, client))
	if err != nil || !outcome.Successful {
		t.Fatalf("finish не завершил параллельный run: %+v, %v", outcome, err)
	}
	client.mu.Lock()
	interrupts := client.interrupts["long"]
	client.mu.Unlock()
	snapshot, _ := run.Load()
	longState := scheduler.State("")
	for _, visit := range snapshot.Meta.Visits {
		if visit.StepID == "long" {
			longState = visit.State
		}
	}
	if interrupts != 1 || longState != scheduler.Cancelled || snapshot.Meta.RunState != runstore.RunSucceeded {
		t.Fatalf("параллельный visit не остановлен: interrupts=%d state=%s meta=%+v", interrupts, longState, snapshot.Meta)
	}
}

// TestExecuteAgentGraphReleasesOnlyUnattemptedStart отличает локальный отказ до
// thread/start от сохранённого Starting после рестарта.
func TestExecuteAgentGraphReleasesOnlyUnattemptedStart(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"start","start":["work"],"steps":[{"id":"work","type":"agent","prompt":"Работа","after":[]}]}`)
	client := newAgentExecutionClient()
	failure := errors.New("initialize unavailable")
	client.beforeCreate["work"] = []error{failure}
	if err := Execute(t.Context(), run, agentExecutionOptions(root, client)); !errors.Is(err, failure) {
		t.Fatalf("потеряна исходная ошибка: %v", err)
	}
	snapshot, _ := run.Load()
	if snapshot.Meta.Visits[0].State != scheduler.Pending {
		t.Fatalf("неотправленный launch не возвращён: %+v", snapshot.Meta.Visits[0])
	}
	if err := run.ReserveVisits([]string{initial.Meta.Visits[0].VisitID}); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), run, agentExecutionOptions(root, client)); !errors.Is(err, ErrAmbiguousStart) {
		t.Fatalf("Starting после рестарта был повторён: %v", err)
	}
}
