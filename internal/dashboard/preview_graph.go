package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// graphPreviewRoots — безопасные воспроизводимые состояния #146. Они не читают
// пользовательские запуски, не вызывают агентов и явно обозначены как демо.
func graphPreviewRoots(now time.Time) []*runNode {
	maxVisits := 5
	nodes := []graphNode{
		{ID: "developer", Title: "Программист", Icon: "Code", Start: true, MaxVisits: &maxVisits},
		{ID: "reviewer", Title: "Ревьювер", Icon: "BranchesDown", MaxVisits: &maxVisits},
		{ID: "qa_frontend", Title: "QA frontend", Icon: "BranchesDown", MaxVisits: &maxVisits},
		{ID: "qa", Title: "QA", Icon: "ShieldCheck", MaxVisits: &maxVisits},
	}
	edges := []graphEdge{{From: "developer", To: "reviewer"}, {From: "reviewer", To: "qa_frontend", Key: "approve", Label: "Ревью пройдено"}, {From: "qa_frontend", To: "qa", Key: "passed", Label: "UI проверен / не менялся"}, {From: "qa", Finish: "succeeded", Key: "passed", Label: "Проверки пройдены"}}
	for _, id := range []string{"reviewer", "qa_frontend", "qa"} {
		key, label := "changes_requested", "Есть замечания"
		if id == "qa_frontend" {
			label = "Замечания к UI"
		}
		if id == "qa" {
			key, label = "failed", "Найдены дефекты"
		}
		edges = append(edges, graphEdge{From: id, To: "developer", Key: key, Label: label}, graphEdge{From: id, Finish: "failed", Key: "blocked", Label: "Агент не может продолжить"})
	}
	var result []*runNode
	for _, scenario := range []struct{ id, label, state string }{{"first", "первый запуск", "running"}, {"review-return", "возврат ревьювера", "running"}, {"ui-return", "возврат QA frontend", "running"}, {"succeeded", "успех", "succeeded"}, {"failed", "ошибка агента", "failed"}, {"limit", "лимит попыток", "failed"}, {"cancelled", "отмена", "cancelled"}, {"legacy", "история без причин", "running"}} {
		graph := &graphView{ID: "preview-graph-" + scenario.id, Version: 2, Name: "Демо графа · " + scenario.label, State: scenario.state, Nodes: nodes, Edges: edges}
		start := runstore.VisitTrigger{Kind: runstore.TriggerStart}
		graph.Executions = []graphExecution{{Key: "developer-1", StepID: "developer", Visit: 1, Attempt: 1, State: "running", Cause: &start, Note: "Демонстрационные данные; агенты не запускались."}}
		if scenario.id != "first" {
			graph.Executions[0].State = "succeeded"
			graph.Executions = append(graph.Executions, graphExecution{Key: "reviewer-1", StepID: "reviewer", Visit: 1, Attempt: 1, State: "succeeded", Cause: &runstore.VisitTrigger{Kind: runstore.TriggerAfter, SourceVisitIDs: []string{"developer-1"}}})
		}
		if scenario.id == "review-return" || scenario.id == "ui-return" || scenario.id == "legacy" {
			source := "reviewer-1"
			if scenario.id == "ui-return" {
				graph.Executions = append(graph.Executions, graphExecution{Key: "qa_frontend-1", StepID: "qa_frontend", Visit: 1, Attempt: 1, State: "succeeded", Cause: &runstore.VisitTrigger{Kind: runstore.TriggerDecision, SourceVisitIDs: []string{"reviewer-1"}, DecisionKey: "approve"}})
				source = "qa_frontend-1"
			}
			graph.Executions = append(graph.Executions, graphExecution{Key: "developer-2", StepID: "developer", Visit: 2, Attempt: 1, State: "running", Cause: &runstore.VisitTrigger{Kind: runstore.TriggerDecision, SourceVisitIDs: []string{source}, DecisionKey: "changes_requested"}})
		}
		if scenario.id == "succeeded" {
			graph.Executions = append(graph.Executions,
				graphExecution{Key: "qa_frontend-1", StepID: "qa_frontend", Visit: 1, Attempt: 1, State: "succeeded", Cause: &runstore.VisitTrigger{Kind: runstore.TriggerDecision, SourceVisitIDs: []string{"reviewer-1"}, DecisionKey: "approve"}},
				graphExecution{Key: "qa-1", StepID: "qa", Visit: 1, Attempt: 1, State: "succeeded", Cause: &runstore.VisitTrigger{Kind: runstore.TriggerDecision, SourceVisitIDs: []string{"qa_frontend-1"}, DecisionKey: "passed"}})
			outcome := workflow.OutcomeSucceeded
			graph.Executions[len(graph.Executions)-1].DecisionRecord = &runstore.DecisionRecord{Key: "passed", Finish: &outcome, Applied: true}
			graph.StopVisitID = "qa-1"
		}
		if scenario.id == "failed" {
			outcome := workflow.OutcomeFailed
			graph.Executions[1].DecisionRecord = &runstore.DecisionRecord{Key: "blocked", Finish: &outcome, Applied: true}
			graph.StopVisitID = "reviewer-1"
			graph.StopReason = "Демо: агент не может продолжить"
		}
		if scenario.id == "cancelled" {
			graph.Executions[len(graph.Executions)-1].State = "cancelled"
		}

		if scenario.id == "limit" {
			graph.StopReason = "Демо: исчерпан maxVisits=5 у developer"
			for i := 2; i <= 5; i++ {
				graph.Executions = append(graph.Executions, graphExecution{Key: fmt.Sprintf("developer-%d", i), StepID: "developer", Visit: i, Attempt: 1, State: "succeeded"})
			}
		}
		if scenario.id == "legacy" {
			for i := range graph.Executions {
				graph.Executions[i].Cause = nil
			}
		}
		root := &runNode{ID: graph.ID, Name: graph.Name, State: graph.State, Tone: tone(graph.State), AgentGraph: true, PreviewGraph: graph, TotalSteps: len(nodes), createdAt: now, updatedAt: now, Updated: now.Format("2006-01-02 15:04:05")}
		for _, n := range nodes {
			state, visit, key := "pending", 0, ""
			for _, e := range graph.Executions {
				if e.StepID == n.ID {
					state, visit, key = e.State, e.Visit, e.Key
				}
			}
			if state == "succeeded" {
				root.CompletedSteps++
			}
			root.Steps = append(root.Steps, stepNode{ID: n.ID, StepID: n.ID, Key: key, VisitID: key, Visit: visit, State: state, Tone: tone(state)})
		}
		root.searchText = strings.Join([]string{root.ID, root.Name, root.State}, " ")
		result = append(result, root)
	}
	return result
}

