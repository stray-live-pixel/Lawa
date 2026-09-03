package scheduler

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// TestPlanAgentSkippedClosureBuildsNestedLayers воспроизводит полную причинную
// цепочку issue #50. Невыбранный decision-кубик становится Skipped, раскрывает
// все свои контрфактические routes, а полностью пропущенный after-барьер получает
// отдельную durable-квитанцию. Каждый вызов возвращает только готовый слой:
// вызывающая сторона должна сначала назначить его visits настоящие VisitID.
func TestPlanAgentSkippedClosureBuildsNestedLayers(t *testing.T) {
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"run":    {To: []string{"live"}},
			"unused": {To: []string{"nested"}},
		}),
		agentStep("live", nil, nil),
		agentStep("nested", nil, map[string]workflow.Route{
			"alpha": {To: []string{"left"}},
			"beta":  {To: []string{"right"}},
		}),
		agentStep("left", nil, nil),
		agentStep("right", nil, nil),
		agentStep("join", []string{"left", "right"}, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "run", Applied: true}),
		agentDecisionVisit("live-1", "live", Succeeded, 2, "choice-1", "run", nil),
	}

	first := requireSkippedClosure(t, w, visits, false)
	wantFirst := []AgentActivation{{
		StepID: "nested", Iteration: 2, InitialState: Skipped,
		Trigger: AgentTriggerView{
			Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "unused",
		},
	}}
	if !reflect.DeepEqual(first.DecisionActivations, wantFirst) {
		t.Fatalf("первый слой не материализовал внешнюю невыбранную ветку:\n got: %#v\nwant: %#v", first.DecisionActivations, wantFirst)
	}
	visits = append(visits, agentSkippedActivation("nested-skip", first.DecisionActivations[0]))

	second := requireSkippedClosure(t, w, visits, false)
	wantSecond := []AgentActivation{
		{
			StepID: "left", Iteration: 3, InitialState: Skipped,
			Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"nested-skip"}, DecisionKey: "alpha"},
		},
		{
			StepID: "right", Iteration: 3, InitialState: Skipped,
			Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"nested-skip"}, DecisionKey: "beta"},
		},
	}
	if !reflect.DeepEqual(second.DecisionActivations, wantSecond) {
		t.Fatalf("nested decision не раскрыл все routes в стабильном порядке:\n got: %#v\nwant: %#v", second.DecisionActivations, wantSecond)
	}
	visits = append(visits,
		agentSkippedActivation("left-skip", second.DecisionActivations[0]),
		agentSkippedActivation("right-skip", second.DecisionActivations[1]),
	)

	third := requireSkippedClosure(t, w, visits, false)
	wantAfter := []AgentActivation{{
		StepID: "join", Iteration: 3, InitialState: Skipped,
		Trigger: AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: []string{"left-skip", "right-skip"}},
	}}
	if !reflect.DeepEqual(third.AfterActivations, wantAfter) {
		t.Fatalf("all-skipped barrier не получил FIFO-квитанцию:\n got: %#v\nwant: %#v", third.AfterActivations, wantAfter)
	}
	visits = append(visits, agentSkippedActivation("join-skip", third.AfterActivations[0]))
	if final := requireSkippedClosure(t, w, visits, false); !reflect.DeepEqual(final, AgentPlan{}) {
		t.Fatalf("замкнутая история повторно создала skipped-visits: %#v", final)
	}
}

// TestPlanAgentSkippedClosureDeduplicatesRoutes проверяет два уровня dedupe.
// Реально выбранный общий target нельзя одновременно назвать пропущенным, а
// общий target двух невыбранных ключей получает один канонический trigger.
// При этом отдельный активный visit того же target не мешает синтетической
// записи: это разные причинные ветки, и Skipped не запускает второй executor.
func TestPlanAgentSkippedClosureDeduplicatesRoutes(t *testing.T) {
	w := agentWorkflow([]string{"choice", "busy"},
		agentStep("choice", nil, map[string]workflow.Route{
			"alpha": {To: []string{"shared", "busy"}},
			"beta":  {To: []string{"busy"}},
			"run":   {To: []string{"shared"}},
		}),
		agentStep("shared", nil, nil),
		agentStep("busy", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "run", Applied: true}),
		agentStartVisit("busy-1", "busy", Running, 1, nil),
		agentDecisionVisit("shared-1", "shared", Running, 2, "choice-1", "run", nil),
	}
	plan := requireSkippedClosure(t, w, visits, false)
	want := []AgentActivation{{
		StepID: "busy", Iteration: 2, InitialState: Skipped,
		Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "alpha"},
	}}
	if !reflect.DeepEqual(plan.DecisionActivations, want) {
		t.Fatalf("selected/shared target продублирован в skipped-ветке: got %#v, want %#v", plan.DecisionActivations, want)
	}
	visits = append(visits, agentSkippedActivation("busy-skip", plan.DecisionActivations[0]))
	if next := requireSkippedClosure(t, w, visits, false); !reflect.DeepEqual(next, AgentPlan{}) {
		t.Fatalf("сосуществующие active и skipped visits создали дубль: %#v", next)
	}
}

