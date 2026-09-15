package workflow

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTemplatesSourcesAndSnapshot проверяет обе версии и все сочетания источников:
// общая инструкция обновляется при новом чтении, но снимок остаётся автономным.
func TestTemplatesSourcesAndSnapshot(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, fileTemplate := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/file=%t", version, fileTemplate), func(t *testing.T) {
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, "templates"), 0o700); err != nil {
					t.Fatal(err)
				}
				text := "Отчёт {{lawa.workflow.id}}: {{lawa.step.id}}."
				template, _ := json.Marshal(text)
				templatePath := filepath.Join(dir, "templates", "report.md")
				if fileTemplate {
					template = []byte(`{"file":"templates/report.md"}`)
					if err := os.WriteFile(templatePath, []byte(text), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				prompt := "Задача. {{общий}} \\{{literal}}"
				promptJSON, _ := json.Marshal(prompt)
				if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte(prompt), 0o600); err != nil {
					t.Fatal(err)
				}
				prefix, edges := "", `"dependsOn":[]`
				if version == 2 {
					prefix, edges = `"version":2,"start":["implement","review"],`, `"after":[]`
				}
				source := []byte(fmt.Sprintf(`{%s"id":"flow","templates":{"report":%s,"общий":"{{report}}"},"steps":[{"id":"implement","type":"agent","prompt":%s,%s},{"id":"review","type":"agent","prompt":{"file":"review.md"},%s}]}`, prefix, template, promptJSON, edges, edges))
				sourcePath := filepath.Join(dir, "workflow.json")
				snapshot, w, err := ResolveSource(source, sourcePath, os.ReadFile)
				if err != nil {
					t.Fatal(err)
				}
				for _, step := range w.Steps {
					want := "Задача. Отчёт flow: " + step.ID + ". {{literal}}"
					if step.Prompt != want {
						t.Fatalf("%s: %q != %q", step.ID, step.Prompt, want)
					}
				}
				if bytes.Contains(snapshot, []byte(`"templates"`)) || bytes.Contains(snapshot, []byte(`"file"`)) {
					t.Fatalf("снимок хранит ссылки: %s", snapshot)
				}
				// Контекст отличается только ID: одинаковый ID в двух отдельных схемах
				// доказывает эквивалентность JSON- и Markdown-prompt без нормализации текста.
				inlineSource := bytes.Replace(source, []byte(`{"file":"review.md"}`), promptJSON, 1)
				_, inline, err := ResolveSource(inlineSource, sourcePath, os.ReadFile)
				if err != nil || inline.Steps[1].Prompt != w.Steps[1].Prompt {
					t.Fatalf("формы prompt различаются: %+v, %v", inline, err)
				}
				if fileTemplate {
					if err := os.WriteFile(templatePath, []byte("Новый {{lawa.step.id}}"), 0o600); err != nil {
						t.Fatal(err)
					}
					_, next, err := ResolveSource(source, sourcePath, os.ReadFile)
					if err != nil {
						t.Fatal(err)
					}
					for _, step := range next.Steps {
						if step.Prompt != "Задача. Новый "+step.ID+" {{literal}}" {
							t.Fatalf("не обновлён шаблон: %+v", step)
						}
					}
					if err := os.Remove(templatePath); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(filepath.Join(dir, "review.md")); err != nil {
					t.Fatal(err)
				}
				loaded, err := Decode(bytes.NewReader(snapshot))
				if err != nil || loaded.Steps[0].Prompt != w.Steps[0].Prompt {
					t.Fatalf("снимок изменён: %+v, %v", loaded, err)
				}
			})
		}
	}
}

