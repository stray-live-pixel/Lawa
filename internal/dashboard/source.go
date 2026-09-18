package dashboard

import (
	"encoding/json"
	"net/http"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// sourceView отдаётся только по запросу открытой вкладки. Prompt-файлы уже
// раскрыты при создании run: текущие файлы диска не подменяют исторический вход.
type sourceView struct {
	JSON      json.RawMessage
	Documents []sourceDocument
	Note      string
}
type sourceDocument struct{ Name, Content string }

// workflowSource принимает ID запуска, но никогда не путь к произвольному файлу.
// LoadForDashboard использует те же проверки runstore, что и основной dashboard.
func (h handler) workflowSource(w http.ResponseWriter, r *http.Request) {
	snapshot, err := runstore.LoadForDashboard(h.root, r.PathValue("run"))
	if err != nil {
		http.Error(w, "Не удалось прочитать исходники: "+diagnostic(err), http.StatusNotFound)
		return
	}
	view := sourceView{JSON: snapshot.WorkflowJSON,
		Note: "Сохранённый JSON запуска. Markdown-файлы и шаблоны раскрыты в prompt; исходные имена файлов отдельно не сохранены."}
	view.Documents = append(view.Documents, sourceDocument{Name: "task.md", Content: snapshot.Task})
	view.Documents = append(view.Documents, instructionDocuments(snapshot.Workflow)...)
	writeJSON(w, view)
}

// instructionDocuments одинаково показывает раскрытые prompt в run и определении.
func instructionDocuments(definition workflow.Workflow) []sourceDocument {
	documents := make([]sourceDocument, 0, len(definition.Steps))
	for _, step := range definition.Steps {
		documents = append(documents, sourceDocument{Name: "Инструкция · " + step.ID, Content: step.Prompt})
	}
	return documents
}
