package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/flows-2/internal/workflow"
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
	// Отдельный файл является исходником инструкции, а команда должна печатать его
	// побайтно. Проверка защищает CLI от случайной обработки, обрезки или добавления
	// служебного текста вокруг готового SKILL.md.
	want, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := execute([]string{"skill"}, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Error("lawa skill изменяет содержимое встроенного SKILL.md")
	}

	// Метаданные идут первыми, чтобы stdout можно было без ручной обработки
	// сохранить в SKILL.md. Точное сравнение не пропустит текст перед frontmatter
	// или потерю обязательных полей, из-за которых Codex не распознает скилл.
	const metadata = "---\nname: lawa\ndescription: \"Запуск и продолжение workflow Lawa из чата Codex.\"\n---\n\n"
	if !strings.HasPrefix(skillInstruction, metadata) {
		t.Errorf("инструкция не начинается с обязательных метаданных SKILL.md: %q", skillInstruction)
	}
	// Пример проверяет тот же production-валидатор, что и команда validate. Так
	// документация не сможет предлагать неизвестные поля, потерянные зависимости
	// или цикл после будущего изменения контракта workflow.
	const exampleStart, exampleEnd = "~~~json\n", "\n~~~"
	example := strings.SplitN(skillInstruction, exampleStart, 2)
	if len(example) != 2 {
		t.Fatal("в инструкции отсутствует JSON-пример workflow")
	}
	example = strings.SplitN(example[1], exampleEnd, 2)
	if len(example) != 2 {
		t.Fatal("JSON-пример workflow не завершён")
	}
	if _, err := workflow.Decode(strings.NewReader(example[0])); err != nil {
		t.Errorf("инструкция содержит невалидный пример workflow: %v", err)
	}
	for _, fragment := range []string{
		"есть только бинарник lawa",
		"явной просьбы пользователя",
		"lawa validate <workflow.json>",
		`"dependsOn": ["architecture", "security"]`,
		"Не добавляй неизвестные поля",
		"прямые и\nкосвенные циклы",
		"lawa run <workflow.json> --cwd <абсолютный-путь-проекта>",
		"lawa resume <run-id>",
		"финальную постановку, комментарий пользователя и ID",
		"memory/<threadId>.md",
		"изменять только собственный",
		"https://github.com/stray-live-pixel/flows-2",
		"https://raw.githubusercontent.com/stray-live-pixel/flows-2/main/product/1.md",
		"Если версия\nизвестна из источника установки",
		"Если нет — не угадывай",
		"его SHA-256 и полный вывод lawa help",
		"получи явное разрешение перед публичной",
		"https://github.com/stray-live-pixel/flows-2/issues/new",
	} {
		if !strings.Contains(skillInstruction, fragment) {
			t.Errorf("в инструкции отсутствует обязательный фрагмент %q", fragment)
		}
	}
}
