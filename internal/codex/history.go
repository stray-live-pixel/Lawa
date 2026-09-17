package codex

import "fmt"

// HistoricalTurn содержит только границы публичного хода. Время протокола —
// Unix seconds; nil означает отсутствие данных, а не нулевую дату.
type HistoricalTurn struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	StartedAt   *int64 `json:"startedAt"`
	CompletedAt *int64 `json:"completedAt"`
}

// ReadTurns читает сохранённый thread без resume/запуска модели. Поля items,
// reasoning и tool payload намеренно не декодируются. Проверки ID и cwd не дают
// подмешать в плеер историю другого проекта или личности.
func (o *Observer) ReadTurns(threadID string) ([]HistoricalTurn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.session == nil || !validProtocolText(threadID) {
		return nil, fmt.Errorf("нужна открытая сессия и ID чата")
	}
	var response struct {
		Thread struct {
			ID, CWD string
			Turns   []HistoricalTurn
		}
	}
	if err := o.session.client.call("thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &response); err != nil {
		return nil, err
	}
	if response.Thread.ID != threadID {
		return nil, fmt.Errorf("Codex вернул другой чат")
	}
	same, err := connectionDirectoryMatches(o.connection, response.Thread.CWD)
	if err != nil {
		return nil, err
	}
	if !same {
		return nil, fmt.Errorf("чат %s относится к другой рабочей папке", threadID)
	}
	return response.Thread.Turns, nil
}
