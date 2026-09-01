package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	if err := os.Mkdir(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(workspace, "pipe.json"), 0o600); err != nil {
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
	for _, test := range []struct {
		request childRequest
		want    string
	}{
		{childRequest{Workflow: "child.json", CWD: outside, Task: "child", ParentRun: parent.Meta.RunID}, "совпадать с workspace"},
		{childRequest{Workflow: "child.json", CWD: "nested", Task: "child", ParentRun: parent.Meta.RunID}, "совпадать с workspace"},
		{childRequest{Workflow: "escape.json", CWD: ".", Task: "child", ParentRun: parent.Meta.RunID}, "прочитать workflow"},
		{childRequest{Workflow: "pipe.json", CWD: ".", Task: "child", ParentRun: parent.Meta.RunID}, "обычным файлом"},
		{childRequest{Workflow: filepath.Join(outside, "outside.json"), CWD: ".", Task: "child", ParentRun: parent.Meta.RunID}, "файл должен находиться"},
	} {
		if _, err := manager.resolve(parent, test.request); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("опасный путь не отклонён: %+v, %v", test.request, err)
		}
	}
}

// TestReadAllowedFileRejectsDirectoryReplacement воспроизводит существенный
// порядок гонки: Lawa уже открыла доверенный workspace, после чего агент заменил
// вложенную папку симлинком наружу. os.Root обязан сохранить границу и не прочитать
// секрет; переименованный безопасный файл внутри того же root остаётся доступен.
func TestReadAllowedFileRejectsDirectoryReplacement(t *testing.T) {
	workspace, runDir, outside := t.TempDir(), t.TempDir(), t.TempDir()
	slot, saved := filepath.Join(workspace, "slot"), filepath.Join(workspace, "saved")
	if err := os.Mkdir(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, "task.md"), []byte("безопасная задача"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "task.md"), []byte("секрет снаружи"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspaceRoot.Close() })
	runRoot, err := os.OpenRoot(runDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runRoot.Close() })

	if err = os.Rename(slot, saved); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, slot); err != nil {
		t.Fatal(err)
	}
	if data, readErr := readAllowedFile(workspaceRoot, runRoot, workspace, runDir, "slot/task.md"); readErr == nil {
		t.Fatalf("после подмены прочитан внешний файл: %q", data)
	}
	data, err := readAllowedFile(workspaceRoot, runRoot, workspace, runDir, "saved/task.md")
	if err != nil || string(data) != "безопасная задача" {
		t.Fatalf("безопасный файл внутри workspace не прочитан: %q, %v", data, err)
	}
}
