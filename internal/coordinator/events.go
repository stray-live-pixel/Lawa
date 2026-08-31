package coordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// appendProcessEvent переводит жизненный цикл дочернего App Server в устойчивый
// журнал Lawa. Командная строка, env и stderr намеренно не копируются.
func appendProcessEvent(run *runstore.LockedRun, stepID, threadID, turnID string, process codex.ProcessEvent) error {
	kind := "process_" + process.Kind
	return run.AppendEvent(runstore.RuntimeEvent{
		Time: process.Time, StepID: stepID, ThreadID: threadID, TurnID: turnID,
		Kind: kind, PID: process.PID, ExitCode: process.ExitCode, Signal: process.Signal,
	})
}

// appendCodexEvent является границей приватности observability. В журнал попадают
// только известный lifecycle, непрозрачные ID и типы item и финальный текст
// agentMessage. Reasoning, содержимое commandExecution, fileChange, MCP payload
// и неизвестные расширения протокола не сериализуются даже частично.
func appendCodexEvent(run *runstore.LockedRun, stepID, threadID, turnID string, event codex.Event) error {
	if strings.Contains(strings.ToLower(event.Method), "reasoning") {
		return nil
	}
	switch event.Method {
	case "turn/started", "turn/completed", "thread/status/changed", "item/started", "item/completed",
		"thread/tokenUsage/updated", "turn/tokenUsage/updated", "tokenUsage/updated", "error":
	default:
		// Расширяемый протокол может добавить произвольное уведомление. Оно не
		// относится к контракту observability и не должно останавливать workflow
		// даже при неизвестной для этой версии Lawa форме params.
		return nil
	}
	params, err := decodeEventParams(event.Params)
	if err != nil {
		return fmt.Errorf("нормализовать событие %s: %w", event.Method, err)
	}
	normalized := runstore.RuntimeEvent{StepID: stepID, ThreadID: threadID, TurnID: turnID}
	switch event.Method {
	case "turn/started":
		normalized.Kind, normalized.State = "turn_started", "running"
	case "turn/completed":
		normalized.Kind = "turn_completed"
		normalized.State = firstString(params, "turn.status", "status")
		normalized.Message = firstString(params, "turn.error.message", "error.message")
	case "thread/status/changed":
		normalized.Kind = "thread_status_changed"
		normalized.State = firstString(params, "thread.status.type", "status.type", "status")
	case "item/started", "item/completed":
		normalized.Kind = strings.ReplaceAll(event.Method, "/", "_")
		normalized.ItemID = firstString(params, "item.id")
		normalized.ItemType = firstString(params, "item.type", "type")
		if strings.Contains(strings.ToLower(normalized.ItemType), "reasoning") {
			return nil
		}
		// Непрозрачный ID нужен только для сопоставления item/started с
		// item/completed. Status и dashboard не показывают его и никогда не
		// получают command, arguments или result из исходного item.
		// Только финальный ответ агента нужен пользователю и зависимостям. Сырые
		// выводы команд и инструментов могут содержать секреты и не копируются.
		if event.Method == "item/completed" && normalized.ItemType == "agentMessage" {
			normalized.Message = firstString(params, "item.text", "item.content", "text")
		}
	case "thread/tokenUsage/updated", "turn/tokenUsage/updated", "tokenUsage/updated":
		normalized.Kind = "token_usage_updated"
		normalized.Usage = numericLeaves(params, 32)
	case "error":
		normalized.Kind = "error"
		normalized.Message = firstString(params, "error.message", "message")
	}
	return run.AppendEvent(normalized)
}

func decodeEventParams(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func firstString(root map[string]any, paths ...string) string {
	for _, path := range paths {
		var current any = root
		for _, part := range strings.Split(path, ".") {
			object, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = object[part]
		}
		if text, ok := current.(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

// numericLeaves извлекает только числовые листья события usage. Имена полей
// сохраняют путь, чтобы новая версия App Server могла расширить счётчики без
// сохранения исходного JSON. Жёсткий лимит защищает журнал от неожиданного payload.
func numericLeaves(root map[string]any, limit int) map[string]int64 {
	result := make(map[string]int64)
	var visit func(string, any)
	visit = func(path string, value any) {
		if len(result) >= limit {
			return
		}
		switch typed := value.(type) {
		case map[string]any:
			// map не гарантирует порядок. Сортировка делает применение лимита
			// воспроизводимым: один payload всегда сохраняет одни счётчики.
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				child := typed[key]
				next := key
				if path != "" {
					next = path + "." + key
				}
				visit(next, child)
			}
		case json.Number:
			if number, err := typed.Int64(); err == nil {
				result[path] = number
			}
		}
	}
	visit("", root)
	if len(result) == 0 {
		return nil
	}
	return result
}