// denseGraphPreviewRoots покрывает рост боковых граней и join из нескольких
// стартов. Схемы малы и создаются только для явного /preview, не для live API.
func denseGraphPreviewRoots(now time.Time) []*runNode {
	var roots []*runNode
	for _, count := range []int{2, 8, 10} {
		id := fmt.Sprintf("preview-graph-ports-%d", count)
		name := fmt.Sprintf("Демо графа · %d входов", count)
		if count == 2 {
			name = "Демо графа · два входа"
		}
		if count == 8 {
			name = "Демо графа · восемь выходов"
		}
		g := &graphView{Version: 2, ID: id, Name: name, State: "running"}
		hubTitle := "Объединить результаты"
		if count == 8 {
			hubTitle = "Запустить проверки"
		}
		g.Nodes = append(g.Nodes, graphNode{ID: "hub", Title: hubTitle, Icon: "BranchesDown", Start: count == 8})
		sources := []string{}
		for i := 0; i < count; i++ {
			key := fmt.Sprintf("check-%d", i+1)
			g.Nodes = append(g.Nodes, graphNode{ID: key, Title: fmt.Sprintf("Проверка %d", i+1), Icon: "ShieldCheck", Start: count != 8})
			from, to := "hub", key
			if count != 8 {
				from, to = key, "hub"
				sources = append(sources, key+"-1")
			}
			g.Edges = append(g.Edges, graphEdge{From: from, To: to})
			entry := graphExecution{Key: key + "-1", StepID: key, Visit: 1, Attempt: 1, State: "succeeded", Cause: &runstore.VisitTrigger{Kind: runstore.TriggerStart}}
			if count == 8 {
				entry.State = "running"
				entry.Cause = &runstore.VisitTrigger{Kind: runstore.TriggerAfter, SourceVisitIDs: []string{"hub-1"}}
			}
			g.Executions = append(g.Executions, entry)
		}
		cause := &runstore.VisitTrigger{Kind: runstore.TriggerAfter, SourceVisitIDs: sources}
		if count == 8 {
			cause = &runstore.VisitTrigger{Kind: runstore.TriggerStart}
		}
		hubState := "running"
		if count == 8 {
			hubState = "succeeded"
		}
		g.Executions = append(g.Executions, graphExecution{Key: "hub-1", StepID: "hub", Visit: 1, Attempt: 1, State: hubState, Cause: cause})
		if count == 10 {
			for _, key := range []string{"publish", "notify"} {
				g.Nodes = append(g.Nodes, graphNode{ID: key, Title: key, Icon: "Code"})
				g.Edges = append(g.Edges, graphEdge{From: "hub", To: key})
			}
		}
		root := &runNode{ID: id, Name: name, State: "running", Tone: tone("running"), AgentGraph: true, PreviewGraph: g, TotalSteps: len(g.Nodes), createdAt: now, updatedAt: now, Updated: now.Format("2006-01-02 15:04:05"), searchText: name}
		for _, n := range g.Nodes {
			state := "pending"
			for _, entry := range g.Executions {
				if entry.StepID == n.ID {
					state = entry.State
				}
			}
			if state == "succeeded" {
				root.CompletedSteps++
			}
			root.Steps = append(root.Steps, stepNode{ID: n.ID, StepID: n.ID, Key: n.ID + "-1", VisitID: n.ID + "-1", Visit: 1, State: state, Tone: tone(state)})
		}
		roots = append(roots, root)
	}
	return roots
}
