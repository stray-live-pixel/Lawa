package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
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

// TestChildResolutionSeparatesCWDFromInputRoots фиксирует новый контракт: cwd
// ребёнка может находиться вне workspace, но workflow и taskFile по-прежнему
// читаются только из workspace родителя или каталога его run.
func TestChildResolutionSeparatesCWDFromInputRoots(t *testing.T) {
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
	if err := syscall.Mkfifo(filepath.Join(workspace, "pipe.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(outside, "cwd-pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent, err := runstore.Create(root, runstore.Input{WorkflowJSON: workflowJSON, Task: "root", CWD: workspace})
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	manager := newChildRunManager(context.Background(), root, "codex", nil, nil, dependencies{
		check: func(_ context.Context, connection codex.Connection) error {
			checks++
			if connection.Directory == nil || connection.CWD != connection.Directory.Path() {
				return errors.New("preflight потерял проверенный каталог")
			}
			return nil
		},
	})
	valid := childRequest{Workflow: "child.json", CWD: outside, Task: "child", ParentRun: parent.Meta.RunID}
	resolved, err := manager.resolve(t.Context(), parent, valid)
	if err != nil {
		t.Fatalf("разрешённый child отклонён: %v", err)
	}
	t.Cleanup(func() { _ = resolved.directory.Close() })
	canonicalOutside, canonicalErr := filepath.EvalSymlinks(outside)
	if canonicalErr != nil || resolved.input.CWD != canonicalOutside || checks != 1 {
		t.Fatalf("внешний cwd или его preflight потерян: %+v, checks=%d", resolved.input, checks)
	}
	for _, test := range []struct {
		request childRequest
		want    string
	}{
		{childRequest{Workflow: "child.json", CWD: "relative", Task: "child", ParentRun: parent.Meta.RunID}, "абсолютным"},
		{childRequest{Workflow: "child.json", CWD: filepath.Join(outside, "missing"), Task: "child", ParentRun: parent.Meta.RunID}, "открыть cwd"},
		{childRequest{Workflow: "child.json", CWD: filepath.Join(outside, "outside.json"), Task: "child", ParentRun: parent.Meta.RunID}, "cwd должен быть папкой"},
		{childRequest{Workflow: "child.json", CWD: filepath.Join(outside, "cwd-pipe"), Task: "child", ParentRun: parent.Meta.RunID}, "cwd должен быть папкой"},
		{childRequest{Workflow: "escape.json", CWD: outside, Task: "child", ParentRun: parent.Meta.RunID}, "прочитать workflow"},
		{childRequest{Workflow: "pipe.json", CWD: outside, Task: "child", ParentRun: parent.Meta.RunID}, "обычным файлом"},
		{childRequest{Workflow: filepath.Join(outside, "outside.json"), CWD: outside, Task: "child", ParentRun: parent.Meta.RunID}, "файл должен находиться"},
	} {
		if child, err := manager.resolve(t.Context(), parent, test.request); err == nil || !strings.Contains(err.Error(), test.want) {
			_ = child.directory.Close()
			t.Fatalf("опасный путь не отклонён: %+v, %v", test.request, err)
		}
	}
}

// TestChildResolutionRejectsPolicyDeniedCWD подтверждает порядок операции:
// managed preflight выполняется до Create, а его понятная ошибка возвращается
// вызвавшему dynamic tool без зарегистрированного дочернего run.
func TestChildResolutionRejectsPolicyDeniedCWD(t *testing.T) {
	root, workspace, denied := filepath.Join(t.TempDir(), "runs"), t.TempDir(), t.TempDir()
	workflowJSON := []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"work","dependsOn":[]}]}`)
	if err := os.WriteFile(filepath.Join(workspace, "child.json"), workflowJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	parent, err := runstore.Create(root, runstore.Input{WorkflowJSON: workflowJSON, Task: "root", CWD: workspace})
	if err != nil {
		t.Fatal(err)
	}
	manager := newChildRunManager(t.Context(), root, "codex", nil, nil, dependencies{
		check: func(context.Context, codex.Connection) error {
			return errors.New("managed restriction запрещает каталог")
		},
	})
	request := childRequest{Workflow: "child.json", CWD: denied, Task: "child", ParentRun: parent.Meta.RunID}
	if _, err = manager.resolve(t.Context(), parent, request); err == nil || !strings.Contains(err.Error(), "cwd недоступен активной политике Codex") {
		t.Fatalf("отказ политики потерян: %v", err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("отказ preflight оставил дочерний run: %v, %v", entries, readErr)
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
