package dashboard

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// stepDefinition хранит только известные до запуска данные. Отсутствующая
// настройка не подменяется текущей конфигурацией Codex на машине просмотрщика.
type stepDefinition struct {
	Start                             bool
	CharacterID                       string
	Character                         *workflow.Character
	Model, ModelSource, Effort, Speed string
}

// definitionGraph дополняет общую топологию инструкциями и наследованием.
// Для v1 стартуют все шаги без dependsOn; v2 использует только явный start.
func definitionGraph(definition workflow.Workflow) graphView {
	nodes, edges := definitionTopology(definition)
	for i, step := range definition.Steps {
		details := &stepDefinition{Start: slices.Contains(definition.Start, step.ID), CharacterID: step.Character}
		if definition.EffectiveVersion() == workflow.VersionLegacy {
			details.Start = len(step.DependsOn) == 0
		}
		if step.Character != "" {
			character := definition.Characters[step.Character]
			details.Character = &character
		}
		if step.Model != nil {
			details.Model, details.ModelSource = *step.Model, "step.model"
		} else if definition.Model != nil {
			details.Model, details.ModelSource = *definition.Model, "workflow.model"
		}
		if step.Effort != nil {
			details.Effort = *step.Effort
		}
		if step.Speed != nil {
			details.Speed = string(*step.Speed)
		}
		nodes[i].Prompt, nodes[i].Definition = step.Prompt, details
	}
	return graphView{Name: definition.ID, Definition: true, Version: definition.EffectiveVersion(), Nodes: nodes, Edges: edges}
}

// DefinitionHandler обслуживает один раскрытый снимок. Здесь нет root, runstore,
// офисного движка и маршрутов чтения файлов: браузер не задаёт путь к исходникам.
// Вызывать после ResolveSource; JSON и definition должны относиться к одному снимку.
func DefinitionHandler(data []byte, definition workflow.Workflow) http.Handler {
	graph := definitionGraph(definition)
	source := sourceView{JSON: json.RawMessage(slices.Clone(data)), Documents: instructionDocuments(definition),
		Note: "Проверенный JSON workflow. Markdown и шаблоны раскрыты при открытии; изменения файлов видны после перезапуска lawa view."}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /view", serveUI)
	mux.HandleFunc("GET /ui/", serveUIAssets)
	mux.HandleFunc("GET /api/definition", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, graph) })
	mux.HandleFunc("GET /api/definition/source", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, source) })
	return mux
}

// ServeDefinition закрывает только переданный listener при отмене контекста.
// Отдельный вход исключает запуск сохранённых команд через ServeWithStartup.
func ServeDefinition(ctx context.Context, listener net.Listener, data []byte, definition workflow.Workflow) error {
	return serve(ctx, listener, DefinitionHandler(data, definition))
}
