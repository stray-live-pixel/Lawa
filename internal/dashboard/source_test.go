package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// Источник берётся из сохранённого run без чтения рабочего каталога. Markdown
// сохраняется строкой, а не вставляется в HTML; неизвестный ID не раскрывает файл.
func TestWorkflowSource(t *testing.T) {
	root := t.TempDir()
	input := `{"id":"demo","steps":[{"id":"a","type":"agent","prompt":"# Saved instruction","dependsOn":[]}]}`
	snapshot, err := runstore.Create(root, runstore.Input{WorkflowJSON: []byte(input), Task: "task", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	Handler(root).ServeHTTP(recorder, httptest.NewRequest("GET", "/api/source/"+snapshot.Meta.RunID, nil))
	var view sourceView
	if recorder.Code != 200 {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if string(view.JSON) != input || len(view.Documents) != 2 || view.Documents[1].Content != "# Saved instruction" {
		t.Fatalf("неверный снимок: %+v", view)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("исходники кешируются")
	}
	recorder = httptest.NewRecorder()
	Handler(root).ServeHTTP(recorder, httptest.NewRequest("GET", "/api/source/missing", nil))
	if recorder.Code != 404 {
		t.Fatal(recorder.Code)
	}
}
