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

// appendCodexEvent является границей между расширяемым протоколом App Server и
// устойчивым журналом Lawa. Lifecycle и короткая Message остаются безопасным
// интерфейсом status/logs. Content сохраняет только выбранный операторский поток:
// сообщения агента, читаемые reasoning summary, команды, stdout/stderr, план и
// названия действий. Он доступен локальной панели Dashboard и может содержать
// секреты из рабочего процесса. Сырые reasoning, аргументы/result инструментов
// и неизвестные payload не сериализуются даже частично.
func appendCodexEvent(run *runstore.LockedRun, stepID, threadID, turnID string, event codex.Event) error {
	if event.Method == "item/reasoning/textDelta" || event.Method == "item/reasoning/delta" {
		return nil
	}
	switch event.Method {
	case "turn/started", "turn/completed", "thread/status/changed", "item/started", "item/completed",
		"item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/commandExecution/outputDelta",
		"turn/plan/updated", "thread/tokenUsage/updated", "turn/tokenUsage/updated", "tokenUsage/updated", "error":
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
		// Непрозрачный ID сопоставляет lifecycle в status и delta в live-панели.
		// Аргументы и result инструментов не копируются: даже локальному оператору
		// достаточно имени действия, а произвольный payload слишком легко
		// превращает журнал в неконтролируемое хранилище секретов.
		if event.Method == "item/started" {
			normalized.Content = itemTraceContent(params, normalized.ItemType)
		}
		if event.Method == "item/completed" && normalized.ItemType == "agentMessage" {
			normalized.Message = firstString(params, "item.text", "item.content", "text")
			normalized.Content = normalized.Message
		}
	case "item/agentMessage/delta":
		normalized.Kind, normalized.ItemType = "agent_message_delta", "agentMessage"
		normalized.ItemID = firstString(params, "itemId", "item.id")
		normalized.Content = firstRawString(params, "delta")
	case "item/reasoning/summaryTextDelta":
		normalized.Kind, normalized.ItemType = "reasoning_summary_delta", "reasoningSummary"
		normalized.ItemID = firstString(params, "itemId", "item.id")
		normalized.Content = firstRawString(params, "delta")
	case "item/commandExecution/outputDelta":
		normalized.Kind, normalized.ItemType = "command_output_delta", "commandExecution"
		normalized.ItemID = firstString(params, "itemId", "item.id")
		normalized.Content = firstRawString(params, "delta")
	case "turn/plan/updated":
		normalized.Kind, normalized.ItemType = "plan_updated", "plan"
		normalized.Content = planTraceContent(params)
	case "thread/tokenUsage/updated", "turn/tokenUsage/updated", "tokenUsage/updated":
		normalized.Kind = "token_usage_updated"
		normalized.Usage = numericLeaves(params, 32)
	case "error":
		normalized.Kind = "error"
		normalized.Message = firstString(params, "error.message", "message")
	}
	if strings.HasSuffix(normalized.Kind, "_delta") && normalized.Content == "" {
		return nil
	}
	if normalized.Kind == "plan_updated" && normalized.Content == "" {
		return nil
	}
	return run.AppendEvent(normalized)
}

// itemTraceContent берёт только поля, которые объясняют действие человеку.
// Для fileChange сохраняются путь и вид изменения без diff, а для инструментов —
// только имя. Команда и связанный с ней output delta могут содержать чувствительные
// данные; Dashboard явно предупреждает об этом и остаётся loopback-интерфейсом по
// умолчанию.
func itemTraceContent(params map[string]any, itemType string) string {
	switch itemType {
	case "commandExecution":
		return firstString(params, "item.command")
	case "mcpToolCall":
		return joinTraceParts(firstString(params, "item.server"), firstString(params, "item.tool"))
	case "dynamicToolCall", "collabToolCall":
		return firstString(params, "item.tool")
	case "webSearch":
		return firstString(params, "item.query")
	case "imageView":
		return firstString(params, "item.path")
	case "fileChange":
		item, _ := params["item"].(map[string]any)
		changes, _ := item["changes"].([]any)
		lines := make([]string, 0, len(changes))
		for _, value := range changes {
			change, _ := value.(map[string]any)
			path, _ := change["path"].(string)
			kind, _ := change["kind"].(string)
			if text := joinTraceParts(kind, path); text != "" {
				lines = append(lines, text)
			}
		}
		return strings.Join(lines, "\n")
	}
	return ""
}

func planTraceContent(params map[string]any) string {
	lines := make([]string, 0)
	if explanation := firstString(params, "explanation"); explanation != "" {
		lines = append(lines, explanation)
	}
	plan, _ := params["plan"].([]any)
	for _, value := range plan {
		entry, _ := value.(map[string]any)
		step, _ := entry["step"].(string)
		status, _ := entry["status"].(string)
		if text := joinTraceParts(status, step); text != "" {
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, "\n")
}

func joinTraceParts(parts ...string) string {
	nonempty := parts[:0]
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			nonempty = append(nonempty, part)
		}
	}
	return strings.Join(nonempty, " · ")
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

// firstRawString отличается от firstString сохранением whitespace-only delta.
// Потоковые фрагменты могут разделять слова единственным пробелом; trim сделал бы
// собранное сообщение нечитаемым, хотя для обычных полей пустой текст бесполезен.
func firstRawString(root map[string]any, path string) string {
	var current any = root
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[part]
	}
	text, _ := current.(string)
	return text
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
