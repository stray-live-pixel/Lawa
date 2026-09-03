package scheduler

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// TestPlanAgentGraphDecisionFanout проверяет один атомарный transition решения:
// Applied ставится источнику один раз, а targets сохраняют пользовательский
// порядок route.to и получают отдельные причинные triggers.
func TestPlanAgentGraphDecisionFanout(t *testing.T) {
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{"go": {To: []string{"beta", "alpha"}}}),
		agentStep("alpha", nil, nil),
		agentStep("beta", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("visit-choice", "choice", Succeeded, 1, &AgentDecisionView{Key: "go"}),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil {
		t.Fatal(err)
	}
	want := AgentPlan{
		ApplyDecisionVisitIDs: []string{"visit-choice"},
		DecisionActivations: []AgentActivation{
			{StepID: "beta", Iteration: 2, Trigger: AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{"visit-choice"}, DecisionKey: "go"}},
			{StepID: "alpha", Iteration: 2, Trigger: AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{"visit-choice"}, DecisionKey: "go"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fanout потерял атомарность или порядок:\n got: %#v\nwant: %#v", got, want)
	}

	// После durable application те же targets уже существуют. Повторный вызов
	// только ждёт их Pending-visits и не материализует маршрут второй раз.
	visits[0].Decision.Applied = true
	visits = append(visits,
		agentDecisionVisit("visit-beta", "beta", Pending, 2, "visit-choice", "go", nil),
		agentDecisionVisit("visit-alpha", "alpha", Pending, 2, "visit-choice", "go", nil),
	)
	got, err = PlanAgentGraph(w, visits)
	if err != nil || !reflect.DeepEqual(got, AgentPlan{}) {
		t.Fatalf("applied route запланирован повторно: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphDecisionMaterializesSkippedAlternatives проверяет полный
// атомарный результат выбора: выбранный fanout остаётся Pending, уникальные
// targets остальных ключей становятся Skipped, а общий target не получает
// противоречивое второе посещение. Порядок альтернатив не зависит от Go map.
func TestPlanAgentGraphDecisionMaterializesSkippedAlternatives(t *testing.T) {
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"selected": {To: []string{"shared", "left"}},
			"z-other":  {To: []string{"shared", "right", "duplicate"}},
			"a-other":  {To: []string{"duplicate", "alpha"}},
			"stop":     {Finish: agentOutcome(workflow.OutcomeFailed)},
		}),
		agentStep("shared", nil, nil), agentStep("left", nil, nil), agentStep("right", nil, nil),
		agentStep("duplicate", nil, nil), agentStep("alpha", nil, nil),
	)
	choice := agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "selected"})
	got, err := PlanAgentGraph(w, []AgentVisitView{choice})
	if err != nil {
		t.Fatal(err)
	}
	want := []AgentActivation{
		{StepID: "shared", Iteration: 2, Trigger: AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "selected"}},
		{StepID: "left", Iteration: 2, Trigger: AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "selected"}},
		{StepID: "duplicate", Iteration: 2, InitialState: Skipped, Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "a-other"}},
		{StepID: "alpha", Iteration: 2, InitialState: Skipped, Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "a-other"}},
		{StepID: "right", Iteration: 2, InitialState: Skipped, Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "z-other"}},
	}
	if !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"choice-1"}) || !reflect.DeepEqual(got.DecisionActivations, want) {
		t.Fatalf("выбор не сохранил selected/skipped атомарно:\n got: %#v\nwant: %#v", got, want)
	}

	// Старый running v4 мог уже сохранить Applied и выбранные targets без
	// synthetic skips. Pure planner должен дополнить только пробел, а следующий
	// вызов после сохранения причин станет идемпотентным.
	choice.Decision.Applied = true
	history := []AgentVisitView{
		choice,
		agentDecisionVisit("shared-1", "shared", Succeeded, 2, "choice-1", "selected", nil),
		agentDecisionVisit("left-1", "left", Succeeded, 2, "choice-1", "selected", nil),
	}
	backfill, err := PlanAgentGraph(w, history)
	if err != nil || !reflect.DeepEqual(backfill.DecisionActivations, want[2:]) || len(backfill.ApplyDecisionVisitIDs) != 0 {
		t.Fatalf("старый Applied snapshot не получил точный backfill: %#v, %v", backfill, err)
	}
	history = append(history,
		agentDecisionSkippedVisit("duplicate-1", "duplicate", 2, "choice-1", "a-other"),
		agentDecisionSkippedVisit("alpha-1", "alpha", 2, "choice-1", "a-other"),
		agentDecisionSkippedVisit("right-1", "right", 2, "choice-1", "z-other"),
	)
	afterBackfill, err := PlanAgentGraph(w, history)
	if err != nil || afterBackfill.Terminal == nil || afterBackfill.Terminal.Outcome != workflow.OutcomeSucceeded || len(afterBackfill.DecisionActivations) != 0 {
		t.Fatalf("сохранённый backfill повторился или заблокировал completion: %#v, %v", afterBackfill, err)
	}
}

