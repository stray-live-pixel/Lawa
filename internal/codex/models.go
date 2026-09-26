package codex

import (
	"context"
	"errors"
	"fmt"
)

// Model описывает доступную аккаунту модель и разрешённые уровни reasoning.
// Discovery не создаёт thread и не расходует модельные токены.
type Model struct {
	ID                        string `json:"id"`
	Model                     string `json:"model"`
	DisplayName               string `json:"displayName"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
		Description     string `json:"description"`
	} `json:"supportedReasoningEfforts"`
}

// ListModels использует официальный model/list и все страницы каталога. Ошибка
// открытия аккаунта не заменяется встроенным списком якобы доступных моделей.
func ListModels(ctx context.Context, connection Connection) (models []Model, err error) {
	if err = validateConnection(ctx, connection); err != nil {
		return nil, err
	}
	s, err := openSession(ctx, Command{Executable: connection.Executable, CWD: connection.CWD, Stderr: connection.Stderr, Directory: connection.Directory}, &Result{})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close(), ctx.Err()) }()
	var cursor *string
	seen := map[string]bool{}
	for {
		var response struct {
			Data       []Model `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err = s.client.call("model/list", map[string]any{"includeHidden": false, "cursor": cursor}, &response); err != nil {
			return nil, err
		}
		models = append(models, response.Data...)
		if response.NextCursor == nil || *response.NextCursor == "" {
			return models, nil
		}
		if seen[*response.NextCursor] {
			return nil, fmt.Errorf("model/list повторил курсор")
		}
		seen[*response.NextCursor] = true
		cursor = response.NextCursor
	}
}
