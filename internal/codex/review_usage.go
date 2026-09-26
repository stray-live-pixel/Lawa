package codex

import "fmt"

// ThreadUsageGroup — публичная разбивка account/usage/read по модели. Эти
// сведения могут отсутствовать при недоступном billing route; nil не равен 0.
type ThreadUsageGroup struct {
	Model             *string `json:"model"`
	ReasoningEffort   *string `json:"reasoningEffort"`
	InputTokens       *int64  `json:"inputTokens"`
	CachedInputTokens *int64  `json:"cachedInputTokens"`
	OutputTokens      *int64  `json:"outputTokens"`
}

// ReadUsage не запускает turn и читает только явно запрошенный thread. Ответ
// принадлежит API, а не вычисляется по длине текста. Провайдер может вернуть
// отсутствие метрик; вызывающий обязан сохранить неполноту, а не выдумать нули.
func (o *Observer) ReadUsage(threadID string) ([]ThreadUsageGroup, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.session == nil || !validProtocolText(threadID) {
		return nil, fmt.Errorf("нужна открытая сессия и ID чата")
	}
	var response struct {
		ThreadUsage *struct {
			ThreadID string             `json:"threadId"`
			Groups   []ThreadUsageGroup `json:"groups"`
		} `json:"threadUsage"`
	}
	if err := o.session.client.call("account/usage/read", map[string]any{"threadId": threadID}, &response); err != nil {
		return nil, err
	}
	if response.ThreadUsage == nil {
		return nil, fmt.Errorf("источник не сообщил usage чата")
	}
	if response.ThreadUsage.ThreadID != threadID {
		return nil, fmt.Errorf("usage относится к другому чату")
	}
	if len(response.ThreadUsage.Groups) == 0 {
		return nil, fmt.Errorf("разбивка usage пока отсутствует")
	}
	return response.ThreadUsage.Groups, nil
}