// TestPlanAgentGraphSkippedAlternativeDoesNotWaitForActiveTarget защищает
// liveness общего target. Реальный visit shared уже выполняется как start, но
// это не мешает атомарно применить независимое решение: выбранная ветка получает
// Pending, а невыбранная причинная ветка shared — terminal Skipped без executor.
func TestPlanAgentGraphSkippedAlternativeDoesNotWaitForActiveTarget(t *testing.T) {
	w := agentWorkflow([]string{"choice", "shared"},
		agentStep("choice", nil, map[string]workflow.Route{
			"selected": {To: []string{"left"}},
			"other":    {To: []string{"shared"}},
		}),
		agentStep("shared", nil, nil),
		agentStep("left", nil, nil),
		agentStep("tail", []string{"shared"}, nil),
	)
	choice := agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "selected"})
	shared := agentStartVisit("shared-1", "shared", Running, 1, nil)
	got, err := PlanAgentGraph(w, []AgentVisitView{choice, shared})
	if err != nil || !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"choice-1"}) ||
		len(got.DecisionActivations) != 2 || len(got.AfterActivations) != 0 {
		t.Fatalf("активный общий target заблокировал решение: %#v, %v", got, err)
	}
	if got.DecisionActivations[0].StepID != "left" || got.DecisionActivations[0].InitialState != "" ||
		got.DecisionActivations[1].StepID != "shared" || got.DecisionActivations[1].InitialState != Skipped {
		t.Fatalf("решение потеряло selected/skipped разделение: %#v", got.DecisionActivations)
	}

	// Повторное планирование доказывает, что журнал с Running и Skipped одного
	// stepId валиден. Синтетическая запись не занимает execution-slot и не должна
	// создавать повторный backfill той же причинной ветки.
	choice.Decision.Applied = true
	history := []AgentVisitView{
		choice,
		shared,
		agentDecisionVisit("left-1", "left", Pending, 2, "choice-1", "selected", nil),
		agentDecisionSkippedVisit("shared-skip", "shared", 2, "choice-1", "other"),
	}
	got, err = PlanAgentGraph(w, history)
	if err != nil || !reflect.DeepEqual(got, AgentPlan{}) {
		t.Fatalf("Running и причинный Skipped одного target не сосуществуют: %#v, %v", got, err)
	}

	// После завершения раннего реального instance FIFO сначала отдаёт именно
	// его. Лишь следующая волна потребляет поздний Skipped и распространяет его
	// отдельным terminal-instance; сохранённая причина остаётся валидной.
	history[1].State = Succeeded
	got, err = PlanAgentGraph(w, history)
	if err != nil || len(got.AfterActivations) != 1 || got.AfterActivations[0].StepID != "tail" ||
		got.AfterActivations[0].InitialState != "" || !slices.Equal(got.AfterActivations[0].Trigger.SourceVisitIDs, []string{"shared-1"}) {
		t.Fatalf("FIFO не выбрал ранний реальный visit: %#v, %v", got, err)
	}
	history = append(history, agentAfterVisit("tail-real", "tail", Succeeded, 1, []string{"shared-1"}, nil))
	got, err = PlanAgentGraph(w, history)
	if err != nil || len(got.AfterActivations) != 1 || got.AfterActivations[0].InitialState != Skipped ||
		!slices.Equal(got.AfterActivations[0].Trigger.SourceVisitIDs, []string{"shared-skip"}) {
		t.Fatalf("FIFO потерял поздний skipped-instance: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphAllSkippedAfterDoesNotWaitForActiveTarget проверяет тот же
// инвариант для after. Join уже реально выполняется по decision-route, однако
// отдельный полностью пропущенный barrier должен сразу получить Skipped и не
// ждать внешнего turn, который может надолго остановиться на approval.
func TestPlanAgentGraphAllSkippedAfterDoesNotWaitForActiveTarget(t *testing.T) {
	w := agentWorkflow([]string{"driver", "choice"},
		agentStep("driver", nil, map[string]workflow.Route{"go": {To: []string{"join"}}}),
		agentStep("choice", nil, map[string]workflow.Route{
			"main":  {To: []string{"main"}},
			"left":  {To: []string{"left"}},
			"right": {To: []string{"right"}},
		}),
		agentStep("main", nil, nil),
		agentStep("left", nil, nil),
		agentStep("right", nil, nil),
		agentStep("join", []string{"left", "right"}, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("driver-1", "driver", Succeeded, 1, &AgentDecisionView{Key: "go", Applied: true}),
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "main", Applied: true}),
		agentDecisionVisit("join-real", "join", Running, 2, "driver-1", "go", nil),
		agentDecisionVisit("main-1", "main", Pending, 2, "choice-1", "main", nil),
		agentDecisionSkippedVisit("left-skip", "left", 2, "choice-1", "left"),
		agentDecisionSkippedVisit("right-skip", "right", 2, "choice-1", "right"),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil || len(got.AfterActivations) != 1 {
		t.Fatalf("активный join заблокировал пропущенный after-instance: %#v, %v", got, err)
	}
	skippedJoin := got.AfterActivations[0]
	if skippedJoin.StepID != "join" || skippedJoin.InitialState != Skipped ||
		!slices.Equal(skippedJoin.Trigger.SourceVisitIDs, []string{"left-skip", "right-skip"}) {
		t.Fatalf("полностью пропущенный barrier материализован неверно: %#v", skippedJoin)
	}

	visits = append(visits, agentAfterVisit(
		"join-skip", "join", Skipped, 2, []string{"left-skip", "right-skip"}, nil,
	))
	if next, nextErr := PlanAgentGraph(w, visits); nextErr != nil || !reflect.DeepEqual(next, AgentPlan{}) {
		t.Fatalf("Running и after-Skipped одного target не сосуществуют: %#v, %v", next, nextErr)
	}
}

// TestPlanAgentGraphRejectsIncompleteAppliedDecision не позволяет повреждённой
// проекции выдать natural success после частично записанного fanout или уже
// применённого terminal route. Applied обязан доказываться всей историей visits,
// а finish должен был атомарно завершить run ещё до следующего вызова planner.
func TestPlanAgentGraphRejectsIncompleteAppliedDecision(t *testing.T) {
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"go":   {To: []string{"alpha", "beta"}},
			"fail": {Finish: agentOutcome(workflow.OutcomeFailed)},
		}),
		agentStep("alpha", nil, nil),
		agentStep("beta", nil, nil),
	)
	tests := []struct {
		name     string
		decision AgentDecisionView
		visits   []AgentVisitView
		contains string
	}{
		{
			name: "partial fanout", decision: AgentDecisionView{Key: "go", Applied: true},
			visits: []AgentVisitView{
				agentDecisionVisit("alpha-1", "alpha", Succeeded, 2, "choice-1", "go", nil),
			},
			contains: "beta",
		},
		{name: "applied finish", decision: AgentDecisionView{Key: "fail", Applied: true}, contains: "terminal run"},
		{name: "unknown", decision: AgentDecisionView{Key: "missing", Applied: true}, contains: "неизвестное решение"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			choice := agentStartVisit("choice-1", "choice", Succeeded, 1, &tc.decision)
			visits := append([]AgentVisitView{choice}, tc.visits...)
			got, err := PlanAgentGraph(w, visits)
			if err == nil || !strings.Contains(err.Error(), tc.contains) || !reflect.DeepEqual(got, AgentPlan{}) {
				t.Fatalf("неполный applied transition принят: %#v, %v", got, err)
			}
		})
	}
}

