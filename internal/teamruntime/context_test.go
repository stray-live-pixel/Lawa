package teamruntime

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// Реальный scheduler и динамические инструменты, управляемый клиент вместо LLM.
// Человеческих сообщений после порога нет; Босс просыпается из сохранённой очереди.
func TestAutomaticCompactionSchedulerAndOldThreadCommands(t *testing.T) {
	e, run, client, _ := teamEngine(t)
	if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error {
		chat.Room.Actors["boss"].Cursor = len(chat.Messages)
		chat.Room.Actors["boss"].NextCheck = time.Now().Add(time.Hour)
		chat.ContextPolicy = &runstore.TeamContextPolicy{ThresholdTokens: 1200, RecentTokens: 300, SummaryTokens: 600, ResponseTokens: 3000}
		for i := 0; i < 40; i++ {
			chat.Messages = append(chat.Messages, runstore.TeamMessage{ID: fmt.Sprint(i), AuthorID: "developer", Text: strings.Repeat("факт ", 25), Date: time.Now()})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "boss-thread", "compact-turn")
		if strings.Contains(c.Text, strings.Repeat("факт ", 25)) {
			t.Fatal("полный журнал подмешан в prompt вместо адресных входов")
		}
		output, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_read", Arguments: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		var view runstore.TeamContext
		if err = json.Unmarshal([]byte(output), &view); err != nil {
			t.Fatal(err)
		}
		if view.Compaction == nil || view.Compaction.Status != "pending" {
			t.Fatal(view)
		}
		// Старый thread использует team_post /context вместо новой schema team_read.
		options, _ := json.Marshal(runstore.TeamReadOptions{Archive: true, From: view.Compaction.From, Through: view.Compaction.Through, Limit: 10})
		args, _ := json.Marshal(map[string]string{"text": "/context " + string(options)})
		archive, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_post", CallID: "archive", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(archive, `"hasMore":true`) {
			t.Fatal(archive)
		}
		input, _ := json.Marshal(runstore.TeamSummary{RequestID: view.Compaction.ID, Through: view.Compaction.Through, Text: "Остаётся открытый вопрос о приёмке. Результат пока не подтверждён.", SourceIDs: []string{"0"}})
		args, _ = json.Marshal(map[string]string{"text": "/compact " + string(input)})
		tool(t, c, "team_post", string(args))
		tool(t, c, "team_post", string(args))
		return codex.Result{Status: "completed"}, nil
	}
	e.tick(t.Context())
	e.wg.Wait()
	chat := readChat(t, e, run)
	if client.calls != 1 || chat.Compaction != nil && chat.Compaction.Pending != nil {
		t.Fatal(client.calls, chat.Compaction)
	}
	client.observed = codex.Observation{LatestTurnID: "compact-turn"}
	e.tick(t.Context())
	e.wg.Wait()
	if client.calls != 1 {
		t.Fatal("пустой повторный запуск")
	}
	// Отдельная копия состояния runtime читает ту же опубликованную версию.
	view, err := runstore.ReadTeamContext(e.Root, run, runstore.TeamReadOptions{})
	if err != nil || view.Summary == nil {
		t.Fatal(view, err)
	}
	if strings.Contains(outputPrompt(e, run, chat), `"metrics"`) {
		t.Fatal("метрики попали в prompt")
	}
}

// outputPrompt проверяет рабочую команду без дополнительного запуска модели.
func outputPrompt(e *Engine, run string, chat runstore.TeamChat) string {
	chat.Room.Actors["boss"].Delivery = &runstore.TeamDelivery{IDs: []string{"initial-goal"}, End: 1}
	s, _ := runstore.Load(e.Root, run)
	return e.command(run, "boss", chat, s).Text
}

// Два доказанно завершённых хода без сводки останавливают повтор. Перезапуск
// scheduler не снимает failed; восстановление требует явной команды оператора.
func TestCompactionRetryIsBounded(t *testing.T) {
	e, run, client, _ := teamEngine(t)
	if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error {
		chat.ContextPolicy = &runstore.TeamContextPolicy{ThresholdTokens: 900, RecentTokens: 200}
		for i := 0; i < 30; i++ {
			chat.Messages = append(chat.Messages, runstore.TeamMessage{ID: fmt.Sprint(i), Text: strings.Repeat("факт ", 30)})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client.execute = func(context.Context, codex.Command) (codex.Result, error) {
		return codex.Result{Status: "completed"}, nil
	}
	e.tick(t.Context())
	e.wg.Wait()
	e.tick(t.Context())
	e.wg.Wait()
	chat := readChat(t, e, run)
	if client.calls != 2 || chat.Compaction.Pending.Status != "failed" {
		t.Fatal(client.calls, chat.Compaction)
	}
	e.tick(t.Context())
	e.wg.Wait()
	if client.calls != 2 {
		t.Fatal("бесконечный повтор")
	}
	if _, err := runstore.RetryTeamCompaction(t.Context(), e.Root, run, "human", "manual-retry"); err != nil {
		t.Fatal(err)
	}
	if readChat(t, e, run).Compaction.Pending.Status != "pending" {
		t.Fatal("нет явного восстановления")
	}
	count := len(readChat(t, e, run).Messages)
	if _, err := runstore.RetryTeamCompaction(t.Context(), e.Root, run, "human", "manual-retry"); err != nil {
		t.Fatal(err)
	}
	if len(readChat(t, e, run).Messages) != count {
		t.Fatal("повтор ручной команды создал дубль")
	}

}

// После аварии подтверждённый завершённый turn наблюдается, а не отправляется
// заново. Старая попытка после смены ID не может заменить актуальную сводку.
func TestCompactionRecoveryAndStaleAttempt(t *testing.T) {
	e, run, client, now := teamEngine(t)
	if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error {
		chat.ContextPolicy = &runstore.TeamContextPolicy{ThresholdTokens: 900, RecentTokens: 200}
		for i := 0; i < 30; i++ {
			chat.Messages = append(chat.Messages, runstore.TeamMessage{ID: fmt.Sprint(i), Text: strings.Repeat("факт ", 30)})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old := *readChat(t, e, run).Compaction.Pending
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := e.change(t.Context(), run, "boss", func(a *runstore.TeamActor) {
		a.ThreadID = "thread"
		a.TurnID = "turn"
		a.Delivery.TurnID = "turn"
		a.Delivery.Attempted = true
	}); err != nil {
		t.Fatal(err)
	}
	// Новый Engine не разделяет оперативную память прежнего процесса.
	restarted := &Engine{Root: e.Root, Client: client, Now: e.Now}
	client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "turn", LatestTurnStatus: "completed"}
	process(t, restarted, run, "boss")
	if client.calls != 0 {
		t.Fatal("повторная отправка после рестарта")
	}
	next := readChat(t, e, run).Compaction.Pending
	if next == nil || next.ID == old.ID || next.Attempt != 2 {
		t.Fatal(next)
	}
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", now.Add(time.Minute)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	stale := runstore.TeamSummary{RequestID: old.ID, Through: old.Through, Text: "Устаревшая сводка", SourceIDs: []string{"0"}}
	if _, err := runstore.PublishTeamSummary(t.Context(), e.Root, run, "boss", stale); err == nil {
		t.Fatal("устаревшая попытка опубликована")
	}
	if err := restarted.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	failed := readChat(t, e, run)
	if failed.Compaction.Pending.Status != "failed" {
		t.Fatal(failed.Compaction)
	}
	restarted.tick(t.Context())
	restarted.wg.Wait()
	if client.calls != 0 {
		t.Fatal("failed запущен после рестарта")
	}
}
