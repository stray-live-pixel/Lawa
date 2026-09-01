package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// TestDecodeChildRequests фиксирует batch-контракт отдельно от App Server:
// порядок сохраняется, неизвестные поля и пустая группа не принимаются.
func TestDecodeChildRequests(t *testing.T) {
	requests, err := decodeChildRequests("run_children", json.RawMessage(`{
  "children":[
    {"workflow":"one.json","cwd":".","task":"one","parentRun":"parent"},
    {"workflow":"two.json","cwd":"sub","taskFile":"task.md","parentRun":"parent"}
  ]
}`))
	if err != nil || len(requests) != 2 || requests[0].Workflow != "one.json" || requests[1].TaskFile != "task.md" {
		t.Fatalf("batch искажён: %+v, %v", requests, err)
	}
	for _, raw := range []string{`{"children":[]}`, `{"children":[],"unknown":true}`} {
		if _, err := decodeChildRequests("run_children", json.RawMessage(raw)); err == nil {
			t.Fatalf("принят неверный batch: %s", raw)
		}
	}
}

// TestChildResolutionDoesNotBroadenWorkspace проверяет границу прав до создания
// run. Абсолютный путь и симлинк наружу не должны превращать дочерний cwd или
// workflow в новый workspace, недоступный родительскому агенту.
func TestChildResolutionDoesNotBroadenWorkspace(t *testing.T) {
	root, workspace, outside := filepath.Join(t.TempDir(), "runs"), t.TempDir(), t.TempDir()
	workflowJSON := []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"work","dependsOn":[]}]}`)
	if err := os.WriteFile(filepath.Join(workspace, "child.json"), workflowJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside.json"), workflowJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.json"), filepath.Join(workspace, "escape.json")); err != nil {
		t.Fatal(err)
	}
	parent, err := runstore.Create(root, runstore.Input{WorkflowJSON: workflowJSON, Task: "root", CWD: workspace})
	if err != nil {
		t.Fatal(err)
	}
	manager := newChildRunManager(context.Background(), root, "codex", nil, nil, dependencies{})
	valid := childRequest{Workflow: "child.json", CWD: ".", Task: "child", ParentRun: parent.Meta.RunID}
	if _, err := manager.resolve(parent, valid); err != nil {
		t.Fatalf("разрешённый child отклонён: %v", err)
	}
	for _, request := range []childRequest{
		{Workflow: "child.json", CWD: outside, Task: "child", ParentRun: parent.Meta.RunID},
		{Workflow: "escape.json", CWD: ".", Task: "child", ParentRun: parent.Meta.RunID},
	} {
		if _, err := manager.resolve(parent, request); err == nil || !strings.Contains(err.Error(), "workspace") {
			t.Fatalf("выход из workspace не отклонён: %+v, %v", request, err)
		}
	}
}
