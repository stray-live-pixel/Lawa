package teamruntime

import (
	"context"
	"encoding/json/v2"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// activity обновляет облачко по публичным сообщениям и началу действий.
// Поток токенов, reasoning, аргументы инструментов и вывод команд не сохраняются.
// Это краткое наблюдение, а не новый пост: оно никого не тегает и не будит.
func (e *Engine) activity(run, id string, event codex.Event) error {
	if event.Method != "item/started" && event.Method != "item/completed" {
		return nil
	}
	var data struct {
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if err := json.Unmarshal(event.Params, &data); err != nil {
		return nil
	}
	summary := ""
	if event.Method == "item/started" {
		switch data.Item.Type {
		case "commandExecution":
			summary = "Работает с проектом"
		case "fileChange":
			summary = "Меняет файлы"
		case "webSearch":
			summary = "Ищет информацию"
		case "mcpToolCall", "dynamicToolCall":
			summary = "Выполняет действие"
		}
	} else if data.Item.Type == "agentMessage" {
		words := strings.Fields(data.Item.Text)
		summary = strings.Join(words[:min(7, len(words))], " ")
		if len([]rune(summary)) > 100 {
			summary = string([]rune(summary)[:97]) + "…"
		}
	}
	if summary == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.change(ctx, run, id, func(a *runstore.TeamActor) { a.Summary = summary })
}
