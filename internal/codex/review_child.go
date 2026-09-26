package codex

import (
	"fmt"
	"strings"
)

// ChildInfo содержит только публичные настройки и входные сообщения ребёнка.
// Закрытые/зашифрованные сообщения не читаются и не восстанавливаются из rollout.
type ChildInfo struct{ ThreadID, ParentThreadID, Model, Effort, Prompt string }

// ReadChildInfo подтверждает связь с родителем и проектом перед сохранением
// данных. Некоторые версии API не возвращают prompt субагента: тогда он пустой.
func (o *Observer) ReadChildInfo(threadID, parentID string) (ChildInfo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var info ChildInfo
	if o.session == nil || !validProtocolText(threadID) || !validProtocolText(parentID) {
		return info, fmt.Errorf("нужны открытая сессия и ID родителя/субагента")
	}
	var result struct {
		Thread struct {
			ID, CWD, ParentThreadID, Model, ReasoningEffort string
			Turns                                           []struct {
				Items []struct {
					Type    string
					Content []struct{ Type, Text string }
				}
			}
		}
	}
	if err := o.session.client.call("thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &result); err != nil {
		return info, err
	}
	t := result.Thread
	if t.ID != threadID || t.ParentThreadID != parentID {
		return info, fmt.Errorf("субагент принадлежит другому запуску")
	}
	same, err := connectionDirectoryMatches(o.connection, t.CWD)
	if err != nil || !same {
		return info, fmt.Errorf("субагент относится к другому проекту")
	}
	info = ChildInfo{ThreadID: t.ID, ParentThreadID: t.ParentThreadID, Model: t.Model, Effort: t.ReasoningEffort}
	for _, turn := range t.Turns {
		for _, item := range turn.Items {
			if item.Type != "userMessage" {
				continue
			}
			var texts []string
			for _, c := range item.Content {
				if c.Type == "text" || c.Type == "inputText" {
					texts = append(texts, c.Text)
				}
			}
			if len(texts) > 0 {
				info.Prompt = strings.Join(texts, "\n\n")
				return info, nil
			}
		}
	}
	return info, nil
}
