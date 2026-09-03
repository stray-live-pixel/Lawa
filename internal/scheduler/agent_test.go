package scheduler

import (
	"errors"
	"reflect"
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

// TestPlanAgentGraphRejectsCycles проверяет временную production-границу PR3a.
// Оба workflow валидны по публичному v2-контракту благодаря maxVisits/onLimit,
// однако pure planner возвращает узнаваемый sentinel до проверки visits.
func TestPlanAgentGraphRejectsCycles(t *testing.T) {
	limit := 2
	onLimit := workflow.OutcomeFailed
	tests := []struct {
		name  string
		start []string
		steps []workflow.Step
	}{
		{
			name: "self loop", start: []string{"loop"},
			steps: []workflow.Step{{
				ID: "loop", Type: "agent", Prompt: "Повтори", After: []string{}, MaxVisits: &limit, OnLimit: &onLimit,
				Decisions: map[string]workflow.Route{"again": {To: []string{"loop"}}},
			}},
		},
		{
			name: "two nodes", start: []string{"first"},
			steps: []workflow.Step{
				{ID: "first", Type: "agent", Prompt: "Первый", After: []string{}, MaxVisits: &limit, OnLimit: &onLimit,
					Decisions: map[string]workflow.Route{"next": {To: []string{"second"}}}},
				{ID: "second", Type: "agent", Prompt: "Второй", After: []string{}, MaxVisits: &limit, OnLimit: &onLimit,
					Decisions: map[string]workflow.Route{"again": {To: []string{"first"}}}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version := workflow.VersionAgentGraph
			w := workflow.Workflow{Version: &version, ID: "cycle", Start: tc.start, Steps: tc.steps}
			if err := w.Validate(); err != nil {
				t.Fatalf("fixture обязан быть допустимым будущим v2-графом: %v", err)
			}
			got, err := PlanAgentGraph(w, nil)
			if !errors.Is(err, ErrAgentGraphCycle) || !reflect.DeepEqual(got, AgentPlan{}) {
				t.Fatalf("цикл не остановлен sentinel-ошибкой: %#v, %v", got, err)
			}
		})
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

func agentOutcome(outcome workflow.TerminalOutcome) *workflow.TerminalOutcome {
	return &outcome
}
