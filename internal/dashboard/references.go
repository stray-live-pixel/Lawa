package dashboard

import (
	"encoding/json/v2"
	"html/template"
	"net/url"
	"strings"
)

// ticketReference — краткая структурированная ссылка на рабочую задачу. Dashboard
// извлекает её из неизменяемого task.md, поэтому она работает и для уже созданных
// run без миграции meta.json. URL остаётся необязательным: ID всё равно полезен,
// если породивший workflow сохранил только идентификатор тикета.
type ticketReference struct {
	ID, Title string
	URL       template.URL
}

// ticketFromTask понимает явные поля, которые используют workflow интеграций с
// трекерами, и первые структурные поля между TRACKER_CONTEXT-маркерами.
// Произвольные ссылки из описания и комментариев намеренно не угадываются: макет,
// инструкция или ссылка на обсуждение не должны стать «тикетом» в dashboard.
func ticketFromTask(task string) ticketReference {
	var direct, context ticketReference
	inContext := false
	for _, line := range strings.Split(task, "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "TRACKER_CONTEXT_BEGIN":
			inContext = true
			continue
		case "TRACKER_CONTEXT_END":
			inContext = false
			continue
		}
		if key, value, ok := strings.Cut(trimmed, "="); ok {
			switch strings.TrimSpace(key) {
			case "ISSUE_ID", "TICKET_ID":
				setFirst(&direct.ID, cleanField(value))
			case "ISSUE_URL", "TICKET_URL":
				if direct.URL == "" {
					direct.URL = safeWebURL(cleanField(value))
				}
			case "TASK_TITLE", "ISSUE_TITLE", "TICKET_TITLE":
				setFirst(&direct.Title, cleanField(value))
			}
		}
		if !inContext {
			continue
		}
		key, value, ok := jsonStringField(trimmed)
		if !ok {
			continue
		}
		switch key {
		case "id":
			setFirst(&context.ID, value)
		case "url":
			if context.URL == "" {
				context.URL = safeWebURL(value)
			}
		case "summary", "title":
			setFirst(&context.Title, value)
		}
	}
	if direct.ID == "" {
		direct = context
	} else if context.ID == "" || context.ID == direct.ID {
		setFirst(&direct.Title, context.Title)
		if direct.URL == "" {
			direct.URL = context.URL
		}
	}
	// Название без ID может быть обычным заголовком постановки, а URL без ID —
	// любой вспомогательной ссылкой. Без идентификатора это не ссылка на тикет.
	if direct.ID == "" {
		return ticketReference{}
	}
	return direct
}

// jsonStringField разбирает только одну строку вида "key": "value". Полный
// task.md не является JSON, а вложенные свободные данные могут быть огромными;
// ограниченный разбор сохраняет предсказуемость и не интерпретирует их структуру.
func jsonStringField(line string) (string, string, bool) {
	if !strings.HasPrefix(line, `"`) {
		return "", "", false
	}
	separator := strings.Index(line, ":")
	if separator < 0 {
		return "", "", false
	}
	keyJSON := strings.TrimSpace(line[:separator])
	valueJSON := strings.TrimSuffix(strings.TrimSpace(line[separator+1:]), ",")
	var key, value string
	if err := json.Unmarshal([]byte(keyJSON), &key); err != nil {
		return "", "", false
	}
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil || value == "" {
		return "", "", false
	}
	return key, value, true
}

func cleanField(value string) string {
	return strings.Trim(strings.TrimSpace(value), `"'`)
}

func setFirst(target *string, value string) {
	if *target == "" && value != "" {
		*target = value
	}
}

// safeWebURL разрешает только обычную внешнюю HTTP(S)-ссылку без credentials.
// task.md принадлежит постановке и считается недоверенным отображаемым текстом;
// javascript:, file: и похожие схемы не должны становиться активной кнопкой.
func safeWebURL(raw string) template.URL {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return template.URL(parsed.String())
}
