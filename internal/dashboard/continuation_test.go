package dashboard

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestContinuationScopes защищает от подмены выбранного прохода последним и
// от ошибочного использования codexThreadId вместо ID файла памяти.
func TestContinuationScopes(t *testing.T) {
	root, snapshot := createAgentDashboardRun(t)
	view, err := (handler{root: root}).loadGraph(snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range view.Executions {
		visit := snapshot.Meta.Visits[i]
		for _, expected := range []string{snapshot.Meta.CWD, snapshot.Meta.RunID,
			visit.VisitID, visit.CodexThreadID, visit.TurnID,
			filepath.Join(root, snapshot.Meta.RunID, "memory", visit.VisitID+".md"),
			filepath.Join(root, snapshot.Meta.RunID, "events.jsonl")} {
			if !strings.Contains(entry.Prompt, expected) {
				t.Fatalf("промпт %s потерял %q", entry.Key, expected)
			}
		}
	}
	if strings.Contains(view.Executions[0].Prompt, snapshot.Meta.Visits[2].VisitID) {
		t.Fatal("первый проход получил контекст второго")
	}
	if !strings.Contains(view.Prompt, "весь workflow") || strings.Contains(view.Prompt, "- область: кубик") {
		t.Fatal("промпт workflow ограничен одним исполнением")
	}
}

// TestContinuationLegacyAndUnstarted проверяет контекст без сессии и без
// посещения; отсутствие Codex ID не мешает передать постановку нового кубика.
func TestContinuationLegacyAndUnstarted(t *testing.T) {
	root := t.TempDir()
	snapshot := createRun(t, root, "Контекст", "")
	view, err := (handler{root: root}).loadGraph(snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Executions[0].Prompt, filepath.Join(root, snapshot.Meta.RunID, "memory", snapshot.Meta.Steps[0].ThreadID+".md")) {
		t.Fatal("legacy промпт потерял файл памяти")
	}
	prompt := continuationPrompt(root, snapshot, "future", "")
	for _, expected := range []string{`stepId="future"`, `codexThreadId: ""`, "task.md", "workflow.json", "meta.json", "Моя дополнительная задача:"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("незапущенный кубик потерял %q", expected)
		}
	}
}
