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

// TestPlanAgentGraphNaturalQuiescence проверяет фактический causal frontier, а не
// все исторические ошибки. Не выбранная половина статического join не блокирует
// естественный успех, но необработанный Failed-лист завершает run ошибкой.
func TestPlanAgentGraphNaturalQuiescence(t *testing.T) {
	t.Run("stranded join succeeds", func(t *testing.T) {
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
		if err != nil || got.Terminal == nil || got.Terminal.Outcome != workflow.OutcomeSucceeded || got.Terminal.CauseVisitID != "" {
			t.Fatalf("невыбранная ветка заблокировала natural completion: %#v, %v", got, err)
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
		len(got.ApplyDecisionVisitIDs) != 0 || len(got.DecisionActivations) != 0 || !strings.Contains(got.Terminal.Reason, "посещение 3") {
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
// свободный target не материализуются частично.
func TestPlanAgentGraphLimitKeepsFanoutAtomic(t *testing.T) {
	limit := 1
	succeeded := workflow.OutcomeSucceeded
	choice := agentStep("choice", nil, map[string]workflow.Route{"go": {To: []string{"beta", "alpha", "free"}}})
	beta, alpha := agentStep("beta", nil, nil), agentStep("alpha", nil, nil)
	beta.MaxVisits, beta.OnLimit, alpha.MaxVisits = &limit, &succeeded, &limit
	w := agentWorkflow([]string{"choice", "beta", "alpha"}, choice, beta, alpha, agentStep("free", nil, nil))
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
		len(got.ApplyDecisionVisitIDs) != 0 || len(got.DecisionActivations) != 0 {
		t.Fatalf("limit fanout нарушил порядок или атомарность: %#v, %v", got, err)
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

func agentAfterVisit(visitID, stepID string, state State, iteration int, sourceIDs []string, decision *AgentDecisionView) AgentVisitView {
	return AgentVisitView{
		VisitID: visitID, StepID: stepID, Iteration: iteration, State: state,
		Trigger: AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: sourceIDs}, Decision: decision,
	}
}

func agentOutcome(outcome workflow.TerminalOutcome) *workflow.TerminalOutcome {
	return &outcome
}