// TestPlanAgentGraphDefersWholeFanout не допускает частичное применение route:
// занятая beta откладывает и её повторное посещение, и свободную alpha. После
// завершения beta обе цели появляются вместе в исходном порядке.
func TestPlanAgentGraphDefersWholeFanout(t *testing.T) {
	w := agentWorkflow([]string{"choice", "beta"},
		agentStep("choice", nil, map[string]workflow.Route{"go": {To: []string{"beta", "alpha"}}}),
		agentStep("alpha", nil, nil),
		agentStep("beta", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "go"}),
		agentStartVisit("beta-1", "beta", Running, 1, nil),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil || !reflect.DeepEqual(got, AgentPlan{}) {
		t.Fatalf("частично занятый fanout не был отложен целиком: %#v, %v", got, err)
	}

	visits[1].State = Succeeded
	got, err = PlanAgentGraph(w, visits)
	if err != nil || !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"choice-1"}) || len(got.DecisionActivations) != 2 ||
		got.DecisionActivations[0].StepID != "beta" || got.DecisionActivations[1].StepID != "alpha" {
		t.Fatalf("освободившийся fanout не применён целиком: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphFailedAfterRecovery фиксирует различие технического Failed и
// результата всего workflow: Failed удовлетворяет after, а успешный recovery
// становится новым frontier и позволяет natural completion завершиться успешно.
func TestPlanAgentGraphFailedAfterRecovery(t *testing.T) {
	w := agentWorkflow([]string{"work"},
		agentStep("work", nil, nil),
		agentStep("recovery", []string{"work"}, nil),
	)
	visits := []AgentVisitView{agentStartVisit("work-1", "work", Failed, 1, nil)}
	got, err := PlanAgentGraph(w, visits)
	if err != nil {
		t.Fatal(err)
	}
	wantActivation := AgentActivation{
		StepID: "recovery", Iteration: 1,
		Trigger: AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: []string{"work-1"}},
	}
	if !reflect.DeepEqual(got.AfterActivations, []AgentActivation{wantActivation}) || got.Terminal != nil {
		t.Fatalf("Failed не разбудил recovery: %#v", got)
	}

	visits = append(visits, AgentVisitView{
		VisitID: "recovery-1", StepID: "recovery", Iteration: 1, State: Succeeded,
		Trigger: wantActivation.Trigger,
	})
	got, err = PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeSucceeded {
		t.Fatalf("обработанный Failed отравил natural completion: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphDecisionFailures проверяет две доказуемые fatal-причины.
// CauseVisitID не выводится из текста: runstore сможет связать terminal run с
// конкретным durable фактом даже при незавершённой параллельной ветке.
func TestPlanAgentGraphDecisionFailures(t *testing.T) {
	w := agentWorkflow([]string{"choice", "parallel"},
		agentStep("choice", nil, map[string]workflow.Route{"done": {Finish: agentOutcome(workflow.OutcomeSucceeded)}}),
		agentStep("parallel", nil, nil),
	)
	tests := []struct {
		name     string
		state    State
		decision *AgentDecisionView
		contains string
	}{
		{name: "missing", state: Succeeded, contains: "без choose_decision"},
		{name: "poisoned running", state: Running, decision: &AgentDecisionView{Key: "done", Error: "два разных вызова"}, contains: "конфликт"},
		{name: "poisoned cancelled", state: Cancelled, decision: &AgentDecisionView{Key: "done", Error: "два разных вызова"}, contains: "конфликт"},
		{name: "unknown", state: Succeeded, decision: &AgentDecisionView{Key: "other"}, contains: "неизвестный ключ"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			visits := []AgentVisitView{
				agentStartVisit("choice-1", "choice", tc.state, 1, tc.decision),
				agentStartVisit("parallel-1", "parallel", Running, 1, nil),
			}
			got, err := PlanAgentGraph(w, visits)
			if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeFailed ||
				got.Terminal.CauseVisitID != "choice-1" || !strings.Contains(got.Terminal.Reason, tc.contains) {
				t.Fatalf("fatal decision потерял исход или proof: %#v, %v", got, err)
			}
			if len(got.ApplyDecisionVisitIDs) != 0 || len(got.DecisionActivations) != 0 || len(got.AfterActivations) != 0 {
				t.Fatalf("fatal decision смешан с другими действиями: %#v", got)
			}
		})
	}
}

// TestPlanAgentGraphDecisionWaveBarrier фиксирует детерминизм параллельной
// волны. Ни ранний, ни поздний finish не применяется, пока второй decision visit
// ещё можно продолжить. После завершения обоих всегда побеждает порядок Visits;
// только durable конфликт обходит барьер как уже доказанная fatal-ошибка.
func TestPlanAgentGraphDecisionWaveBarrier(t *testing.T) {
	w := agentWorkflow([]string{"first", "second"},
		agentStep("first", nil, map[string]workflow.Route{"done": {Finish: agentOutcome(workflow.OutcomeSucceeded)}}),
		agentStep("second", nil, map[string]workflow.Route{"stop": {Finish: agentOutcome(workflow.OutcomeFailed)}}),
	)
	firstDone := agentStartVisit("first-1", "first", Succeeded, 1, &AgentDecisionView{Key: "done"})
	secondDone := agentStartVisit("second-1", "second", Succeeded, 1, &AgentDecisionView{Key: "stop"})
	partials := [][]AgentVisitView{
		{agentStartVisit("first-1", "first", Running, 1, nil), secondDone},
		{firstDone, agentStartVisit("second-1", "second", Running, 1, nil)},
	}
	for _, visits := range partials {
		if got, err := PlanAgentGraph(w, visits); err != nil || !reflect.DeepEqual(got, AgentPlan{}) {
			t.Fatalf("частично завершённая decision-wave изменила run: %#v, %v", got, err)
		}
	}
	got, err := PlanAgentGraph(w, []AgentVisitView{firstDone, secondDone})
	if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeSucceeded || got.Terminal.CauseVisitID != "first-1" {
		t.Fatalf("полная волна выбрала finish не по Visits: %#v, %v", got, err)
	}
	for _, fatal := range []struct {
		name     string
		state    State
		decision *AgentDecisionView
	}{
		{name: "poison", state: Running, decision: &AgentDecisionView{Key: "stop", Error: "конфликт"}},
		{name: "missing", state: Succeeded},
		{name: "unknown", state: Succeeded, decision: &AgentDecisionView{Key: "other"}},
	} {
		t.Run(fatal.name, func(t *testing.T) {
			visits := []AgentVisitView{
				agentStartVisit("first-1", "first", Running, 1, nil),
				agentStartVisit("second-1", "second", fatal.state, 1, fatal.decision),
			}
			fatalPlan, fatalErr := PlanAgentGraph(w, visits)
			if fatalErr != nil || fatalPlan.Terminal == nil || fatalPlan.Terminal.Outcome != workflow.OutcomeFailed || fatalPlan.Terminal.CauseVisitID != "second-1" {
				t.Fatalf("доказанная fatal-ошибка стала ждать барьер: %#v, %v", fatalPlan, fatalErr)
			}
		})
	}
}

// TestPlanAgentGraphRouteWaitsForDecisionWave не даёт готовому route создать
// target, пока параллельный decision visit Running. После его Failed барьер
// закрыт, и маршрут материализуется обычным атомарным переходом.
func TestPlanAgentGraphRouteWaitsForDecisionWave(t *testing.T) {
	w := agentWorkflow([]string{"route", "gate", "source"},
		agentStep("route", nil, map[string]workflow.Route{"go": {To: []string{"work"}}}),
		agentStep("gate", nil, map[string]workflow.Route{"done": {Finish: agentOutcome(workflow.OutcomeSucceeded)}}),
		agentStep("work", nil, nil),
		agentStep("source", nil, nil),
		agentStep("after", []string{"source"}, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("route-1", "route", Succeeded, 1, &AgentDecisionView{Key: "go"}),
		agentStartVisit("gate-1", "gate", Running, 1, nil),
		agentStartVisit("source-1", "source", Succeeded, 1, nil),
	}
	if got, err := PlanAgentGraph(w, visits); err != nil || !reflect.DeepEqual(got, AgentPlan{}) {
		t.Fatalf("route или after материализован до полного decision-wave: %#v, %v", got, err)
	}
	visits[1].State = Failed
	got, err := PlanAgentGraph(w, visits)
	if err != nil || !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"route-1"}) ||
		len(got.DecisionActivations) != 1 || got.DecisionActivations[0].StepID != "work" ||
		len(got.AfterActivations) != 1 || got.AfterActivations[0].StepID != "after" {
		t.Fatalf("route и after не применены после закрытия барьера: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphFinishIsImmediate не позволяет параллельному Running visit
// задержать явный finish или добавить в тот же атомарный commit обычные routes.
func TestPlanAgentGraphFinishIsImmediate(t *testing.T) {
	w := agentWorkflow([]string{"route", "finish", "busy"},
		agentStep("route", nil, map[string]workflow.Route{"go": {To: []string{"work"}}}),
		agentStep("finish", nil, map[string]workflow.Route{"stop": {Finish: agentOutcome(workflow.OutcomeFailed)}}),
		agentStep("busy", nil, nil),
		agentStep("work", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("route-1", "route", Succeeded, 1, &AgentDecisionView{Key: "go"}),
		agentStartVisit("finish-1", "finish", Succeeded, 1, &AgentDecisionView{Key: "stop"}),
		agentStartVisit("busy-1", "busy", Running, 1, nil),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeFailed ||
		got.Terminal.CauseVisitID != "finish-1" || !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"finish-1"}) || len(got.DecisionActivations) != 0 {
		t.Fatalf("finish не получил немедленный изолированный transition: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphTerminalMarksPendingSkipped проверяет границу общего finish.
// Ещё не зарезервированный start-visit можно сделать Skipped атомарно, а Running
// остаётся фактической активной работой для последующего адресного interrupt.
// Невыбранный отсутствующий target решения тоже получает synthetic Skipped.
func TestPlanAgentGraphTerminalMarksPendingSkipped(t *testing.T) {
	w := agentWorkflow([]string{"finish", "queued", "busy"},
		agentStep("finish", nil, map[string]workflow.Route{
			"done": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
			"work": {To: []string{"unused"}},
		}),
		agentStep("queued", nil, nil),
		agentStep("busy", nil, nil),
		agentStep("unused", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("finish-1", "finish", Succeeded, 1, &AgentDecisionView{Key: "done"}),
		agentStartVisit("queued-1", "queued", Pending, 1, nil),
		agentStartVisit("busy-1", "busy", Running, 1, nil),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeSucceeded ||
		!reflect.DeepEqual(got.MarkSkippedVisitIDs, []string{"queued-1"}) || len(got.DecisionActivations) != 1 {
		t.Fatalf("finish не отделил pending от уже начатой работы: %#v, %v", got, err)
	}
	skipped := got.DecisionActivations[0]
	if skipped.StepID != "unused" || skipped.InitialState != Skipped || skipped.Trigger.Kind != AgentTriggerDecisionSkipped ||
		skipped.Trigger.DecisionKey != "work" {
		t.Fatalf("finish потерял невыбранную ветку: %#v", skipped)
	}
}

// TestPlanAgentGraphDefersSharedTarget показывает no-overlap между двумя готовыми
// решениями. Первый visit в durable-порядке занимает общий target; второй choice
// остаётся unapplied целиком и продолжает только после завершения target.
func TestPlanAgentGraphDefersSharedTarget(t *testing.T) {
	route := map[string]workflow.Route{"go": {To: []string{"shared"}}}
	w := agentWorkflow([]string{"first", "second"},
		agentStep("first", nil, route),
		agentStep("second", nil, route),
		agentStep("shared", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("first-1", "first", Succeeded, 1, &AgentDecisionView{Key: "go"}),
		agentStartVisit("second-1", "second", Succeeded, 1, &AgentDecisionView{Key: "go"}),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil || !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"first-1"}) || len(got.DecisionActivations) != 1 {
		t.Fatalf("общий target активирован с overlap: %#v, %v", got, err)
	}

	visits[0].Decision.Applied = true
	visits = append(visits, agentDecisionVisit("shared-1", "shared", Succeeded, 2, "first-1", "go", nil))
	got, err = PlanAgentGraph(w, visits)
	if err != nil || !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"second-1"}) ||
		len(got.DecisionActivations) != 1 || got.DecisionActivations[0].Trigger.SourceVisitIDs[0] != "second-1" {
		t.Fatalf("отложенное решение не продолжилось после освобождения target: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphFIFOJoin создаёт две полные пары источников. Первый join уже
// потребил ранние visits, поэтому новый barrier обязан взять вторые источники в
// порядке Step.After и унаследовать их максимальную iteration.
func TestPlanAgentGraphFIFOJoin(t *testing.T) {
	fanout := map[string]workflow.Route{"go": {To: []string{"left", "right"}}}
	w := agentWorkflow([]string{"wave-one", "wave-two"},
		agentStep("wave-one", nil, fanout),
		agentStep("wave-two", nil, fanout),
		agentStep("left", nil, nil),
		agentStep("right", nil, nil),
		agentStep("join", []string{"right", "left"}, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("wave-1", "wave-one", Succeeded, 1, &AgentDecisionView{Key: "go", Applied: true}),
		agentStartVisit("wave-2", "wave-two", Succeeded, 1, &AgentDecisionView{Key: "go", Applied: true}),
		agentDecisionVisit("left-1", "left", Succeeded, 2, "wave-1", "go", nil),
		agentDecisionVisit("right-1", "right", Succeeded, 2, "wave-1", "go", nil),
		agentDecisionVisit("left-2", "left", Succeeded, 2, "wave-2", "go", nil),
		agentDecisionVisit("right-2", "right", Succeeded, 2, "wave-2", "go", nil),
		{
			VisitID: "join-1", StepID: "join", Iteration: 2, State: Succeeded,
			Trigger: AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: []string{"right-1", "left-1"}},
		},
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil {
		t.Fatal(err)
	}
	want := AgentActivation{
		StepID: "join", Iteration: 2,
		Trigger: AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: []string{"right-2", "left-2"}},
	}
	if !reflect.DeepEqual(got.AfterActivations, []AgentActivation{want}) {
		t.Fatalf("join нарушил FIFO/order/iteration: got %#v, want %#v", got.AfterActivations, want)
	}
}

// TestPlanAgentGraphCancelledWaits подтверждает, что Cancelled не является
// after-источником и не превращает временное отсутствие действий в completion.
func TestPlanAgentGraphCancelledWaits(t *testing.T) {
	w := agentWorkflow([]string{"work"},
		agentStep("work", nil, nil),
		agentStep("after", []string{"work"}, nil),
	)
	got, err := PlanAgentGraph(w, []AgentVisitView{agentStartVisit("work-1", "work", Cancelled, 1, nil)})
	if err != nil || !reflect.DeepEqual(got, AgentPlan{}) {
		t.Fatalf("Cancelled ошибочно удовлетворил after или завершил run: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphAfterSkippedSemantics отделяет отсутствующую ветку от
// технического результата. Только целиком skipped-барьер распространяет Skipped;
// хотя бы один реально выполненный Failed/Succeeded источник требует запустить
// downstream-агента, чтобы он обработал смешанный набор причин.
func TestPlanAgentGraphAfterSkippedSemantics(t *testing.T) {
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"left": {To: []string{"left"}}, "none": {To: []string{"marker"}}, "right": {To: []string{"right"}},
		}),
		agentStep("left", nil, nil),
		agentStep("right", nil, nil),
		agentStep("marker", nil, nil),
		agentStep("join", []string{"left", "right"}, nil),
		agentStep("tail", []string{"join"}, nil),
	)
	for _, tc := range []struct {
		name      string
		selected  string
		leftState State
		wantState State
	}{
		{name: "all skipped", selected: "none", wantState: Skipped},
		{name: "succeeded and skipped", selected: "left", leftState: Succeeded},
		{name: "failed and skipped", selected: "left", leftState: Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			visits := []AgentVisitView{agentStartVisit(
				"choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: tc.selected, Applied: true},
			)}
			if tc.selected == "none" {
				visits = append(visits,
					agentDecisionVisit("marker-1", "marker", Succeeded, 2, "choice-1", "none", nil),
					agentDecisionSkippedVisit("left-1", "left", 2, "choice-1", "left"),
					agentDecisionSkippedVisit("right-1", "right", 2, "choice-1", "right"),
				)
			} else {
				visits = append(visits,
					agentDecisionVisit("left-1", "left", tc.leftState, 2, "choice-1", "left", nil),
					agentDecisionSkippedVisit("marker-1", "marker", 2, "choice-1", "none"),
					agentDecisionSkippedVisit("right-1", "right", 2, "choice-1", "right"),
				)
			}
			got, err := PlanAgentGraph(w, visits)
			if err != nil || got.Terminal != nil || len(got.AfterActivations) != 1 {
				t.Fatalf("after не создал единственный join: %#v, %v", got, err)
			}
			join := got.AfterActivations[0]
			if join.StepID != "join" || join.Iteration != 2 || join.InitialState != tc.wantState ||
				!slices.Equal(join.Trigger.SourceVisitIDs, []string{"left-1", "right-1"}) {
				t.Fatalf("after неверно классифицировал источники: %#v", join)
			}

			if tc.wantState == Skipped {
				visits = append(visits, agentAfterVisit("join-1", "join", Skipped, 2, []string{"left-1", "right-1"}, nil))
				next, nextErr := PlanAgentGraph(w, visits)
				if nextErr != nil || len(next.AfterActivations) != 1 || next.AfterActivations[0].StepID != "tail" || next.AfterActivations[0].InitialState != Skipped {
					t.Fatalf("skipped не распространился по after-DAG: %#v, %v", next, nextErr)
				}
			}
		})
	}
}

// TestPlanAgentGraphPropagatesNestedSkippedDecision воспроизводит вложенную
// if/else-ветку из ревью. Пропущенный decision не выбирает бизнес-ключ, однако
// технически распространяет отсутствие на union всех route.to. Иначе leafA и
// leafB не появились бы в истории, join завершился бы естественно без запуска.
func TestPlanAgentGraphPropagatesNestedSkippedDecision(t *testing.T) {
	w := agentWorkflow([]string{"outer"},
		agentStep("outer", nil, map[string]workflow.Route{
			"nested": {To: []string{"inner"}}, "right": {To: []string{"right"}},
		}),
		agentStep("inner", nil, map[string]workflow.Route{
			"a": {To: []string{"leafA"}}, "b": {To: []string{"leafB"}},
		}),
		agentStep("right", nil, nil),
		agentStep("leafA", nil, nil),
		agentStep("leafB", nil, nil),
		agentStep("join", []string{"right", "leafA", "leafB"}, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("outer-1", "outer", Succeeded, 1, &AgentDecisionView{Key: "right", Applied: true}),
		agentDecisionVisit("right-1", "right", Succeeded, 2, "outer-1", "right", nil),
		agentDecisionSkippedVisit("inner-1", "inner", 2, "outer-1", "nested"),
	}
	got, err := PlanAgentGraph(w, visits)
	wantLeaves := []AgentActivation{
		{StepID: "leafA", Iteration: 3, InitialState: Skipped, Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"inner-1"}, DecisionKey: "a"}},
		{StepID: "leafB", Iteration: 3, InitialState: Skipped, Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"inner-1"}, DecisionKey: "b"}},
	}
	if err != nil || got.Terminal != nil || !reflect.DeepEqual(got.DecisionActivations, wantLeaves) {
		t.Fatalf("вложенная skipped-ветка не дошла до листьев: %#v, %v", got, err)
	}

	visits = append(visits,
		agentDecisionSkippedVisit("leafA-1", "leafA", 3, "inner-1", "a"),
		agentDecisionSkippedVisit("leafB-1", "leafB", 3, "inner-1", "b"),
	)
	got, err = PlanAgentGraph(w, visits)
	if err != nil || got.Terminal != nil || len(got.AfterActivations) != 1 ||
		got.AfterActivations[0].StepID != "join" || got.AfterActivations[0].InitialState != "" ||
		!slices.Equal(got.AfterActivations[0].Trigger.SourceVisitIDs, []string{"right-1", "leafA-1", "leafB-1"}) {
		t.Fatalf("mixed join не дождался полного nested closure: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphDeduplicatesNestedSkippedWave проверяет два уровня
// дедупликации. Общий target нескольких пропущенных decision-кубиков одной
// волны создаётся один раз; такой же target другой реальной decision-волны
// остаётся отдельной причинной веткой.
func TestPlanAgentGraphDeduplicatesNestedSkippedWave(t *testing.T) {
	t.Run("same wave", func(t *testing.T) {
		w := agentWorkflow([]string{"outer"},
			agentStep("outer", nil, map[string]workflow.Route{
				"main": {To: []string{"main"}}, "nested": {To: []string{"left", "right"}},
			}),
			agentStep("main", nil, nil),
			agentStep("left", nil, map[string]workflow.Route{"go": {To: []string{"shared"}}}),
			agentStep("right", nil, map[string]workflow.Route{"go": {To: []string{"shared"}}}),
			agentStep("shared", nil, nil),
		)
		visits := []AgentVisitView{
			agentStartVisit("outer-1", "outer", Succeeded, 1, &AgentDecisionView{Key: "main", Applied: true}),
			agentDecisionVisit("main-1", "main", Succeeded, 2, "outer-1", "main", nil),
			agentDecisionSkippedVisit("left-1", "left", 2, "outer-1", "nested"),
			agentDecisionSkippedVisit("right-1", "right", 2, "outer-1", "nested"),
		}
		got, err := PlanAgentGraph(w, visits)
		if err != nil || len(got.DecisionActivations) != 1 || got.DecisionActivations[0].StepID != "shared" ||
			!slices.Equal(got.DecisionActivations[0].Trigger.SourceVisitIDs, []string{"left-1"}) {
			t.Fatalf("shared target одной волны не дедуплицирован: %#v, %v", got, err)
		}
		forged := append(slices.Clone(visits),
			agentDecisionSkippedVisit("shared-left", "shared", 3, "left-1", "go"),
			agentDecisionSkippedVisit("shared-right", "shared", 3, "right-1", "go"),
		)
		if _, err = PlanAgentGraph(w, forged); err == nil || !strings.Contains(err.Error(), "той же skipped-волне") {
			t.Fatalf("pure boundary приняла повторный target одной волны: %v", err)
		}
	})

	t.Run("different waves", func(t *testing.T) {
		w := agentWorkflow([]string{"first", "second"},
			agentStep("first", nil, map[string]workflow.Route{
				"main": {To: []string{"main1"}}, "nested": {To: []string{"left"}},
			}),
			agentStep("second", nil, map[string]workflow.Route{
				"main": {To: []string{"main2"}}, "nested": {To: []string{"right"}},
			}),
			agentStep("main1", nil, nil), agentStep("main2", nil, nil),
			agentStep("left", nil, map[string]workflow.Route{"go": {To: []string{"shared"}}}),
			agentStep("right", nil, map[string]workflow.Route{"go": {To: []string{"shared"}}}),
			agentStep("shared", nil, nil),
		)
		visits := []AgentVisitView{
			agentStartVisit("first-1", "first", Succeeded, 1, &AgentDecisionView{Key: "main", Applied: true}),
			agentStartVisit("second-1", "second", Succeeded, 1, &AgentDecisionView{Key: "main", Applied: true}),
			agentDecisionVisit("main1-1", "main1", Succeeded, 2, "first-1", "main", nil),
			agentDecisionSkippedVisit("left-1", "left", 2, "first-1", "nested"),
			agentDecisionVisit("main2-1", "main2", Succeeded, 2, "second-1", "main", nil),
			agentDecisionSkippedVisit("right-1", "right", 2, "second-1", "nested"),
		}
		got, err := PlanAgentGraph(w, visits)
		if err != nil || len(got.DecisionActivations) != 2 || got.DecisionActivations[0].StepID != "shared" ||
			got.DecisionActivations[1].StepID != "shared" {
			t.Fatalf("разные skipped-волны ошибочно схлопнулись: %#v, %v", got, err)
		}
	})
}

// TestPlanAgentGraphSkippedDecisionSelfLoopIsFinite фиксирует конечность
// синтетической ветки. Пропущенный decision распространяет union route.to, но
// уже достигнутый в той же волне self-loop не создаёт второе посещение.
func TestPlanAgentGraphSkippedDecisionSelfLoopIsFinite(t *testing.T) {
	limit := 1
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"main": {To: []string{"main"}}, "loop": {To: []string{"loop"}},
		}),
		agentStep("main", nil, nil),
		workflow.Step{ID: "loop", Type: "agent", Prompt: "Не запускай цикл", After: []string{}, MaxVisits: &limit,
			Decisions: map[string]workflow.Route{
				"again": {To: []string{"loop"}}, "done": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
			}},
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "main", Applied: true}),
		agentDecisionVisit("main-1", "main", Succeeded, 2, "choice-1", "main", nil),
		agentDecisionSkippedVisit("loop-1", "loop", 2, "choice-1", "loop"),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeSucceeded || len(got.DecisionActivations) != 0 {
		t.Fatalf("skipped self-loop размножился либо заблокировал completion: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphSkippedDecisionSCCIsFinite расширяет проверку self-loop на
// компоненту из двух decision-шагов. MaxVisits здесь нужен статической схеме для
// реального исполнения, но конечность Skipped обеспечивает только wave-dedupe:
// пропуск B не создаёт второй A той же причинной волны.
func TestPlanAgentGraphSkippedDecisionSCCIsFinite(t *testing.T) {
	limit := 1
	a := agentStep("a", nil, map[string]workflow.Route{
		"next": {To: []string{"b"}}, "stop": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
	})
	b := agentStep("b", nil, map[string]workflow.Route{
		"back": {To: []string{"a"}}, "stop": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
	})
	a.MaxVisits, b.MaxVisits = &limit, &limit
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"cycle": {To: []string{"a"}}, "main": {To: []string{"main"}},
		}),
		agentStep("main", nil, nil), a, b,
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "main", Applied: true}),
		agentDecisionVisit("main-1", "main", Succeeded, 2, "choice-1", "main", nil),
		agentDecisionSkippedVisit("a-1", "a", 2, "choice-1", "cycle"),
	}
	first, err := PlanAgentGraph(w, visits)
	if err != nil || len(first.DecisionActivations) != 1 || first.DecisionActivations[0].StepID != "b" {
		t.Fatalf("первый шаг skipped-SCC не создан: %#v, %v", first, err)
	}
	visits = append(visits, agentDecisionSkippedVisit("b-1", "b", 3, "a-1", "next"))
	last, err := PlanAgentGraph(w, visits)
	if err != nil || last.Terminal == nil || last.Terminal.Outcome != workflow.OutcomeSucceeded || len(last.DecisionActivations) != 0 {
		t.Fatalf("skipped-SCC повторно создал a либо не завершился: %#v, %v", last, err)
	}
}

// TestPlanAgentSkippedClosureBuildsTerminalLayers проверяет pure API для одного
// Advance. Runstore сначала материализует direct skip с настоящим VisitID, затем
// вызывает planner послойно: nested routes, all-skipped after и его nested route.
// Параллельный cleanup Skipped не получает причинную волну и не разворачивается.
func TestPlanAgentSkippedClosureBuildsTerminalLayers(t *testing.T) {
	w := agentWorkflow([]string{"finish", "cleanup"},
		agentStep("finish", nil, map[string]workflow.Route{
			"branch": {To: []string{"nested"}}, "done": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
		}),
		agentStep("cleanup", nil, map[string]workflow.Route{"must-not-run": {To: []string{"cleanup-child"}}}),
		agentStep("nested", nil, map[string]workflow.Route{
			"a": {To: []string{"leafA"}}, "b": {To: []string{"leafB"}},
		}),
		agentStep("leafA", nil, nil), agentStep("leafB", nil, nil),
		agentStep("join", []string{"leafA", "leafB"}, map[string]workflow.Route{"next": {To: []string{"tail"}}}),
		agentStep("tail", nil, nil), agentStep("cleanup-child", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("finish-1", "finish", Succeeded, 1, &AgentDecisionView{Key: "done", Applied: true}),
		agentStartVisit("cleanup-1", "cleanup", Skipped, 1, nil),
		agentDecisionSkippedVisit("nested-1", "nested", 2, "finish-1", "branch"),
	}

	first, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{})
	wantFirst := []AgentActivation{
		{StepID: "leafA", Iteration: 3, InitialState: Skipped, Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"nested-1"}, DecisionKey: "a"}},
		{StepID: "leafB", Iteration: 3, InitialState: Skipped, Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"nested-1"}, DecisionKey: "b"}},
	}
	if err != nil || !reflect.DeepEqual(first.DecisionActivations, wantFirst) || len(first.AfterActivations) != 0 ||
		len(first.ApplyDecisionVisitIDs) != 0 || len(first.MarkSkippedVisitIDs) != 0 || first.Terminal != nil {
		t.Fatalf("первый terminal closure-слой неверен: %#v, %v", first, err)
	}
	visits = append(visits,
		agentDecisionSkippedVisit("leafA-1", "leafA", 3, "nested-1", "a"),
		agentDecisionSkippedVisit("leafB-1", "leafB", 3, "nested-1", "b"),
	)
	second, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{})
	if err != nil || len(second.DecisionActivations) != 0 || len(second.AfterActivations) != 1 ||
		second.AfterActivations[0].StepID != "join" || second.AfterActivations[0].InitialState != Skipped {
		t.Fatalf("all-skipped after не стал вторым closure-слоем: %#v, %v", second, err)
	}
	visits = append(visits, agentAfterVisit("join-1", "join", Skipped, 3, []string{"leafA-1", "leafB-1"}, nil))
	third, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{})
	if err != nil || len(third.DecisionActivations) != 1 || third.DecisionActivations[0].StepID != "tail" ||
		!slices.Equal(third.DecisionActivations[0].Trigger.SourceVisitIDs, []string{"join-1"}) {
		t.Fatalf("волна after не распространилась на nested decision: %#v, %v", third, err)
	}
	visits = append(visits, agentDecisionSkippedVisit("tail-1", "tail", 4, "join-1", "next"))
	last, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{})
	if err != nil || !reflect.DeepEqual(last, AgentPlan{}) {
		t.Fatalf("terminal closure не достиг неподвижной точки: %#v, %v", last, err)
	}
}

