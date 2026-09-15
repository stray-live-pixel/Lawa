package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// TestGraphIncludesUnstartedDependencies проверяет реальную топологию обеих
// версий: порядок steps намеренно не является порядком выполнения.
func TestGraphIncludesUnstartedDependencies(t *testing.T) {
	for _, source := range []string{
		`{"id":"dag","steps":[{"id":"b","type":"agent","prompt":"b","dependsOn":["a"]},{"id":"a","type":"agent","prompt":"a","dependsOn":[]}]}`,
		`{"version":2,"id":"agent","start":["a"],"steps":[{"id":"b","type":"agent","prompt":"b","after":["a"]},{"id":"a","type":"agent","prompt":"a","after":[]}]}`,
	} {
		root := t.TempDir()
		snapshot, err := runstore.Create(root, runstore.Input{WorkflowJSON: []byte(source), Task: "Проверить граф", CWD: root})
		if err != nil {
			t.Fatal(err)
		}
		view, err := (handler{root: root}).loadGraph(snapshot.Meta.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Nodes) != 2 || len(view.Edges) != 1 || view.Edges[0] != (graphEdge{"a", "b", ""}) {
			t.Fatalf("потеряна зависимость ещё не запущенного кубика: %+v", view)
		}
	}
}

// TestGraphKeepsRoutesAndVisits проверяет главный риск циклов: статическая
// схема содержит self-loop, а результаты и trace остаются у точных посещений.
func TestGraphKeepsRoutesAndVisits(t *testing.T) {
	root, snapshot := createAgentDashboardRun(t)
	view, err := (handler{root: root}).loadGraph(snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Nodes) != 2 || len(view.Executions) != 3 || len(view.Edges) != 1 || view.Edges[0] != (graphEdge{"loop", "loop", "again"}) {
		t.Fatalf("потеряны схема или посещения: %+v", view)
	}
	for i, entry := range view.Executions {
		if entry.Key != snapshot.Meta.Visits[i].VisitID || !strings.Contains(entry.TraceURL, "visit="+entry.Key) {
			t.Fatalf("неверная привязка истории: %+v", entry)
		}
		if entry.Result != "" {
			t.Fatal("старое сообщение ошибочно выдано за финальный отчёт")
		}
	}
}

// TestExecutionResultScopeAndCompletion защищает от ложного успеха: delta,
// другой turn, другой visit и отчёт ещё работающего агента не являются итогом.
func TestExecutionResultScopeAndCompletion(t *testing.T) {
	step := stepNode{StepID: "work", VisitID: "v2", turnID: "turn2", State: "succeeded"}
	event := runstore.RuntimeEvent{StepID: "work", VisitID: "v2", TurnID: "turn2", Kind: "item_completed", ItemType: "agentMessage", Content: "Итог: исправлена ошибка. Проверка прошла."}
	for _, change := range []string{"visit", "turn", "delta", "commentary", "running", "skipped"} {
		t.Run(change, func(t *testing.T) {
			current, candidate := step, event
			switch change {
			case "visit":
				candidate.VisitID = "v1"
			case "turn":
				candidate.TurnID = "turn1"
			case "delta":
				candidate.Kind = "agent_message_delta"
			case "commentary":
				candidate.Content = "Сейчас проверяю результат"
			default:
				current.State = change
			}
			if result, _ := executionResult(current, []runstore.RuntimeEvent{candidate}); result != "" {
				t.Fatalf("ложный отчёт: %q", result)
			}
		})
	}
	for _, state := range []string{"succeeded", "failed", "cancelled"} {
		step.State = state
		if result, _ := executionResult(step, []runstore.RuntimeEvent{event}); result != event.Content {
			t.Fatalf("потерян отчёт при %s: %q", state, result)
		}
	}
}

// TestGraphHTTPAndStoredReport проходит через настоящее хранилище и HTTP.
// HTML не требует PNG; вредоносный текст workflow не выходит из JSON-контекста.
func TestGraphHTTPAndStoredReport(t *testing.T) {
	root := t.TempDir()
	snapshot := createRun(t, root, "</script><script>alert(1)</script>", "")
	setState(t, root, snapshot, "succeeded")
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	err = run.AppendEvent(runstore.RuntimeEvent{StepID: "cube", Kind: "item_completed", ItemType: "agentMessage", Content: "Итог: готово. Проверено тестом."})
	if closeErr := run.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/graph/", "/api/graph/"} {
		recorder := httptest.NewRecorder()
		Handler(root).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, prefix+snapshot.Meta.RunID, nil))
		if recorder.Code != 200 || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("неверный HTTP: %d", recorder.Code)
		}
		if prefix == "/api/graph/" {
			var view graphView
			if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Executions[0].Result != "Итог: готово. Проверено тестом." {
				t.Fatalf("не прочитан durable отчёт: %+v", view)
			}
		} else if strings.Contains(recorder.Body.String(), "</script><script>alert(1)") || strings.Contains(recorder.Body.String(), "<img src=\"/uml/") {
			t.Fatal("HTML содержит небезопасный текст или старую картинку")
		}
	}
	recorder := httptest.NewRecorder()
	Handler(root).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/graph/missing", nil))
	if recorder.Code != 404 {
		t.Fatal("неизвестный run должен возвращать 404")
	}
}
