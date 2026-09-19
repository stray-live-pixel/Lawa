package dashboard

import (
	"fmt"
	"strings"
	"time"
)

// migrationPreviewRoots — автономный снимок топологии sp-main-migration от
// 16.09.2026: только ID и связи, без промптов, путей, реальных результатов и
// запуска агентов. Состояния вымышлены для проверки UI. Изменение исходного
// workflow не меняет fixture автоматически: топологию обновляют явно.
func migrationPreviewRoots(now time.Time) []*runNode {
	ids := []string{"page-plan", "implement-1", "implement-2", "implement-3", "implement-4", "implement-5", "integrate", "review", "release", "optimization-plan", "optimize", "optimization-release", "report", "project-map", "final-report"}
	edges := []graphEdge{{From: "report", To: "page-plan", Label: ""}, {From: "page-plan", To: "project-map", Label: "finalize"}, {From: "page-plan", To: "implement-1", Label: "work_1"}, {From: "page-plan", To: "implement-1", Label: "work_2"}, {From: "page-plan", To: "implement-2", Label: "work_2"}, {From: "page-plan", To: "implement-1", Label: "work_3"}, {From: "page-plan", To: "implement-2", Label: "work_3"}, {From: "page-plan", To: "implement-3", Label: "work_3"}, {From: "page-plan", To: "implement-1", Label: "work_4"}, {From: "page-plan", To: "implement-2", Label: "work_4"}, {From: "page-plan", To: "implement-3", Label: "work_4"}, {From: "page-plan", To: "implement-4", Label: "work_4"}, {From: "page-plan", To: "implement-1", Label: "work_5"}, {From: "page-plan", To: "implement-2", Label: "work_5"}, {From: "page-plan", To: "implement-3", Label: "work_5"}, {From: "page-plan", To: "implement-4", Label: "work_5"}, {From: "page-plan", To: "implement-5", Label: "work_5"}, {From: "implement-1", To: "integrate", Label: ""}, {From: "implement-2", To: "integrate", Label: ""}, {From: "implement-3", To: "integrate", Label: ""}, {From: "implement-4", To: "integrate", Label: ""}, {From: "implement-5", To: "integrate", Label: ""}, {From: "integrate", To: "review", Label: ""}, {From: "review", To: "release", Label: ""}, {From: "release", To: "optimization-plan", Label: "optimize"}, {From: "release", To: "report", Label: "report"}, {From: "optimization-plan", To: "optimize", Label: ""}, {From: "optimize", To: "optimization-release", Label: ""}, {From: "optimization-release", To: "report", Label: "report"}, {From: "project-map", To: "final-report", Label: ""}}
	scenarios := []struct {
		id, label, state string
		completed        int
		current          string
	}{
		{"parallel", "параллельная работа", "running", 1, "implement-1"},
		{"cycle", "повторный цикл", "running", 1, "implement-2"},
		{"waiting", "ожидание подтверждения", "waiting_for_approval", 7, "review"},
		{"failed", "ошибка интеграции", "failed", 6, "integrate"},
		{"completed", "завершён", "succeeded", 15, ""},
		{"cancelled", "остановлен", "cancelled", 2, "implement-2"},
	}
	var roots []*runNode
	for index, scenario := range scenarios {
		id := "preview-migration-" + scenario.id
		graph := &graphView{Version: 2, ID: id, Name: "sp-main-migration · " + scenario.label, State: scenario.state,
			Prompt: "Демонстрационные данные для разработки UI. Агенты не запускаются.", Edges: edges}
		node := &runNode{ID: id, Name: graph.Name, State: scenario.state, Tone: tone(scenario.state),
			AgentGraph: true, TotalSteps: len(ids), PreviewGraph: graph,
			createdAt: now.Add(-time.Duration(index+1) * time.Hour), updatedAt: now,
			Updated: now.Format("2006-01-02 15:04:05")}
		// Предыдущий проход хранится отдельными visits, чтобы переключатель посещений
		// показывал историю цикла, а дерево и счётчик описывали последнее состояние.
		if scenario.id == "cycle" {
			for _, stepID := range ids[:13] {
				graph.Executions = append(graph.Executions, graphExecution{Key: stepID + "-1", StepID: stepID,
					State: "succeeded", Visit: 1, Attempt: 1, Result: "Демо: первый проход завершён.", Trigger: "Предыдущий цикл"})
			}
		}
		for i, stepID := range ids {
			state := "pending"
			if i < scenario.completed {
				state = "succeeded"
			}
			if stepID == scenario.current {
				state = scenario.state
			}
			if scenario.id == "parallel" && i >= 1 && i <= 5 {
				state = "running"
			}
			if scenario.state == "failed" || scenario.state == "cancelled" {
				if state == "pending" {
					state = "cancelled"
				}
			}
			visit := 1
			if scenario.id == "cycle" {
				visit = 2
			}
			key := fmt.Sprintf("%s-%d", stepID, visit)
			item := stepNode{ID: stepID, StepID: stepID, Key: key, VisitID: key, Visit: visit, Attempt: 1,
				State: state, Tone: tone(state), Active: state == "running" || state == "waiting_for_approval"}
			if state == "succeeded" {
				node.CompletedSteps++
			}
			if state == "running" {
				item.Action = "commandExecution"
			}
			if state == "failed" {
				item.Message = "Демо: проверка интеграции завершилась ошибкой."
			}
			node.Steps = append(node.Steps, item)
			if item.Active {
				node.ActiveSteps = append(node.ActiveSteps, item)
			}
			routes := []string{}
			for _, edge := range edges {
				if edge.From == stepID {
					routes = append(routes, edge.Label+" → "+edge.To)
				}
			}
			graph.Nodes = append(graph.Nodes, graphNode{ID: stepID, Start: stepID == "page-plan", Prompt: "Демонстрационный кубик", Routes: routes})
			// pending ещё не имеет посещения; завершённый результат не показываем заранее.
			if state != "pending" {
				result := ""
				if state == "succeeded" {
					result = "## Демонстрационный результат\n\nКубик **" + stepID + "** завершён.\n\n- Проверки выполнены.\n- Результат передан следующему этапу."
				}
				graph.Executions = append(graph.Executions, graphExecution{Key: key, StepID: stepID,
					State: state, Visit: visit, Attempt: 1, Result: result, Note: item.Message})
			}
		}
		node.searchText = strings.Join(append([]string{node.ID, node.Name, node.State}, ids...), " ")
		roots = append(roots, node)
	}
	return roots
}