// TestTemplateContext фиксирует наследование модели и пустые значения настроек,
// которые Codex выбирает позднее. Значения ID не интерпретируются повторно.
func TestTemplateContext(t *testing.T) {
	source := []byte(`{"id":"{{literal}}","model":"shared","steps":[{"id":"one","type":"agent","dependsOn":[],"prompt":"{{lawa.workflow.id}}/{{lawa.workflow.version}}/{{lawa.workflow.model}}/{{lawa.step.id}}/{{lawa.step.type}}/{{lawa.step.model}}/{{lawa.step.effort}}/{{lawa.step.speed}}"},{"id":"two","type":"agent","dependsOn":[],"model":"override","effort":"high","speed":"fast","prompt":"{{lawa.step.model}}/{{lawa.step.effort}}/{{lawa.step.speed}}"}]}`)
	_, w, err := ResolveSource(source, "workflow.json", os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if w.Steps[0].Prompt != "{{literal}}/1/shared/one/agent/shared//" || w.Steps[1].Prompt != "override/high/fast" {
		t.Fatalf("неверный контекст: %+v", w.Steps)
	}
	absent := templateContext(Workflow{}, Step{})
	if absent["lawa.workflow.model"] != "" || absent["lawa.step.model"] != "" {
		t.Fatalf("придуманы настройки: %v", absent)
	}
}

// TestTemplateErrors требует предметную ошибку до создания снимка даже для
// неиспользуемого шаблона. Неверный реестр не должен исчезать при нормализации.
func TestTemplateErrors(t *testing.T) {
	cases := []struct{ name, registry, prompt, want string }{
		{"null", `null`, `"text"`, "templates"},
		{"array", `[]`, `"text"`, "templates"},
		{"reserved", `{"lawa.step.id":"bad"}`, `"text"`, "имя"},
		{"empty name", `{"":"bad"}`, `"text"`, "имя"},
		{"space", `{"bad name":"bad"}`, `"text"`, "имя"},
		{"empty", `{"x":" "}`, `"text"`, "непустой"},
		{"null entry", `{"x":null}`, `"text"`, "templates.x"},
		{"extra", `{"x":{"file":"templates/a.md","text":"bad"}}`, `"text"`, "templates.x"},
		{"absolute", `{"x":{"file":"/templates/a.md"}}`, `"text"`, "templates.x.file"},
		{"outside", `{"x":{"file":"templates/../a.md"}}`, `"text"`, "templates.x.file"},
		{"wrong folder", `{"x":{"file":"a.md"}}`, `"text"`, "templates.x.file"},
		{"wrong extension", `{"x":{"file":"templates/a.txt"}}`, `"text"`, "templates.x.file"},
		{"missing file", `{"x":{"file":"templates/missing.md"}}`, `"text"`, "templates.x.file"},
		{"duplicate", `{"x":"a","x":"b"}`, `"text"`, "duplicate"},
		{"unknown", `{}`, `"{{missing}}"`, "missing"},
		{"unknown context", `{}`, `"{{lawa.run.id}}"`, "lawa.run.id"},
		{"unused unknown", `{"x":"{{missing}}"}`, `"text"`, "missing"},
		{"self cycle", `{"x":"{{x}}"}`, `"text"`, "цикл"},
		{"cycle", `{"x":"{{y}}","y":"{{x}}"}`, `"text"`, "цикл"},
		{"unclosed", `{}`, `"{{x"`, "незакрытая"},
		{"empty result", `{}`, `"{{lawa.step.model}}"`, "непустой prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := []byte(fmt.Sprintf(`{"id":"flow","templates":%s,"steps":[{"id":"s","type":"agent","dependsOn":[],"prompt":%s}]}`, tc.registry, tc.prompt))
			snapshot, w, err := ResolveSource(source, filepath.Join(t.TempDir(), "workflow.json"), os.ReadFile)
			if err == nil || !strings.Contains(err.Error(), tc.want) || snapshot != nil || len(w.Steps) != 0 {
				t.Fatalf("неверная ошибка/частичный результат: %s, %+v, %v", snapshot, w, err)
			}
		})
	}
	for _, content := range [][]byte{{0xff}, []byte(" \n")} {
		_, err := readTemplates([]byte(`{"x":{"file":"templates/a.md"}}`), "workflow.json", func(string) ([]byte, error) { return content, nil })
		if err == nil {
			t.Fatalf("принят неверный UTF-8/пустой текст: %q", content)
		}
	}
}

// TestTemplateBoundsAndEscapes защищает от разрастания вложенных шаблонов и
// повторного раскрытия литералов. Обычные длинные prompt остаются совместимыми.
func TestTemplateBoundsAndEscapes(t *testing.T) {
	templates := map[string]string{"literal": `\{{unknown}}`, "pair": "{{literal}}{{literal}}"}
	got, err := expandPrompt(`{{ pair }} \{{lawa.step.id}}`, templates, nil)
	if err != nil || got != "{{unknown}}{{unknown}} {{lawa.step.id}}" {
		t.Fatalf("экранирование: %q, %v", got, err)
	}
	large := strings.Repeat("x", (1<<20)+1)
	if got, err := expandPrompt(large, nil, nil); err != nil || got != large {
		t.Fatal("изменён старый длинный prompt")
	}
	if _, err := expandPrompt("{{large}}", map[string]string{"large": large}, nil); err == nil {
		t.Fatal("нет лимита размера")
	}
	growing := map[string]string{"t00": "x"}
	for i := 1; i <= 21; i++ {
		growing[fmt.Sprintf("t%02d", i)] = strings.Repeat(fmt.Sprintf("{{t%02d}}", i-1), 2)
	}
	if _, err := expandPrompt("{{t21}}", growing, nil); err == nil {
		t.Fatal("нет защиты от экспоненциального роста")
	}
	// Предварительно закешированные короткие цепочки тоже не отменяют лимит.
	reverse := map[string]string{"t00": "end"}
	for i := 1; i <= 65; i++ {
		reverse[fmt.Sprintf("t%02d", i)] = fmt.Sprintf("{{t%02d}}", i-1)
	}
	if _, err := expandPrompt("{{t65}}", reverse, nil); err == nil {
		t.Fatal("кеш позволил обойти лимит глубины")
	}
	deep := map[string]string{"t65": "end"}
	for i := 0; i < 65; i++ {
		deep[fmt.Sprintf("t%02d", i)] = fmt.Sprintf("{{t%02d}}", i+1)
	}
	if _, err := expandPrompt("{{t00}}", deep, nil); err == nil {
		t.Fatal("нет лимита глубины")
	}
}
