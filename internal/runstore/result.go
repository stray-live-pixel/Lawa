//go:build darwin || linux

package runstore

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// resultForState вызывается только под LockedRun.mu. Итог публикуется атомарно
// с terminal state, после durable записи событий. Поэтому следующий Prepare
// никогда не увидит завершённый шаг без уже сохранённого свойства result.
// Нет нового маркера/turn — нет результата; старые попытки не наследуются.
func (r *LockedRun) resultForState(stepID, visitID, turnID string, state scheduler.State) (string, error) {
	if !resultState(state) || turnID == "" {
		return "", nil
	}
	events, err := readEventsFromDir(r.dir, r.runID, nil)
	if err != nil {
		return "", fmt.Errorf("прочитать результат исполнения: %w", err)
	}
	result := ""
	for _, event := range events {
		if event.StepID != stepID || event.VisitID != visitID || event.TurnID != turnID || event.Kind != "item_completed" || event.ItemType != "agentMessage" {
			continue
		}
		text := strings.TrimSpace(event.Content)
		if strings.HasPrefix(text, "Итог:") {
			result = text
		}
	}
	return result, nil
}

// resultState отделяет финальный ответ от промежуточного сообщения агента.
func resultState(state scheduler.State) bool {
	return state == scheduler.Succeeded || state == scheduler.Failed || state == scheduler.Cancelled
}

// validResult сохраняет совместимость со старыми metadata без result, но не
// позволяет выдать чужой/активный turn за завершённый отчёт. Лимит соответствует
// нормализованному приватному Content журнала (с запасом для UTF-8 и суффикса).
func validResult(result, turnID string, state scheduler.State) bool {
	return result == "" || resultState(state) && turnID != "" && utf8.ValidString(result) && len(result) <= 128000 && !strings.ContainsRune(result, 0)
}
