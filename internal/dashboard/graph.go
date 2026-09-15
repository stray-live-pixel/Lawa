package dashboard

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// graphHTML — самостоятельный экран без CDN и renderer. Начальные данные
// встроены через html/template, который экранирует их в JavaScript-контексте.
//
//go:embed graph.html
var graphHTML string

var graphTemplate = template.Must(template.New("graph").Parse(graphHTML))

// graphView разделяет неизменяемую схему workflow и историю её исполнений.
// Поэтому ещё не посещённый кубик виден, а цикл не затирает предыдущий результат.
type graphView struct {
	ID, Name, State, StopReason, Prompt string
	Nodes                               []graphNode
	Edges                               []graphEdge
	Executions                          []graphExecution
}

type graphNode struct {
	ID, Prompt string
	Routes     []string
}

type graphEdge struct{ From, To, Label string }

// graphExecution содержит только данные конкретного step/visit. Память и trace
// загружаются отдельно по существующим защищённым маршрутам, не для всего графа.
type graphExecution struct {
	Key, StepID, State, Result, Note, Decision, Trigger string
	TraceURL, MemoryURL, Prompt                         string
	Visit, Attempt                                      int
}

// graph показывает тот же снимок, что API; обход путей и повреждённые run
// отклоняет runstore. Ни HTML, ни JSON с приватными результатами не кэшируются.
func (h handler) graph(w http.ResponseWriter, r *http.Request) {
	view, err := h.loadGraph(r.PathValue("run"))
	if err != nil {
		http.Error(w, "Не удалось прочитать запуск: "+diagnostic(err), http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := graphTemplate.Execute(w, view); err != nil {
		http.Error(w, "Не удалось построить граф", http.StatusInternalServerError)
	}
}

// loadGraph строит рёбра по workflow, а не по порядку строк metadata. after и
// dependsOn направлены от источника к получателю; именованные to — от решения.
// finish и onLimit показаны текстом у кубика и не притворяются отдельными шагами.
func (h handler) loadGraph(runID string) (graphView, error) {
	snapshot, err := runstore.LoadForDashboard(h.root, runID)
	if err != nil {
		return graphView{}, err
	}
	root, err := filepath.Abs(h.root)
	if err != nil {
		return graphView{}, err
	}
	node := makeRunNode(h.root, snapshot)
	view := graphView{ID: runID, Name: node.Name, State: node.State, StopReason: node.StopReason, Prompt: continuationPrompt(root, snapshot, "", "")}
	for _, step := range snapshot.Workflow.Steps {
		item := graphNode{ID: step.ID, Prompt: continuationPrompt(root, snapshot, step.ID, "")}
		for _, source := range append(append([]string{}, step.DependsOn...), step.After...) {
			view.Edges = append(view.Edges, graphEdge{From: source, To: step.ID, Label: "после"})
		}
		for _, key := range sortedRouteKeys(step.Decisions) {
			route := step.Decisions[key]
			item.Routes = append(item.Routes, key+" → "+formatRouteDestination(route))
			for _, target := range route.To {
				view.Edges = append(view.Edges, graphEdge{From: step.ID, To: target, Label: key})
			}
		}
		if limit := formatVisitLimit(step); limit != "" {
			item.Routes = append(item.Routes, limit)
		}
		view.Nodes = append(view.Nodes, item)
	}
	events, eventErr := runstore.ReadEvents(h.root, runID)
	// Один проход по журналу вместо полного сканирования для каждого кубика.
	// Префиксы не дают legacy stepID столкнуться с visitID другой версии.
	byExecution := make(map[string][]runstore.RuntimeEvent)
	for _, event := range events {
		key := "step:" + event.StepID
		if event.VisitID != "" {
			key = "visit:" + event.VisitID
		}
		if event.Kind == "item_completed" && event.ItemType == "agentMessage" {
			byExecution[key] = append(byExecution[key], event)
		}
	}
	for _, step := range node.Steps {
		key := "step:" + step.StepID
		if step.VisitID != "" {
			key = "visit:" + step.VisitID
		}
		result, note := executionResult(step, byExecution[key])
		if eventErr != nil {
			// Ошибка журнала не должна выглядеть как отсутствие ответа агента.
			result, note = "", "Не удалось прочитать отчёт: "+diagnostic(eventErr)
		}
		entry := graphExecution{Key: step.Key, StepID: step.StepID, State: step.State,
			Visit: step.Visit, Attempt: step.Attempt, Result: result, Note: note,
			Decision: strings.Join(nonemptyStrings(step.Decision, step.Explanation, step.Transition), " · "),
			Trigger:  step.Trigger, TraceURL: string(step.TraceURL)}
		entry.Prompt = continuationPrompt(root, snapshot, step.StepID, step.VisitID)
		if step.HasMemory {
			entry.MemoryURL = string(step.MemoryURL)
		}
		view.Executions = append(view.Executions, entry)
	}
	return view, nil
}

// executionResult принимает только завершённое сообщение с договорённым
// маркером. Delta и промежуточный ответ никогда не выдаются за финальный отчёт.
// При resume turnID ограничивает отчёт текущей попыткой; visitID изолирует циклы.
// Чтение старых журналов не создаёт выдуманный отчёт из последнего сообщения.
func executionResult(step stepNode, events []runstore.RuntimeEvent) (string, string) {
	switch step.State {
	case "pending":
		return "", "Кубик ещё не запускался."
	case "starting", "running", "waiting_for_approval":
		return "", "Работа ещё не завершена. Итог появится после завершения."
	case "skipped":
		return "", "Ветка пропущена. Агент не запускался."
	}
	result := ""
	for _, event := range events {
		if step.VisitID != "" && event.VisitID != step.VisitID || step.VisitID == "" && event.StepID != step.StepID {
			continue
		}
		if step.turnID != "" && event.TurnID != step.turnID {
			continue
		}
		if event.Kind == "item_completed" && event.ItemType == "agentMessage" {
			text := strings.TrimSpace(event.Content)
			if strings.HasPrefix(text, "Итог:") {
				result = text
			}
		}
	}
	note := strings.Join(nonemptyStrings(step.TechnicalError, step.DecisionError), "\n")
	if result == "" {
		note = strings.TrimSpace(note + "\nФинальный отчёт не сохранён. Подробности — в сообщениях и памяти кубика.")
	}
	return result, note
}

// nonemptyStrings не создаёт пустые разделители в операторской диагностике.
func nonemptyStrings(values ...string) []string {
	var result []string
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