// TestPlanAgentSkippedClosureConsumesCleanupBeforeCausal проверяет общий FIFO с
// тремя типами sources. Реальный instance после global finish больше не создаёт
// downstream-работу и обходится. Следующий rootless cleanup обязан первым
// оставить Skipped after-квитанцию; только последующий слой может потребить
// causal Skipped и продолжить его волну. Selector и validator тем самым
// используют один и тот же устойчивый порядок.
func TestPlanAgentSkippedClosureConsumesCleanupBeforeCausal(t *testing.T) {
	w := agentWorkflow([]string{"finish", "shared", "driver"},
		agentStep("finish", nil, map[string]workflow.Route{
			"branch": {To: []string{"shared"}}, "done": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
		}),
		agentStep("shared", nil, nil),
		agentStep("driver", nil, map[string]workflow.Route{"go": {To: []string{"shared"}}}),
		agentStep("tail", []string{"shared"}, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("finish-1", "finish", Succeeded, 1, &AgentDecisionView{Key: "done", Applied: true}),
		agentStartVisit("shared-real", "shared", Succeeded, 1, nil),
		agentStartVisit("driver-1", "driver", Succeeded, 1, &AgentDecisionView{Key: "go", Applied: true}),
		agentDecisionVisit("shared-cleanup", "shared", Skipped, 2, "driver-1", "go", nil),
		agentDecisionSkippedVisit("shared-causal", "shared", 2, "finish-1", "branch"),
	}

	first, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{})
	if err != nil || len(first.AfterActivations) != 1 ||
		!slices.Equal(first.AfterActivations[0].Trigger.SourceVisitIDs, []string{"shared-cleanup"}) {
		t.Fatalf("cleanup не получил первую FIFO-квитанцию: %#v, %v", first, err)
	}
	visits = append(visits, agentAfterVisit(
		"tail-cleanup", "tail", Skipped, 2, []string{"shared-cleanup"}, nil,
	))
	second, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{})
	if err != nil || len(second.AfterActivations) != 1 ||
		!slices.Equal(second.AfterActivations[0].Trigger.SourceVisitIDs, []string{"shared-causal"}) {
		t.Fatalf("causal source не открылся после cleanup: %#v, %v", second, err)
	}
	visits = append(visits, agentAfterVisit(
		"tail-causal", "tail", Skipped, 2, []string{"shared-causal"}, nil,
	))
	if last, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{}); err != nil || !reflect.DeepEqual(last, AgentPlan{}) {
		t.Fatalf("terminal FIFO не достиг неподвижной точки: %#v, %v", last, err)
	}
}