// TestPlanAgentSkippedClosureStopsDecisionSCC доказывает конечность обхода. Одна
// причинная волна проходит A→B→A, но второй A подавляется; независимые выходы из
// обоих decisions при этом не теряются. maxVisits=1 не мешает, потому что
// Skipped не означает фактический запуск агента и не расходует квоту.
func TestPlanAgentSkippedClosureStopsDecisionSCC(t *testing.T) {
	limit := 1
	a := agentStep("a", nil, map[string]workflow.Route{
		"continue": {To: []string{"b"}}, "leave": {To: []string{"leaf-a"}},
	})
	b := agentStep("b", nil, map[string]workflow.Route{
		"continue": {To: []string{"a"}}, "leave": {To: []string{"leaf-b"}},
	})
	a.MaxVisits, b.MaxVisits = &limit, &limit
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"run": {To: []string{"live"}}, "unused": {To: []string{"a"}},
		}),
		agentStep("live", nil, nil), a, b,
		agentStep("leaf-a", nil, nil), agentStep("leaf-b", nil, nil),
	)
	visits := []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "run", Applied: true}),
		agentDecisionVisit("live-1", "live", Succeeded, 2, "choice-1", "run", nil),
	}

	first := requireSkippedClosure(t, w, visits, false)
	visits = append(visits, agentSkippedActivation("a-skip", first.DecisionActivations[0]))
	second := requireSkippedClosure(t, w, visits, false)
	if got := skippedStepIDs(second.DecisionActivations); !reflect.DeepEqual(got, []string{"b", "leaf-a"}) {
		t.Fatalf("первый узел SCC потерял выходы: %v", got)
	}
	visits = append(visits,
		agentSkippedActivation("b-skip", second.DecisionActivations[0]),
		agentSkippedActivation("leaf-a-skip", second.DecisionActivations[1]),
	)
	third := requireSkippedClosure(t, w, visits, false)
	if got := skippedStepIDs(third.DecisionActivations); !reflect.DeepEqual(got, []string{"leaf-b"}) {
		t.Fatalf("обратное ребро SCC повторно создало A или потеряло выход: %v", got)
	}
	visits = append(visits, agentSkippedActivation("leaf-b-skip", third.DecisionActivations[0]))
	if final := requireSkippedClosure(t, w, visits, false); !reflect.DeepEqual(final, AgentPlan{}) {
		t.Fatalf("decision-SCC не достиг fixpoint: %#v", final)
	}
}

// TestPlanAgentSkippedClosureTerminalSources фиксирует две специальные причины
// terminal closure: применённый finish и не применённый decision, чей runnable
// target N+1 заменён maxVisits. В обоих случаях альтернативы остаются причинными
// Skipped; без terminal-доказательства такие snapshots отклоняются.
func TestPlanAgentSkippedClosureTerminalSources(t *testing.T) {
	t.Run("finish", func(t *testing.T) {
		w := agentWorkflow([]string{"choice"},
			agentStep("choice", nil, map[string]workflow.Route{
				"finish": {Finish: agentOutcome(workflow.OutcomeSucceeded)},
				"unused": {To: []string{"unused"}},
			}),
			agentStep("unused", nil, nil),
		)
		visits := []AgentVisitView{
			agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "finish", Applied: true}),
		}
		if _, err := PlanAgentSkippedClosure(w, visits, false, "", AgentTriggerView{}); err == nil || !strings.Contains(err.Error(), "terminal run") {
			t.Fatalf("running snapshot принял applied finish: %v", err)
		}
		plan := requireSkippedClosure(t, w, visits, true)
		if got := skippedStepIDs(plan.DecisionActivations); !reflect.DeepEqual(got, []string{"unused"}) {
			t.Fatalf("finish потерял невыбранную ветку: %v", got)
		}
	})

	t.Run("decision limit", func(t *testing.T) {
		limit := 1
		limited := agentStep("limited", nil, nil)
		limited.MaxVisits = &limit
		w := agentWorkflow([]string{"choice", "limited"},
			agentStep("choice", nil, map[string]workflow.Route{
				"retry": {To: []string{"limited"}}, "unused": {To: []string{"unused"}},
			}),
			limited, agentStep("unused", nil, nil),
		)
		visits := []AgentVisitView{
			agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "retry"}),
			agentStartVisit("limited-1", "limited", Succeeded, 1, nil),
		}
		trigger := AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "retry"}
		plan, err := PlanAgentSkippedClosure(w, visits, true, "limited", trigger)
		if err != nil || !reflect.DeepEqual(skippedStepIDs(plan.DecisionActivations), []string{"unused"}) {
			t.Fatalf("decision-limit не сохранил известную альтернативу: %#v, %v", plan, err)
		}
	})
}

