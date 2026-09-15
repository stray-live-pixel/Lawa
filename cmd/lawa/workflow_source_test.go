package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLocalMarkdownAbsoluteSymlink сохраняет прежнюю файловую семантику CLI:
// ограничения os.Root дочерних запросов не должны запрещать локальные симлинки.
func TestLocalMarkdownAbsoluteSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, []byte("# Инструкция\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "step.md")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workflow.json")
	if err := os.WriteFile(path, []byte(`{"id":"local","steps":[{"id":"one","type":"agent","prompt":{"file":"step.md"},"dependsOn":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, definition, err := loadWorkflowSource(path)
	if err != nil || definition.Steps[0].Prompt != "# Инструкция\n" {
		t.Fatalf("локальная ссылка отклонена: %+v, %v", definition, err)
	}
}