// TestPlanAgentSkippedClosurePreservesCausalRootsAcrossCleanupJoin не даёт
// rootless terminal cleanup стереть происхождение соседней невыбранной ветки.
// Сам join уже Skipped по всем sources, но его decision-routes обязаны получить
// существующую causal wave; вымышленный root для cleanup при этом не создаётся.
func TestPlanAgentSkippedClosurePreservesCausalRootsAcrossCleanupJoin(t *testing.T) {
	w := agentWorkflow([]string{"finish", "cleanup"},
		agentStep("finish", nil, map[string]workflow.Route{
			"branch": {To: []string{"branch"}}, "done": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
		}),
		agentStep("cleanup", nil, nil), agentStep("branch", nil, nil),
		agentStep("join", []string{"cleanup", "branch"}, map[string]workflow.Route{
			"x": {To: []string{"leaf-x"}}, "y": {To: []string{"leaf-y"}},
		}),
		agentStep("leaf-x", nil, nil), agentStep("leaf-y", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("finish-1", "finish", Succeeded, 1, &AgentDecisionView{Key: "done", Applied: true}),
		agentStartVisit("cleanup-1", "cleanup", Skipped, 1, nil),
		agentDecisionSkippedVisit("branch-1", "branch", 2, "finish-1", "branch"),
	}
	first, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{})
	if err != nil || len(first.AfterActivations) != 1 || first.AfterActivations[0].StepID != "join" {
		t.Fatalf("mixed cleanup+causal join не создан: %#v, %v", first, err)
	}
	visits = append(visits, agentAfterVisit(
		"join-1", "join", Skipped, 2, []string{"cleanup-1", "branch-1"}, nil,
	))
	second, err := PlanAgentSkippedClosure(w, visits, true, "", AgentTriggerView{})
	if err != nil || len(second.DecisionActivations) != 2 ||
		second.DecisionActivations[0].StepID != "leaf-x" || second.DecisionActivations[1].StepID != "leaf-y" {
		t.Fatalf("causal roots не прошли через cleanup join: %#v, %v", second, err)
	}
}