// TestPlanAgentSkippedClosureRejectsForgery проверяет критические инварианты
// durable view: selected target нельзя подделать как невыбранный, а runnable
// after нельзя построить целиком из Skipped-источников.
func TestPlanAgentSkippedClosureRejectsForgery(t *testing.T) {
	t.Run("selected target", func(t *testing.T) {
		w := agentWorkflow([]string{"choice"},
			agentStep("choice", nil, map[string]workflow.Route{
				"run": {To: []string{"live"}}, "unused": {To: []string{"unused"}},
			}),
			agentStep("live", nil, nil), agentStep("unused", nil, nil),
		)
		visits := []AgentVisitView{
			agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "run", Applied: true}),
			agentDecisionVisit("live-1", "live", Succeeded, 2, "choice-1", "run", nil),
			{
				VisitID: "forged", StepID: "live", Iteration: 2, State: Skipped,
				Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "run"},
			},
		}
		if _, err := PlanAgentSkippedClosure(w, visits, false, "", AgentTriggerView{}); err == nil {
			t.Fatal("selected target принят как skipped")
		}
	})

	t.Run("runnable after from skipped", func(t *testing.T) {
		w := agentWorkflow([]string{"choice"},
			agentStep("choice", nil, map[string]workflow.Route{
				"run": {To: []string{"live"}}, "unused": {To: []string{"left"}},
			}),
			agentStep("live", nil, nil), agentStep("left", nil, nil),
			agentStep("join", []string{"left"}, nil),
		)
		visits := []AgentVisitView{
			agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "run", Applied: true}),
			agentDecisionVisit("live-1", "live", Succeeded, 2, "choice-1", "run", nil),
			{
				VisitID: "left-skip", StepID: "left", Iteration: 2, State: Skipped,
				Trigger: AgentTriggerView{Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{"choice-1"}, DecisionKey: "unused"},
			},
			agentAfterVisit("forged-join", "join", Pending, 2, []string{"left-skip"}, nil),
		}
		if _, err := PlanAgentSkippedClosure(w, visits, false, "", AgentTriggerView{}); err == nil || !strings.Contains(err.Error(), "skipped after") {
			t.Fatalf("runnable after из skipped-источников принят: %v", err)
		}
	})
}

// TestPlanAgentGraphDoesNotProduceSkipped сохраняет границу этого PR: основному
// producer ещё не поручено материализовывать альтернативы. Новый pure API можно
// интегрировать отдельно, не меняя поведение уже работающего планировщика.
func TestPlanAgentGraphDoesNotProduceSkipped(t *testing.T) {
	w := agentWorkflow([]string{"choice"},
		agentStep("choice", nil, map[string]workflow.Route{
			"run": {To: []string{"live"}}, "unused": {To: []string{"unused"}},
		}),
		agentStep("live", nil, nil), agentStep("unused", nil, nil),
	)
	plan, err := PlanAgentGraph(w, []AgentVisitView{
		agentStartVisit("choice-1", "choice", Succeeded, 1, &AgentDecisionView{Key: "run"}),
	})
	if err != nil || len(plan.DecisionActivations) != 1 || plan.DecisionActivations[0].StepID != "live" ||
		plan.DecisionActivations[0].InitialState != "" || plan.DecisionActivations[0].Trigger.Kind != AgentTriggerDecision {
		t.Fatalf("основной producer неожиданно изменил поведение: %#v, %v", plan, err)
	}
}

func requireSkippedClosure(t *testing.T, w workflow.Workflow, visits []AgentVisitView, terminal bool) AgentPlan {
	t.Helper()
	plan, err := PlanAgentSkippedClosure(w, visits, terminal, "", AgentTriggerView{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ApplyDecisionVisitIDs) != 0 || plan.Terminal != nil {
		t.Fatalf("skip-only API вернул исполняемое или terminal-действие: %#v", plan)
	}
	for _, activation := range append(append([]AgentActivation{}, plan.DecisionActivations...), plan.AfterActivations...) {
		if activation.InitialState != Skipped {
			t.Fatalf("skip-only API вернул не-Skipped активацию: %#v", activation)
		}
	}
	return plan
}

func agentSkippedActivation(visitID string, activation AgentActivation) AgentVisitView {
	return AgentVisitView{
		VisitID: visitID, StepID: activation.StepID, Iteration: activation.Iteration,
		State: Skipped, Trigger: activation.Trigger,
	}
}

func skippedStepIDs(activations []AgentActivation) []string {
	result := make([]string, 0, len(activations))
	for _, activation := range activations {
		result = append(result, activation.StepID)
	}
	return result
}
