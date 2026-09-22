package runstore

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Страницы не зависят от конкурентной записи: курсор ссылается на устойчивый
// префикс, повтор чтения не продвигает доставку и не теряет край страницы.
func TestContextPagesConcurrentAndInvalidCursor(t *testing.T) {
	root := t.TempDir()
	s := teamRun(t, root, "", "Цель")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			_, err := PostTeam(t.Context(), root, s.Meta.RunID, "work", fmt.Sprint(i), "Факт")
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	first, err := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Limit: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 7 || !first.HasMore || first.Messages[6].Position != 7 {
		t.Fatal(first)
	}
	_, err = PostTeam(t.Context(), root, s.Meta.RunID, "work", "late", "Поздний факт")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Cursor: first.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 14 || second.Messages[0].Position != 8 || second.Messages[13].ID != "late" {
		t.Fatal(second)
	}
	empty, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Cursor: second.Cursor})
	if len(empty.Messages) != 0 || empty.HasMore {
		t.Fatal(empty)
	}
	invalid, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Cursor: "invalid"})
	if !invalid.ResetRequired || invalid.Notice == "" {
		t.Fatal(invalid)
	}
	other := teamRun(t, root, "", "Другая цель")
	wrong, _ := ReadTeamContext(root, other.Meta.RunID, TeamReadOptions{Cursor: first.Cursor})
	if !wrong.ResetRequired {
		t.Fatal(wrong)
	}
	archive, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Archive: true, From: 3, Through: 6, Limit: 2})
	tail, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Archive: true, From: 3, Through: 6, Cursor: archive.Cursor})
	if len(archive.Messages) != 2 || len(tail.Messages) != 2 || tail.Messages[1].Position != 6 || tail.HasMore {
		t.Fatal(archive, tail)
	}
	byID, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Archive: true, IDs: []string{"late"}})
	if len(byID.Messages) != 1 {
		t.Fatal(byID)
	}
}