// TestPlanAgentSkippedClosureReservesAfterLimitSources защищает FIFO-набор,
// на котором maxVisits уже выбрал terminal outcome. Один и тот же b-source
// нельзя затем повторно присоединить к более позднему skipped a-source и
// материализовать фиктивную квитанцию N+1: эта причина уже принадлежит
// несозданной runnable-активации, сохранённой в AgentTerminal.
func TestPlanAgentSkippedClosureReservesAfterLimitSources(t *testing.T) {
	limit := 1
	limited := agentStep("limited", []string{"a", "b"}, nil)
	limited.MaxVisits = &limit
	w := agentWorkflow([]string{"choice", "a", "limited"},
		agentStep("choice", nil, map[string]workflow.Route{
			"main": {To: []string{"worker"}}, "skipped": {To: []string{"a", "b"}},
		}),
		agentStep("a", nil, nil), limited, agentStep("worker", nil, nil), agentStep("b", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "main", Applied: true}),
		agentStartVisit("a-real", "a", Succeeded, 1, nil),
		agentStartVisit("limited-1", "limited", Succeeded, 1, nil),
		agentDecisionVisit("worker-1", "worker", Succeeded, 2, "choice-1", "main", nil),
		agentDecisionSkippedVisit("a-skip", "a", 2, "choice-1", "skipped"),
		agentDecisionSkippedVisit("b-skip", "b", 2, "choice-1", "skipped"),
	}

	plan, err := PlanAgentGraph(w, visits)
	if err != nil || plan.Terminal == nil || plan.Terminal.LimitStepID != "limited" ||
		!slices.Equal(plan.Terminal.LimitTrigger.SourceVisitIDs, []string{"a-real", "b-skip"}) {
		t.Fatalf("fixture не создала ожидаемый after-limit: %#v, %v", plan, err)
	}
	closure, err := PlanAgentSkippedClosure(
		w, visits, true, plan.Terminal.LimitStepID, plan.Terminal.LimitTrigger,
	)
	if err != nil || !reflect.DeepEqual(closure, AgentPlan{}) {
		t.Fatalf("terminal closure повторно использовал source after-limit: %#v, %v", closure, err)
	}
}

