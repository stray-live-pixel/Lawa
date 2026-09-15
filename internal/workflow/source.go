package workflow

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ResolveSource превращает пользовательский JSON со ссылками prompt.file в
// автономный снимок с текстовыми prompt и проверяет весь граф через Decode.
// Путь ссылки считается от каталога sourcePath, а не от cwd исполнителя.
// readFile задаёт файловые полномочия вызывающего кода: дочерний запуск обязан
// передать чтение через разрешённые os.Root, обычный CLI — локальное чтение.
// Функция не пишет файлы. При любой ошибке частичный снимок не возвращается.
// Уже сохранённые run читаются только через Decode и никогда не раскрывают ссылки.
func ResolveSource(data []byte, sourcePath string, readFile func(string) ([]byte, error)) ([]byte, Workflow, error) {
	var document map[string]jsontext.Value
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, Workflow{}, fmt.Errorf("workflow: %w", err)
	}
	var steps []map[string]jsontext.Value
	if raw, ok := document["steps"]; ok {
		if err := json.Unmarshal(raw, &steps); err != nil {
			return nil, Workflow{}, fmt.Errorf("workflow steps: %w", err)
		}
	}
	changed := false
	for index, step := range steps {
		raw := bytes.TrimSpace(step["prompt"])
		if len(raw) == 0 || raw[0] != '{' {
			continue // Строки и ошибочные скалярные значения проверит общий Decode.
		}
		var reference struct {
			File string `json:"file"`
		}
		if err := json.Unmarshal(raw, &reference, json.RejectUnknownMembers(true)); err != nil {
			return nil, Workflow{}, fmt.Errorf("steps[%d].prompt: %w", index, err)
		}
		if strings.TrimSpace(reference.File) == "" || !strings.EqualFold(filepath.Ext(reference.File), ".md") {
			return nil, Workflow{}, fmt.Errorf("steps[%d].prompt.file: нужен путь к .md файлу", index)
		}
		path := reference.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(sourcePath), path)
		}
		content, err := readFile(path)
		if err != nil {
			return nil, Workflow{}, fmt.Errorf("steps[%d].prompt.file %q: %w", index, reference.File, err)
		}
		if !utf8.Valid(content) || strings.TrimSpace(string(content)) == "" {
			return nil, Workflow{}, fmt.Errorf("steps[%d].prompt.file %q: нужен непустой текст UTF-8", index, reference.File)
		}
		// Markdown передаётся дословно: заголовки, пробелы и переносы являются
		// частью инструкции. JSON-кодирование лишь экранирует строку снимка.
		step["prompt"], err = json.Marshal(string(content))
		if err != nil {
			return nil, Workflow{}, err
		}
		changed = true
	}
	if changed {
		var err error
		document["steps"], err = json.Marshal(steps)
		if err != nil {
			return nil, Workflow{}, err
		}
		data, err = json.Marshal(document)
		if err != nil {
			return nil, Workflow{}, err
		}
	}
	// Для старого строкового формата сохраняем исходные байты. Нормализация
	// не ослабляет строгую схему: неизвестные поля, связи и типы проверяются здесь.
	definition, err := Decode(bytes.NewReader(data))
	if err != nil {
		return nil, Workflow{}, err
	}
	return data, definition, nil
}