// Публикация не меняет ни исходники, ни цель, ни поручения. Запись повторной
// версии не удаляет незавершённый вопрос; перенос смысла проверяет Босс.
func TestCompactionVersionsIdempotencyAndBoundaries(t *testing.T) {
	root := t.TempDir()
	s := teamRun(t, root, "", "Цель")
	err := UpdateTeam(t.Context(), root, s.Meta.RunID, func(chat *TeamChat) error {
		initializeRoom(chat, s.Workflow.Characters)
		chat.ContextPolicy = &TeamContextPolicy{ThresholdTokens: 900, RecentTokens: 200, SummaryTokens: 500, ResponseTokens: 2000}
		for i := 0; i < 35; i++ {
			chat.Messages = append(chat.Messages, TeamMessage{ID: fmt.Sprint(i), AuthorID: "human", Text: strings.Repeat("факт ", 20), Date: time.Now()})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := ReadTeam(root, s.Meta.RunID)
	r := *before.Compaction.Pending
	first, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Limit: 2})
	if first.Summary != nil || first.Messages[0].ID != "initial-goal" {
		t.Fatal(first)
	}
	err = UpdateTeam(t.Context(), root, s.Meta.RunID, func(chat *TeamChat) error {
		chat.Room.Actors["boss"].Status = "working"
		chat.Room.Actors["boss"].Delivery = &TeamDelivery{IDs: []string{r.ID}, End: len(chat.Messages)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteTeam(t.Context(), root, s.Meta.RunID, "boss", "premature-completion", "@human Готово"); err == nil {
		t.Fatal("завершение поглотило pending compaction")
	}
	summary := TeamSummary{RequestID: r.ID, Through: r.Through, Text: "Вопрос о приёмке остаётся открытым. Предложение цвета не принято.", SourceIDs: []string{"0"}}
	if _, err := PublishTeamSummary(t.Context(), root, s.Meta.RunID, "developer", summary); err == nil {
		t.Fatal("сотрудник опубликовал сводку")
	}
	_, err = PostTeam(t.Context(), root, s.Meta.RunID, "", "concurrent", "Свежий факт")
	if err != nil {
		t.Fatal(err)
	}
	event, err := PublishTeamSummary(t.Context(), root, s.Meta.RunID, "boss", summary)
	if err != nil {
		t.Fatal(err)
	}
	again, err := PublishTeamSummary(t.Context(), root, s.Meta.RunID, "boss", summary)
	if err != nil || again.ID != event.ID {
		t.Fatal(again, err)
	}
	summary.Text = "Подмена"
	if _, err := PublishTeamSummary(t.Context(), root, s.Meta.RunID, "boss", summary); err == nil {
		t.Fatal("перезаписана версия")
	}
	after, _ := ReadTeam(root, s.Meta.RunID)
	if after.Goal != before.Goal || after.Messages[1].Text != before.Messages[1].Text || len(after.Messages) != len(before.Messages)+2 {
		t.Fatal("изменены исходники")
	}
	view, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{})
	if view.Summary == nil || view.Messages[0].Position != r.Through+1 {
		t.Fatal(view)
	}
	invalid, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Cursor: first.Cursor})
	if !invalid.ResetRequired {
		t.Fatal("старый курсор не требует снимка")
	}
	archived, _ := ReadTeamContext(root, s.Meta.RunID, TeamReadOptions{Archive: true, IDs: []string{"0"}})
	if len(archived.Messages) != 1 {
		t.Fatal(archived)
	}
	// Второй порог сохраняет предыдущую версию и ещё не закрытый вопрос.
	err = UpdateTeam(t.Context(), root, s.Meta.RunID, func(chat *TeamChat) error {
		for i := 0; i < 35; i++ {
			chat.Messages = append(chat.Messages, TeamMessage{ID: fmt.Sprintf("next-%d", i), Text: strings.Repeat("новое ", 20)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	newer, _ := ReadTeam(root, s.Meta.RunID)
	next := newer.Compaction.Pending
	if next == nil || next.PreviousID != r.ID || next.From != r.Through+1 {
		t.Fatal(next)
	}
	second := TeamSummary{RequestID: next.ID, Through: next.Through, Text: "Вопрос о приёмке всё ещё открыт. Получены новые результаты.", SourceIDs: []string{"0", "next-0"}}
	if _, err := PublishTeamSummary(t.Context(), root, s.Meta.RunID, "boss", second); err != nil {
		t.Fatal(err)
	}
	final, _ := ReadTeam(root, s.Meta.RunID)
	count := 0
	for _, m := range final.Messages {
		if m.Summary != nil {
			count++
		}
	}
	if count != 2 || !strings.Contains(latestTeamSummary(final).Text, "всё ещё открыт") {
		t.Fatal(final)
	}
	if final.History.Frames[len(final.History.Frames)-1].MessageCount != len(final.Messages) {
		t.Fatal("нет исторического события")
	}
}

// Лимит относится ко всему обычному ответу; сообщение выдаётся целиком или
// явно сообщается, что нужен архив. Метрики и закрытые поручения не просачиваются.
func TestContextBudgetAndFixedWorkflow(t *testing.T) {
	chat := newTeam("run", "Цель")
	chat.ContextPolicy = &TeamContextPolicy{ResponseTokens: 900}
	for i := 0; i < 30; i++ {
		chat.Messages = append(chat.Messages, TeamMessage{ID: fmt.Sprint(i), Text: strings.Repeat("содержимое ", 25)})
	}
	ensureTeamCompaction(&chat, time.Now())
	if chat.Compaction != nil {
		t.Fatal("создан Босс фиксированному workflow")
	}
	view, err := teamContext(chat, TeamReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !view.HasMore || len(view.Messages) == 0 || EstimateTeamTokens(view) > 900 {
		t.Fatal(view)
	}
	chat.Messages[0].Text = strings.Repeat("большой текст ", 600)
	view, _ = teamContext(chat, TeamReadOptions{})
	if !view.HasMore || len(view.Messages) != 0 || !strings.Contains(view.Notice, "превышает") {
		t.Fatal(view)
	}
	archive, _ := teamContext(chat, TeamReadOptions{Archive: true, IDs: []string{"0"}})
	if len(archive.Messages) != 1 || archive.Messages[0].Text != chat.Messages[0].Text {
		t.Fatal("обрезан оригинал")
	}
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), "metrics") || strings.Contains(string(data), "history") {
		t.Fatal(string(data))
	}
}
