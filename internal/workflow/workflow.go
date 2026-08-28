// Package workflow читает и проверяет схему Lawa без файловых и сетевых побочных
// эффектов. Общий валидатор предназначен также для будущих команд run и resume.
package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Workflow описывает неизменяемый вход запуска. Порядок Steps не задаёт порядок
// выполнения: зависимости определяются только идентификаторами из DependsOn.
type Workflow struct {
	ID    string `json:"id"`
	Steps []Step `json:"steps"`
}

// Step — задача агента. ID является ключом графа, а не путём или ID чата Codex.
// Пустой, но явно заданный DependsOn разрешает старт без ожидания других задач.
type Step struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Prompt    string   `json:"prompt"`
	DependsOn []string `json:"dependsOn"`
}

// Decode читает ровно один JSON-объект и возвращает только целиком корректный граф.
// При ошибке результат пустой: вызывающий код не должен запускать часть схемы.
// Идентификаторы и промпты сохраняются без обрезки пробелов и других исправлений.
func Decode(r io.Reader) (Workflow, error) {
	var w Workflow
	var steps []json.RawMessage
	if err := decodeObject(r, map[string]any{"id": &w.ID, "steps": &steps}); err != nil {
		return Workflow{}, fmt.Errorf("workflow: %w", err)
	}
	for i, raw := range steps {
		var step Step
		fields := map[string]any{
			"id": &step.ID, "type": &step.Type,
			"prompt": &step.Prompt, "dependsOn": &step.DependsOn,
		}
		if err := decodeObject(bytes.NewReader(raw), fields); err != nil {
			return Workflow{}, fmt.Errorf("steps[%d]: %w", i, err)
		}
		w.Steps = append(w.Steps, step)
	}
	if err := w.Validate(); err != nil {
		return Workflow{}, err
	}
	return w, nil
}

// decodeObject принимает только перечисленные поля с точным регистром имён.
// Обычный Unmarshal допускает повторные ключи и нечувствителен к регистру полей
// структуры. Здесь неоднозначный вход отклоняется, чтобы проверка и исполнение
// не могли по-разному трактовать один workflow. Поток после объекта должен закончиться.
func decodeObject(r io.Reader, fields map[string]any) error {
	d := json.NewDecoder(r)
	opening, err := d.Token()
	if err != nil {
		return err
	}
	if opening != json.Delim('{') {
		return fmt.Errorf("ожидался JSON-объект")
	}
	seen := make(map[string]bool)
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return fmt.Errorf("ожидалось имя поля")
		}
		target, known := fields[name]
		if !known {
			return fmt.Errorf("неизвестное поле %q", name)
		}
		if seen[name] {
			return fmt.Errorf("повторное поле %q", name)
		}
		seen[name] = true
		if err := d.Decode(target); err != nil {
			return fmt.Errorf("поле %q: %w", name, err)
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := d.Decode(&extra); err != io.EOF {
		if err != nil {
			return fmt.Errorf("прочитать конец JSON: %w", err)
		}
		return fmt.Errorf("после объекта ожидался конец JSON")
	}
	return nil
}

// Validate проверяет поля, ссылки и ацикличность, не изменяя исходный граф.
// Nil и [] у DependsOn различаются: только явный массив соответствует схеме.
// Повторные зависимости отклоняются как неоднозначная запись одного ребра.
func (w Workflow) Validate() error {
	if strings.TrimSpace(w.ID) == "" || len(w.Steps) == 0 {
		return fmt.Errorf("нужны непустой id workflow и непустой массив steps")
	}
	indices := make(map[string]int, len(w.Steps))
	for i, s := range w.Steps {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("steps[%d]: нужен непустой id", i)
		}
		if _, exists := indices[s.ID]; exists {
			return fmt.Errorf("повторный id шага %q", s.ID)
		}
		indices[s.ID] = i
		if s.Type != "agent" || strings.TrimSpace(s.Prompt) == "" || s.DependsOn == nil {
			return fmt.Errorf("шаг %q: нужны type=agent, непустой prompt и массив dependsOn", s.ID)
		}
	}
	remaining := make([]int, len(w.Steps))
	followers := make([][]int, len(w.Steps))
	for i, s := range w.Steps {
		seen := make(map[string]bool)
		for _, dependency := range s.DependsOn {
			parent, exists := indices[dependency]
			if !exists {
				return fmt.Errorf("шаг %q: неизвестная зависимость %q", s.ID, dependency)
			}
			if seen[dependency] {
				return fmt.Errorf("шаг %q: повторная зависимость %q", s.ID, dependency)
			}
			seen[dependency] = true
			remaining[i]++
			followers[parent] = append(followers[parent], i)
		}
	}
	// Алгоритм Кана обрабатывает каждую вершину и ребро один раз: O(V + E).
	// Очередь содержит только вершины без оставшихся входящих рёбер. Если после
	// обхода остались вершины, есть цикл. Рекурсии нет даже у очень длинной цепочки.
	var queue []int
	for i, count := range remaining {
		if count == 0 {
			queue = append(queue, i)
		}
	}
	for head := 0; head < len(queue); head++ {
		for _, next := range followers[queue[head]] {
			remaining[next]--
			if remaining[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(queue) != len(w.Steps) {
		return fmt.Errorf("обнаружен цикл зависимостей")
	}
	return nil
}