// TestPlanAgentSkippedClosureContinuesAfterLimitFIFO защищает Skipped-источник,
// который уже находился за точным real trigger ограниченного after-шага. Сам
// trigger N+1 виртуально потреблён terminal outcome, а следующая причинная
// ветка не расходует квоту и должна получить отдельную durable-квитанцию.
func TestPlanAgentSkippedClosureContinuesAfterLimitFIFO(t *testing.T) {
	limit := 1
	limited := agentStep("limited", []string{"source"}, nil)
	limited.MaxVisits = &limit
	w := agentWorkflow([]string{"first", "second", "source"},
		agentStep("first", nil, map[string]workflow.Route{"go": {To: []string{"source"}}}),
		agentStep("second", nil, map[string]workflow.Route{
			"main": {To: []string{"worker"}}, "unused": {To: []string{"source"}},
		}),
		agentStep("source", nil, nil), limited, agentStep("worker", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("first-1", "first", Succeeded, 1, &AgentDecisionView{Key: "go", Applied: true}),
		agentStartVisit("second-1", "second", Succeeded, 1, &AgentDecisionView{Key: "main", Applied: true}),
		agentStartVisit("source-1", "source", Succeeded, 1, nil),
		agentDecisionVisit("source-2", "source", Succeeded, 2, "first-1", "go", nil),
		agentDecisionVisit("worker-1", "worker", Succeeded, 2, "second-1", "main", nil),
		agentDecisionSkippedVisit("source-skip", "source", 2, "second-1", "unused"),
		agentAfterVisit("limited-1", "limited", Succeeded, 1, []string{"source-1"}, nil),
	}
	plan, err := PlanAgentGraph(w, visits)
	if err != nil || plan.Terminal == nil || plan.Terminal.LimitStepID != "limited" ||
		!slices.Equal(plan.Terminal.LimitTrigger.SourceVisitIDs, []string{"source-2"}) {
		t.Fatalf("fixture не создала after-limit на real FIFO-source: %#v, %v", plan, err)
	}
	closure, err := PlanAgentSkippedClosure(
		w, visits, true, plan.Terminal.LimitStepID, plan.Terminal.LimitTrigger,
	)
	if err != nil || len(closure.AfterActivations) != 1 ||
		closure.AfterActivations[0].StepID != "limited" || closure.AfterActivations[0].InitialState != Skipped ||
		!slices.Equal(closure.AfterActivations[0].Trigger.SourceVisitIDs, []string{"source-skip"}) {
		t.Fatalf("terminal closure потерял causal Skipped после limit-trigger: %#v, %v", closure, err)
	}
}

// TestPlanAgentGraphRejectsTerminalCleanupSkipped защищает pure boundary:
// start и выбранный decision-target могут стать Skipped только одновременно с
// terminal run, поэтому running planner не должен принимать такую историю как
// естественно завершённую. Synthetic decision_skipped проверяется отдельно.
func TestPlanAgentGraphRejectsTerminalCleanupSkipped(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		w := agentWorkflow([]string{"start"}, agentStep("start", nil, nil))
		_, err := PlanAgentGraph(w, []AgentVisitView{agentStartVisit("start-1", "start", Skipped, 1, nil)})
		if err == nil || !strings.Contains(err.Error(), "terminal-причины") {
			t.Fatalf("running planner принял произвольно пропущенный start: %v", err)
		}
	})

	t.Run("selected decision target", func(t *testing.T) {
		w := agentWorkflow([]string{"choice"},
			agentStep("choice", nil, map[string]workflow.Route{"go": {To: []string{"target"}}}),
			agentStep("target", nil, nil),
		)
		visits := []AgentVisitView{
			agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "go", Applied: true}),
			agentDecisionVisit("target-1", "target", Skipped, 2, "choice-1", "go", nil),
		}
		_, err := PlanAgentGraph(w, visits)
		if err == nil || !strings.Contains(err.Error(), "terminal run") {
			t.Fatalf("running planner принял skipped выбранного target: %v", err)
		}
	})
}

// TestPlanAgentGraphNaturalQuiescence проверяет фактический causal frontier, а не
// все исторические ошибки. Невыбранная половина join сначала получает durable
// Skipped и удовлетворяет after вместе с выбранной веткой; только после самого
// join возможен естественный успех. Необработанный Failed-лист завершает run
// ошибкой.
func TestPlanAgentGraphNaturalQuiescence(t *testing.T) {
	t.Run("if else reaches join", func(t *testing.T) {
		w := agentWorkflow([]string{"choice"},
			agentStep("choice", nil, map[string]workflow.Route{
				"left": {To: []string{"left"}}, "right": {To: []string{"right"}},
			}),
			agentStep("left", nil, nil),
			agentStep("right", nil, nil),
			agentStep("join", []string{"left", "right"}, nil),
		)
		visits := []AgentVisitView{
			agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "left", Applied: true}),
			agentDecisionVisit("left-1", "left", Succeeded, 2, "choice-1", "left", nil),
		}
		got, err := PlanAgentGraph(w, visits)
		if err != nil || got.Terminal != nil || len(got.DecisionActivations) != 1 ||
			got.DecisionActivations[0].StepID != "right" || got.DecisionActivations[0].InitialState != Skipped {
			t.Fatalf("невыбранная ветка не получила skipped: %#v, %v", got, err)
		}
		visits = append(visits, agentDecisionSkippedVisit("right-1", "right", 2, "choice-1", "right"))
		got, err = PlanAgentGraph(w, visits)
		if err != nil || got.Terminal != nil || len(got.AfterActivations) != 1 || got.AfterActivations[0].StepID != "join" ||
			got.AfterActivations[0].InitialState != "" || !slices.Equal(got.AfterActivations[0].Trigger.SourceVisitIDs, []string{"left-1", "right-1"}) {
			t.Fatalf("selected+skipped не запустили join: %#v, %v", got, err)
		}
		visits = append(visits, agentAfterVisit("join-1", "join", Succeeded, 2, []string{"left-1", "right-1"}, nil))
		got, err = PlanAgentGraph(w, visits)
		if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeSucceeded || got.Terminal.CauseVisitID != "" {
			t.Fatalf("завершённый join не дал natural success: %#v, %v", got, err)
		}
	})

	t.Run("failed frontier fails", func(t *testing.T) {
		w := agentWorkflow([]string{"leaf"}, agentStep("leaf", nil, nil))
		got, err := PlanAgentGraph(w, []AgentVisitView{agentStartVisit("leaf-1", "leaf", Failed, 1, nil)})
		if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeFailed || got.Terminal.CauseVisitID != "leaf-1" {
			t.Fatalf("Failed frontier не определил natural outcome: %#v, %v", got, err)
		}
	})
}

