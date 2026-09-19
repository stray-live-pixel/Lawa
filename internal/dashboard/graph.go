package dashboard

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// graphView разделяет неизменяемую схему workflow и историю её исполнений.
// Поэтому ещё не посещённый кубик виден, а цикл не затирает предыдущий результат.
type graphView struct {
	StopVisitID                         string `json:",omitempty"`
	Definition                          bool   `json:",omitempty"`
	Version                             int    `json:",omitempty"`
	ID, Name, State, StopReason, Prompt string
	Nodes                               []graphNode
	Edges                               []graphEdge
	Executions                          []graphExecution
}

type graphNode struct {
	Icon, Title string                    `json:",omitempty"`
	Start       bool                      `json:",omitempty"`
	MaxVisits   *int                      `json:",omitempty"`
	OnLimit     *workflow.TerminalOutcome `json:",omitempty"`
	Definition  *stepDefinition           `json:",omitempty"`
	ID, Prompt  string
	Routes      []string
}

type graphEdge struct {
	From, To, Label string
	Key             string `json:",omitempty"`
	Finish          string `json:",omitempty"`
}

// graphExecution содержит только данные конкретного step/visit. Память и trace
// загружаются отдельно по существующим защищённым маршрутам, не для всего графа.
type graphExecution struct {
	// RunNumber считает только разрешённые активации: skipped не расходует maxVisits.
	// Visit остаётся исходным номером истории, Attempt — техническим повтором turn.
	RunNumber                                           int                      `json:",omitempty"`
	Cause                                               *runstore.VisitTrigger   `json:",omitempty"`
	DecisionRecord                                      *runstore.DecisionRecord `json:",omitempty"`
	Key, StepID, State, Result, Note, Decision, Trigger string
	TraceURL, MemoryURL, Prompt                         string
	Visit, Attempt                                      int
}

// graph отдаёт JSON снимка; обход путей и повреждённые run отклоняет runstore.
// Приватные результаты не кешируются. Статическую оболочку обслуживает serveUI.
func (h handler) graph(w http.ResponseWriter, r *http.Request) {
	view, err := h.loadGraph(r.PathValue("run"))
	if err != nil {
		http.Error(w, "Не удалось прочитать запуск: "+diagnostic(err), http.StatusNotFound)
		return
	}
	writeJSON(w, view)
}

// loadGraph строит рёбра по workflow, а не по порядку строк metadata. after и
// dependsOn направлены от источника к получателю; именованные to — от решения.
// finish сохраняет отдельный outcome для визуального маркера; onLimit остаётся
// правилом шага. Маркеры интерфейса не добавляются в исполняемый workflow.
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
	view.Version = snapshot.Workflow.EffectiveVersion()
	view.StopVisitID = snapshot.Meta.StopVisitID
	view.Nodes, view.Edges = definitionTopology(snapshot.Workflow)
	visits := make(map[string]runstore.Visit)
	runNumbers := make(map[string]int)
	counts := make(map[string]int)
	for _, visit := range snapshot.Meta.Visits {
		visits[visit.VisitID] = visit
		if string(visit.State) != "skipped" {
			counts[visit.StepID]++
			runNumbers[visit.VisitID] = counts[visit.StepID]
		}
	}
	for i := range view.Nodes {
		view.Nodes[i].Prompt = continuationPrompt(root, snapshot, view.Nodes[i].ID, "")
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
		if step.Result != "" {
			result, note = step.Result, strings.Join(nonemptyStrings(step.TechnicalError, step.DecisionError), "\n")
		}
		if eventErr != nil && step.Result == "" {
			// Ошибка журнала не должна выглядеть как отсутствие ответа агента.
			result, note = "", "Не удалось прочитать отчёт: "+diagnostic(eventErr)
		}
		entry := graphExecution{Key: step.Key, StepID: step.StepID, State: step.State,
			Visit: step.Visit, Attempt: step.Attempt, Result: result, Note: note,
			Decision: strings.Join(nonemptyStrings(step.Decision, step.Explanation, step.Transition), " · "),
			Trigger:  step.Trigger, TraceURL: string(step.TraceURL)}
		if visit, ok := visits[step.VisitID]; ok {
			cause := visit.Trigger
			entry.Cause, entry.DecisionRecord = &cause, visit.Decision
			entry.RunNumber = runNumbers[step.VisitID]
		}
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

// definitionTopology — общий источник рёбер и маршрутов для определения и run.
// История исполнения добавляется отдельно и не меняет структуру workflow.
func definitionTopology(definition workflow.Workflow) (nodes []graphNode, edges []graphEdge) {
	for _, step := range definition.Steps {
		item := graphNode{ID: step.ID, MaxVisits: step.MaxVisits, OnLimit: step.OnLimit,
			Start: slices.Contains(definition.Start, step.ID)}
		if definition.EffectiveVersion() == workflow.VersionLegacy {
			item.Start = len(step.DependsOn) == 0
		}
		if step.Icon != nil {
			item.Icon = *step.Icon
		}
		if character, ok := definition.Characters[step.Character]; ok {
			item.Title = character.Name
		}
		for _, source := range append(append([]string{}, step.DependsOn...), step.After...) {
			// Направление зависимости уже показывает стрелка. Подписи нужны только
			// именованным решениям, в том числе если сам маршрут назван «после».
			edges = append(edges, graphEdge{From: source, To: step.ID})
		}
		for _, key := range sortedRouteKeys(step.Decisions) {
			route := step.Decisions[key]
			item.Routes = append(item.Routes, key+" → "+formatRouteDestination(route))
			label := key
			if route.Label != nil {
				label = *route.Label
			}
			if route.Finish != nil {
				edges = append(edges, graphEdge{From: step.ID, Label: label, Key: key, Finish: string(*route.Finish)})
			}
			for _, target := range route.To {
				edges = append(edges, graphEdge{From: step.ID, To: target, Label: label, Key: key})
			}
		}
		if limit := formatVisitLimit(step); limit != "" {
			item.Routes = append(item.Routes, limit)
		}
		nodes = append(nodes, item)
	}
	return nodes, edges
}
