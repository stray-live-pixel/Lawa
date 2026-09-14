package workflow

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveSourceSnapshot проверяет обе версии, смешанный ввод и автономность:
// изменение исходного Markdown не должно менять инструкции сохранённого run.
func TestResolveSourceSnapshot(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "check.md")
			text := "# Проверка\n\n- Прочитай код.\n- Сохрани выводы.\n"
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			prefix, edges := "", `"dependsOn":[]`
			if version == 2 {
				prefix, edges = `"version":2,"start":["check","inline"],`, `"after":[]`
			}
			source := []byte(fmt.Sprintf(`{%s"id":"md","steps":[{"id":"check","type":"agent","prompt":{"file":"check.md"},%s},{"id":"inline","type":"agent","prompt":"check.md",%s}]}`, prefix, edges, edges))
			snapshot, definition, err := ResolveSource(source, filepath.Join(dir, "workflow.json"), os.ReadFile)
			if err != nil {
				t.Fatal(err)
			}
			if definition.Steps[0].Prompt != text || definition.Steps[1].Prompt != "check.md" {
				t.Fatalf("инструкции искажены: %+v", definition.Steps)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			loaded, err := Decode(bytes.NewReader(snapshot))
			if err != nil || loaded.Steps[0].Prompt != text {
				t.Fatalf("снимок зависит от файла: %+v, %v", loaded, err)
			}
		})
	}
}

// TestResolveSourceErrors не позволяет принять опечатку за инструкцию или
// ослабить строгую JSON-схему при раскрытии ссылок.
func TestResolveSourceErrors(t *testing.T) {
	for _, prompt := range []string{`{}`, `{"file":null}`, `{"file":""}`, `{"file":"check.txt"}`, `{"file":42}`, `{"file":"check.md","text":"extra"}`, `{"file":"check.md","file":"other.md"}`, `[]`, `true`} {
		source := []byte(fmt.Sprintf(`{"id":"md","steps":[{"id":"check","type":"agent","prompt":%s,"dependsOn":[]}]}`, prompt))
		if _, _, err := ResolveSource(source, "workflow.json", func(string) ([]byte, error) { return []byte("text"), nil }); err == nil {
			t.Errorf("принят prompt %s", prompt)
		}
	}
	for _, content := range [][]byte{nil, []byte(" \n\t"), {0xff}} {
		source := []byte(`{"id":"md","steps":[{"id":"check","type":"agent","prompt":{"file":"check.md"},"dependsOn":[]}]}`)
		if _, _, err := ResolveSource(source, "workflow.json", func(string) ([]byte, error) { return content, nil }); err == nil {
			t.Errorf("принят контент %q", content)
		}
	}
	for _, extra := range []string{`"unknown":1,`, `"id":"duplicate",`} {
		source := []byte(`{` + extra + `"id":"md","steps":[{"id":"check","type":"agent","prompt":{"file":"check.md"},"dependsOn":[]}]}`)
		if _, _, err := ResolveSource(source, "workflow.json", func(string) ([]byte, error) { return []byte("text"), nil }); err == nil {
			t.Errorf("принято неверное поле %s", extra)
		}
	}
	source := []byte(`{"id":"md","steps":[{"id":"check","type":"agent","prompt":{"file":"missing.md"},"dependsOn":[]}]}`)
	if _, _, err := ResolveSource(source, "workflow.json", os.ReadFile); err == nil || !strings.Contains(err.Error(), "steps[0].prompt.file") {
		t.Fatalf("нет диагностики ссылки: %v", err)
	}
}

// TestResolveSourceInlineUnchanged сохраняет байтовую совместимость старых входов.
func TestResolveSourceInlineUnchanged(t *testing.T) {
	source := []byte("{\n\"id\":\"old\",\"steps\":[{\"id\":\"one\",\"type\":\"agent\",\"prompt\":\"text\",\"dependsOn\":[]}]}")
	got, _, err := ResolveSource(source, "workflow.json", func(string) ([]byte, error) {
		t.Fatal("неожиданное чтение файла")
		return nil, nil
	})
	if err != nil || !bytes.Equal(got, source) {
		t.Fatalf("изменён старый вход: %s, %v", got, err)
	}
}