// TestPlanAgentGraphSkippedDoesNotSpendMaxVisits проверяет различие номера
// истории и квоты исполнения. Первый target только отмечен Skipped, поэтому
// следующая настоящая активация разрешена при maxVisits=1; лимит срабатывает
// лишь при попытке ещё одного запуска и ссылается на реальный visit.
func TestPlanAgentGraphSkippedDoesNotSpendMaxVisits(t *testing.T) {
	limit := 1
	target := agentStep("target", nil, map[string]workflow.Route{
		"again": {To: []string{"target"}}, "done": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
	})
	target.MaxVisits = &limit
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"work": {To: []string{"driver"}}, "skip-target": {To: []string{"target"}},
		}),
		agentStep("driver", nil, map[string]workflow.Route{"go": {To: []string{"target"}}}),
		target,
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "work", Applied: true}),
		agentDecisionVisit("driver-1", "driver", Succeeded, 2, "choice-1", "work", &AgentDecisionView{Key: "go"}),
		agentDecisionSkippedVisit("target-skip", "target", 2, "choice-1", "skip-target"),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil || !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"driver-1"}) || len(got.DecisionActivations) != 1 ||
		got.DecisionActivations[0].StepID != "target" || got.DecisionActivations[0].InitialState != "" {
		t.Fatalf("skipped visit ошибочно израсходовал maxVisits: %#v, %v", got, err)
	}

	visits[1].Decision.Applied = true
	visits = append(visits, agentDecisionVisit(
		"target-real", "target", Succeeded, 3, "driver-1", "go", &AgentDecisionView{Key: "again"},
	))
	got, err = PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.LimitStepID != "target" || got.Terminal.CauseVisitID != "target-real" ||
		got.Terminal.LimitIteration != 4 || len(got.ApplyDecisionVisitIDs) != 0 ||
		!strings.Contains(got.Terminal.Reason, "исполнение №2") {
		t.Fatalf("лимит не сослался на единственный реальный visit: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphBoundedSelfLoop проверяет точную границу maxVisits. Второе
// посещение ещё создаётся, но попытка третьего не применяет source decision и
// сохраняет последний разрешённый visit как структурированную причину. Отдельно
// проверяем безопасный failed по умолчанию, явный onLimit и повреждённый N+1.
func TestPlanAgentGraphBoundedSelfLoop(t *testing.T) {
	limit := 2
	step := agentStep("loop", nil, map[string]workflow.Route{
		"again": {To: []string{"loop"}}, "done": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
	})
	step.MaxVisits = &limit
	w := agentWorkflow([]string{"loop"}, step)
	visits := []AgentVisitView{agentStartVisit("loop-1", "loop", Succeeded, 1, &AgentDecisionView{Key: "again"})}

	got, err := PlanAgentGraph(w, visits)
	if err != nil || !reflect.DeepEqual(got.ApplyDecisionVisitIDs, []string{"loop-1"}) || len(got.DecisionActivations) != 1 ||
		got.DecisionActivations[0].StepID != "loop" || got.DecisionActivations[0].Iteration != 2 {
		t.Fatalf("разрешённое второе посещение не создано: %#v, %v", got, err)
	}
	visits[0].Decision.Applied = true
	visits = append(visits, agentDecisionVisit("loop-2", "loop", Succeeded, 2, "loop-1", "again", &AgentDecisionView{Key: "again"}))
	got, err = PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeFailed ||
		got.Terminal.CauseVisitID != "loop-2" || got.Terminal.LimitStepID != "loop" ||
		got.Terminal.LimitTrigger.Kind != AgentTriggerDecision ||
		!slices.Equal(got.Terminal.LimitTrigger.SourceVisitIDs, []string{"loop-2"}) ||
		got.Terminal.LimitTrigger.DecisionKey != "again" || got.Terminal.LimitIteration != 3 ||
		len(got.ApplyDecisionVisitIDs) != 0 || len(got.DecisionActivations) != 0 || !strings.Contains(got.Terminal.Reason, "исполнение №3") {
		t.Fatalf("граница self-loop не дала атомарный failed: %#v, %v", got, err)
	}

	onLimit := workflow.OutcomeSucceeded
	w.Steps[0].OnLimit = &onLimit
	got, err = PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeSucceeded || !strings.Contains(got.Terminal.Reason, "onLimit") {
		t.Fatalf("явный onLimit не определил исход: %#v, %v", got, err)
	}

	visits[1].Decision.Applied = true
	visits = append(visits, agentDecisionVisit("loop-3", "loop", Pending, 3, "loop-2", "again", nil))
	if got, err = PlanAgentGraph(w, visits); err == nil || !strings.Contains(err.Error(), "превышает maxVisits=2") || !reflect.DeepEqual(got, AgentPlan{}) {
		t.Fatalf("сохранённое N+1 посещение прошло проверку: %#v, %v", got, err)
	}
}

// TestPlanAgentGraphLimitKeepsFanoutAtomic фиксирует порядок route.to и
// no-overlap. Пока beta активна, весь fanout ждёт. После её завершения первая
// исчерпанная цель выбирает outcome, а source остаётся unapplied и ни alpha, ни
// свободный выбранный target не материализуются частично. При этом target
// невыбранного ключа сохраняется Skipped: квота не должна стирать аудит выбора.
func TestPlanAgentGraphLimitKeepsFanoutAtomic(t *testing.T) {
	limit := 1
	succeeded := workflow.OutcomeSucceeded
	choice := agentStep("choice", nil, map[string]workflow.Route{
		"go": {To: []string{"beta", "alpha", "free"}}, "other": {To: []string{"unused"}},
	})
	beta, alpha := agentStep("beta", nil, nil), agentStep("alpha", nil, nil)
	beta.MaxVisits, beta.OnLimit, alpha.MaxVisits = &limit, &succeeded, &limit
	w := agentWorkflow(
		[]string{"choice", "beta", "alpha"}, choice, beta, alpha,
		agentStep("free", nil, nil), agentStep("unused", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "go"}),
		agentStartVisit("beta-1", "beta", Running, 1, nil),
		agentStartVisit("alpha-1", "alpha", Succeeded, 1, nil),
	}
	if got, err := PlanAgentGraph(w, visits); err != nil || !reflect.DeepEqual(got, AgentPlan{}) {
		t.Fatalf("занятый fanout не был отложен целиком: %#v, %v", got, err)
	}
	visits[1].State = Succeeded
	got, err := PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeSucceeded ||
		got.Terminal.LimitStepID != "beta" || got.Terminal.CauseVisitID != "beta-1" ||
		got.Terminal.LimitTrigger.Kind != AgentTriggerDecision ||
		!slices.Equal(got.Terminal.LimitTrigger.SourceVisitIDs, []string{"choice-1"}) ||
		got.Terminal.LimitTrigger.DecisionKey != "go" || got.Terminal.LimitIteration != 2 ||
		len(got.ApplyDecisionVisitIDs) != 0 || len(got.DecisionActivations) != 1 ||
		got.DecisionActivations[0].StepID != "unused" || got.DecisionActivations[0].InitialState != Skipped ||
		got.DecisionActivations[0].Trigger.Kind != AgentTriggerDecisionSkipped ||
		!slices.Equal(got.DecisionActivations[0].Trigger.SourceVisitIDs, []string{"choice-1"}) {
		t.Fatalf("limit fanout нарушил порядок или атомарность: %#v, %v", got, err)
	}
	terminalHistory := append(slices.Clone(visits), agentDecisionSkippedVisit(
		"unused-1", "unused", 2, "choice-1", "other",
	))
	limitTrigger := AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "go"}
	if closure, closureErr := PlanAgentSkippedClosure(w, terminalHistory, true, "beta", limitTrigger); closureErr != nil || !reflect.DeepEqual(closure, AgentPlan{}) {
		t.Fatalf("terminal closure не принял skipped от точного decision-limit: %#v, %v", closure, closureErr)
	}
	if _, closureErr := PlanAgentSkippedClosure(w, terminalHistory, true, "", AgentTriggerView{}); closureErr == nil {
		t.Fatal("terminal closure принял skipped от произвольного unapplied decision")
	}
}

// TestPlanAgentGraphMultiStepCycleIterations воспроизводит цикл
// test -> checker -> fix -> test. FIFO after сохраняет номер волны, decision
// увеличивает его, а третья готовая проверка останавливается на квоте checker=2.
func TestPlanAgentGraphMultiStepCycleIterations(t *testing.T) {
	limit := 2
	testStep := agentStep("test", []string{"fix"}, nil)
	checker := agentStep("checker", []string{"test"}, map[string]workflow.Route{
		"repeat": {To: []string{"fix"}}, "done": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
	})
	checker.MaxVisits = &limit
	w := agentWorkflow([]string{"test"}, testStep, checker, agentStep("fix", nil, nil))
	visits := []AgentVisitView{
		agentStartVisit("test-1", "test", Succeeded, 1, nil),
		agentAfterVisit("checker-1", "checker", Succeeded, 1, []string{"test-1"}, &AgentDecisionView{Key: "repeat", Applied: true}),
		agentDecisionVisit("fix-1", "fix", Succeeded, 2, "checker-1", "repeat", nil),
		agentAfterVisit("test-2", "test", Succeeded, 2, []string{"fix-1"}, nil),
		agentAfterVisit("checker-2", "checker", Succeeded, 2, []string{"test-2"}, &AgentDecisionView{Key: "repeat", Applied: true}),
		agentDecisionVisit("fix-2", "fix", Succeeded, 3, "checker-2", "repeat", nil),
		agentAfterVisit("test-3", "test", Succeeded, 3, []string{"fix-2"}, nil),
	}
	got, err := PlanAgentGraph(w, visits)
	if err != nil || got.Terminal == nil || got.Terminal.LimitStepID != "checker" || got.Terminal.CauseVisitID != "checker-2" ||
		got.Terminal.LimitTrigger.Kind != AgentTriggerAfter ||
		!slices.Equal(got.Terminal.LimitTrigger.SourceVisitIDs, []string{"test-3"}) || got.Terminal.LimitIteration != 3 ||
		!strings.Contains(got.Terminal.Reason, "iteration=3") || len(got.AfterActivations) != 0 {
		t.Fatalf("многошаговый цикл потерял квоту или iteration: %#v, %v", got, err)
	}
}

func agentWorkflow(start []string, steps ...workflow.Step) workflow.Workflow {
	version := workflow.VersionAgentGraph
	return workflow.Workflow{Version: &version, ID: "agent-test", Start: start, Steps: steps}
}

func agentStep(id string, after []string, decisions map[string]workflow.Route) workflow.Step {
	if after == nil {
		after = []string{}
	}
	return workflow.Step{ID: id, Type: "agent", Prompt: "Выполни " + id, After: after, Decisions: decisions}
}

func agentStartVisit(visitID, stepID string, state State, iteration int, decision *AgentDecisionView) AgentVisitView {
	return AgentVisitView{
		VisitID: visitID, StepID: stepID, Iteration: iteration, State: state,
		Trigger: AgentTriggerView{Kind: AgentTriggerStart}, Decision: decision,
	}
}

func agentDecisionVisit(visitID, stepID string, state State, iteration int, sourceID, key string, decision *AgentDecisionView) AgentVisitView {
	return AgentVisitView{
		VisitID: visitID, StepID: stepID, Iteration: iteration, State: state,
		Trigger:  AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{sourceID}, DecisionKey: key},
		Decision: decision,
	}
}

func agentDecisionSkippedVisit(visitID, stepID string, iteration int, sourceID, key string) AgentVisitView {
	return AgentVisitView{
		VisitID: visitID, StepID: stepID, Iteration: iteration, State: Skipped,
		Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{sourceID}, DecisionKey: key},
	}
}

func agentAfterVisit(visitID, stepID string, state State, iteration int, sourceIDs []string, decision *AgentDecisionView) AgentVisitView {
	return AgentVisitView{
		VisitID: visitID, StepID: stepID, Iteration: iteration, State: state,
		Trigger: AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: sourceIDs}, Decision: decision,
	}
}

func agentOutcome(outcome workflow.TerminalOutcome) *workflow.TerminalOutcome {
	return &outcome
}
