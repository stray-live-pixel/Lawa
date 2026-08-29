package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI проверяет справку без Codex, аргументы и валидацию настоящих файлов.
// Ошибочный ввод не должен печатать сообщение об успешной проверке.
func TestCLI(t *testing.T) {
	invalid := filepath.Join(t.TempDir(), "invalid.json")
	content := []byte(`{"id":"broken","steps":[]}`)
	if err := os.WriteFile(invalid, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"без аргументов", nil, "Команды:"},
		{"справка", []string{"help"}, "Команды:"},
		{"короткая справка", []string{"-h"}, "Команды:"},
		{"длинная справка", []string{"--help"}, "Команды:"},
		{"инструкция скилла", []string{"skill"}, "# Lawa: запуск workflow из чата Codex"},
		{"пример", []string{"validate", "../../examples/review.json"}, `Workflow "review" корректен; шагов: 4.`},
		{"неверный граф", []string{"validate", invalid}, ""},
		{"нет файла", []string{"validate", invalid + ".missing"}, ""},
		{"папка вместо файла", []string{"validate", filepath.Dir(invalid)}, ""},
		{"нет пути", []string{"validate"}, ""},
		{"лишний аргумент", []string{"validate", invalid, "extra"}, ""},
		{"несуществующая команда", []string{"unknown"}, ""},
		{"run не маскируется проверкой", []string{"run", invalid}, ""},
		{"лишний аргумент скилла", []string{"skill", "extra"}, ""},
		{"лишний аргумент справки", []string{"help", "extra"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := execute(tc.args, &out)
			if tc.want == "" {
				if err == nil || out.Len() != 0 {
					t.Fatalf("ожидалась только ошибка: %v, %q", err, out.String())
				}
			} else if err != nil || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("неожиданный результат: %v, %q", err, out.String())
			}
		})
	}
	if after, err := os.ReadFile(invalid); err != nil || !bytes.Equal(after, content) {
		t.Fatalf("проверка изменила исходный файл: %v", err)
	}
}

// failingWriter позволяет проверить ошибку вывода без реального закрытого канала.
type failingWriter struct{ err error }

// Write имитирует отказ приёмника до записи первого байта.
func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// TestOutputError не позволяет объявить успех, когда результат не удалось вывести.
func TestOutputError(t *testing.T) {
	failure := errors.New("ошибка вывода")
	for _, args := range [][]string{{"help"}, {"skill"}, {"validate", "../../examples/review.json"}} {
		if err := execute(args, failingWriter{failure}); !errors.Is(err, failure) {
			t.Fatalf("ошибка вывода потеряна: %v", err)
		}
	}
}

// TestSkillInstruction фиксирует обязательные части пользовательского сценария.
// Это защищает инструкцию от незаметного превращения в общий обзор: она должна
// оставаться пригодной для запуска, наблюдения и безопасного resume из одного чата.
func TestSkillInstruction(t *testing.T) {
	for _, fragment := range []string{
		"явной просьбы пользователя",
		"lawa validate <workflow.json>",
		"lawa run <workflow.json> --cwd <абсолютный-путь-проекта>",
		"lawa resume <run-id>",
		"финальную постановку, комментарий пользователя и ID",
		"memory/<threadId>.md",
		"изменять только собственный",
	} {
		if !strings.Contains(skillInstruction, fragment) {
			t.Errorf("в инструкции отсутствует обязательный фрагмент %q", fragment)
		}
	}
}
