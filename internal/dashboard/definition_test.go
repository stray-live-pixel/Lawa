package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// TestDefinitionExample проверяет реальный цикл без runstore: старт, оба возврата,
// terminal outcomes, лимиты и раскрытый вложенный шаблон остаются в снимке.
func TestDefinitionExample(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "development-cycle.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, definition, err := workflow.ResolveSource(raw, path, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	handler := DefinitionHandler(data, definition)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/definition", nil))
	var graph graphView
	if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	if !graph.Definition || graph.Version != 2 || graph.ID != "" || len(graph.Executions) != 0 || len(graph.Nodes) != 3 || len(graph.Edges) != 4 {
		t.Fatalf("неверный граф: %+v", graph)
	}
	for i, node := range graph.Nodes {
		if node.Definition.Start != (i == 0) || node.Definition.Character == nil || strings.Contains(node.Prompt, "{{") || !strings.Contains(node.Prompt, "После каждого посещения") {
			t.Fatalf("потеряны входы шага: %+v", node)
		}
		if !strings.Contains(strings.Join(node.Routes, "\n"), "maxVisits=5 · onLimit=failed") {
			t.Fatal(node.Routes)
		}
	}
	if !strings.Contains(strings.Join(graph.Nodes[2].Routes, "\n"), "passed → finish:succeeded") {
		t.Fatal(graph.Nodes[2].Routes)
	}
	for _, edge := range []graphEdge{{"reviewer", "developer", "changes_requested"}, {"qa", "developer", "failed"}} {
		found := false
		for _, actual := range graph.Edges {
			if actual == edge {
				found = true
			}
		}
		if !found {
			t.Fatalf("нет возврата %+v", edge)
		}
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/definition/source", nil))
	var source sourceView
	if err := json.Unmarshal(response.Body.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	if len(source.Documents) != 3 || !strings.Contains(source.Documents[0].Content, "После каждого посещения") || !json.Valid(source.JSON) {
		t.Fatal("исходники не раскрыты")
	}
}

// TestDefinitionIsolation защищает отдельный сервер от добавления API офиса,
// произвольных файлов и записей. Разрешены только снимок и встроенные ресурсы.
func TestDefinitionIsolation(t *testing.T) {
	raw := []byte(`{"id":"dag","model":"shared","steps":[{"id":"a","type":"agent","prompt":"A","dependsOn":[]},{"id":"b","type":"agent","prompt":"B","dependsOn":["a"],"model":"own","effort":"high","speed":"fast"}]}`)
	data, definition, err := workflow.ResolveSource(raw, "workflow.json", os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	graph := definitionGraph(definition)
	if graph.Version != 1 || !graph.Nodes[0].Definition.Start || graph.Nodes[1].Definition.Start || graph.Nodes[0].Definition.ModelSource != "workflow.model" || graph.Nodes[1].Definition.ModelSource != "step.model" || graph.Nodes[1].Definition.Effort != "high" || graph.Nodes[1].Definition.Speed != "fast" {
		t.Fatalf("неверное наследование: %+v", graph.Nodes)
	}
	handler := DefinitionHandler(data, definition)
	for _, path := range []string{"/", "/office", "/api/dashboard", "/api/graph/run", "/api/source/run", "/memory/run/task.md", "/api/definition/../../etc/passwd", "/api/definition/source/file", "/ui/missing", "/workflow.json"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code == 200 {
			t.Fatalf("путь доступен: %s", path)
		}
	}
	for _, path := range []string{"/view", "/api/definition", "/api/definition/source"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
		if response.Code != 405 {
			t.Fatalf("запись разрешена: %s %d", path, response.Code)
		}
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path+"?path=/etc/passwd", nil))
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "root:") {
			t.Fatalf("снимок небезопасен: %s", path)
		}
	}
}
