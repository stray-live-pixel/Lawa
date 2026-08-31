// Package workflow читает и проверяет схему Lawa без файловых и сетевых побочных
// эффектов. Общий валидатор используют validate, run и чтение сохранённого resume.
package workflow

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Speed — выбранный пользователем режим обслуживания запроса Codex. Отдельный
// тип не даёт coordinator перепутать продуктовые значения normal/fast с сырым
// serviceTier протокола, который может расширяться независимо от схемы Lawa.
type Speed string

const (
	SpeedNormal Speed = "normal"
	SpeedFast   Speed = "fast"
)

// Workflow описывает неизменяемый вход запуска. Порядок Steps не задаёт порядок
// выполнения: зависимости определяются только идентификаторами из DependsOn.
type Workflow struct {
	ID    string `json:"id"`
	Steps []Step `json:"steps"`
}

// Step — задача агента. ID является ключом графа, а не путём или ID чата Codex.
// Пустой, но явно заданный DependsOn разрешает старт без ожидания других задач.
// Model, Effort и Speed — указатели, потому что отсутствие поля означает
// наследование настройки Codex, а явно заданное значение должно попасть в
// неизменяемый снимок run и использоваться при каждом продолжении этого кубика.
type Step struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Prompt    string   `json:"prompt"`
	DependsOn []string `json:"dependsOn"`
	Model     *string  `json:"model,omitempty"`
	Effort    *string  `json:"effort,omitempty"`
	Speed     *Speed   `json:"speed,omitempty"`
}

// Decode читает ровно один JSON-объект и возвращает только целиком корректный граф.
// При ошибке результат пустой: вызывающий код не должен запускать часть схемы.
// Идентификаторы и промпты сохраняются без обрезки пробелов и других исправлений.
// json/v2 отклоняет повторные ключи и повреждённый Unicode, учитывает регистр
// имён. UnmarshalRead также требует EOF после объекта; неизвестные поля запрещаем
// явно. Проверка обязательных полей и связей остаётся в общем Validate.
func Decode(r io.Reader) (Workflow, error) {
	var w Workflow
	if err := json.UnmarshalRead(r, &w, json.RejectUnknownMembers(true)); err != nil {
		return Workflow{}, fmt.Errorf("workflow: %w", err)
	}
	if err := w.Validate(); err != nil {
		return Workflow{}, err
	}
	return w, nil
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
		if err := validateOptionalSetting(s.ID, "model", s.Model); err != nil {
			return err
		}
		if err := validateOptionalSetting(s.ID, "effort", s.Effort); err != nil {
			return err
		}
		if s.Speed != nil && *s.Speed != SpeedNormal && *s.Speed != SpeedFast {
			return fmt.Errorf("шаг %q: speed должен быть %q или %q", s.ID, SpeedNormal, SpeedFast)
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

// validateOptionalSetting проверяет только устойчивый контракт Lawa: значение
// присутствует, корректно закодировано и является одним токеном. Список моделей
// и допустимых для них effort меняется на стороне Codex, поэтому совместимость
// пары проверяет сам app-server без молчаливой подстановки другого значения.
func validateOptionalSetting(stepID, name string, value *string) error {
	if value == nil {
		return nil
	}
	if !utf8.ValidString(*value) || *value == "" || strings.IndexFunc(*value, unicode.IsSpace) >= 0 {
		return fmt.Errorf("шаг %q: %s должен быть непустым значением UTF-8 без пробельных символов", stepID, name)
	}
	return nil
}
