package dashboard

import (
	"encoding/json"
	"net/http"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
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
	for _, step := range snapshot.Workflow.Steps {
		view.Documents = append(view.Documents, sourceDocument{Name: "Инструкция · " + step.ID, Content: step.Prompt})
	}
	writeJSON(w, view)
}
