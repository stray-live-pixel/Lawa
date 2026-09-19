package teamruntime

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// claimDiscussion резервирует реальную сохранённую доставку без сетевого клиента.
func claimDiscussion(t *testing.T, e *Engine, run, actor string) {
	t.Helper()
	ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, actor, time.Now().Add(time.Hour))
	if err != nil || !ok {
		t.Fatal(actor, ok, err)
	}
}

// postDiscussion проверяет обычный путь записи, включая team.lock и повтор ID.
func postDiscussion(t *testing.T, e *Engine, run, actor, id, text string) runstore.TeamMessage {
	t.Helper()
	m, err := runstore.PostActor(t.Context(), e.Root, run, actor, id, text)
	if err != nil {
		t.Fatal(id, err)
	}
	return m
}

// TestDiscussionLimitRestartAndBossDecision защищает границу ровно шести реплик,
// одну эскалацию после рестарта и отдельный новый цикл без приёмки обязательств.
func TestDiscussionLimitRestartAndBossDecision(t *testing.T) {
	e, run, _, _ := peerEngine(t)
	actor, to := "developer", "designer"
	for i := 1; i <= 6; i++ {
		claimDiscussion(t, e, run, actor)
		m := postDiscussion(t, e, run, actor, fmt.Sprintf("peer-%d", i), "@"+to+" Уточни решение")
		if m.Suppressed != (i == 6) {
			t.Fatal("неверная граница лимита", m)
		}
		if err := e.finish(t.Context(), run, actor); err != nil {
			t.Fatal(err)
		}
		// Новый Engine не хранит счётчик в памяти процесса.
		e = &Engine{Root: e.Root, Now: e.Now}
		actor, to = to, actor
	}
	chat := readChat(t, e, run)
	var root string
	for id, d := range chat.Room.Discussions {
		root = id
		if d.Count != 6 || d.State != "paused" {
			t.Fatal(d)
		}
	}
	escalations := 0
	for _, m := range chat.Messages {
		if m.Kind == "discussion_escalation" {
			escalations++
			if m.To != "boss" || len(m.ReplyToIDs) != 6 || !strings.Contains(m.Text, "peer-1") || !strings.Contains(m.Text, "peer-6") {
				t.Fatal(m)
			}
		}
	}
	if root == "" || escalations != 1 {
		t.Fatal("нужна одна эскалация", chat)
	}
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, actor, time.Now().Add(time.Hour)); ok || err != nil {
		t.Fatal("остановленная цепочка доставлена", ok, err)
	}
	postDiscussion(t, e, run, "designer", "peer-6", "@developer Уточни решение")
	claimDiscussion(t, e, run, "boss")
	command := "/discussion " + root + " 1 resume @developer Попробуй другой вариант"
	postDiscussion(t, e, run, "boss", "resume-1", command)
	postDiscussion(t, e, run, "boss", "resume-1", command)
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "stale-decision", command); err == nil {
		t.Fatal("повторное решение сбросило бюджет")
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	actor, to = "developer", "designer"
	for i := 1; i <= 6; i++ {
		claimDiscussion(t, e, run, actor)
		postDiscussion(t, e, run, actor, fmt.Sprintf("cycle2-%d", i), "@"+to+" Проверим вариант")
		if err := e.finish(t.Context(), run, actor); err != nil {
			t.Fatal(err)
		}
		actor, to = to, actor
	}
	claimDiscussion(t, e, run, "boss")
	postDiscussion(t, e, run, "boss", "close", "/discussion "+root+" 2 close Используем первый вариант")
	postDiscussion(t, e, run, "boss", "close", "/discussion "+root+" 2 close Используем первый вариант")
	chat = readChat(t, e, run)
	if chat.Room.Discussions[root].State != "closed" || len(chat.Room.Tasks) != 1 || chat.Room.Tasks[root].AcceptedAt != nil {
		t.Fatal("решение подменило приёмку", chat.Room)
	}
	escalations = 0
	for _, m := range chat.Messages {
		if m.Kind == "discussion_escalation" {
			escalations++
		}
	}
	if escalations != 2 {
		t.Fatal("ожидалось по одной эскалации на цикл", escalations)
	}
}

// TestDiscussionMixedDeliveryAndLateTurn проверяет независимые бюджеты в одной
// порции, запрет обхода через старый ход и доступность отчёта Боссу при паузе.
func TestDiscussionMixedDeliveryAndLateTurn(t *testing.T) {
	e, run, _, _ := peerEngine(t)
	// Вторая задача приходит тому же сотруднику, пока первая ещё не получена.
	if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error {
		b := chat.Room.Actors["boss"]
		b.Status, b.Delivery = "working", &runstore.TeamDelivery{IDs: []string{"initial-goal"}, End: len(chat.Messages)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	postDiscussion(t, e, run, "boss", "independent", "@developer Проверь клавиатуру")
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	claimDiscussion(t, e, run, "developer")
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "ambiguous", "@designer Уточни"); err == nil {
		t.Fatal("смешаны цепочки")
	}
	chat := readChat(t, e, run)
	root := chat.Room.Actors["developer"].Delivery.IDs[0]
	for i := 1; i <= 6; i++ {
		postDiscussion(t, e, run, "developer", fmt.Sprintf("burst-%d", i), "/reply "+root+" @designer Уточни цвет")
		if i == 1 {
			claimDiscussion(t, e, run, "designer")
		}
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "designer", "late", "@developer Ответ"); err == nil {
		t.Fatal("поздний ответ продолжил паузу")
	}
	postDiscussion(t, e, run, "designer", "boss-report", "@boss Моя позиция: синий")
	m := postDiscussion(t, e, run, "developer", "other", "/reply independent @designer Уточни клавиатуру")
	if m.Suppressed || m.DiscussionID != "independent" {
		t.Fatal("независимая задача остановлена", m)
	}
	claimDiscussion(t, e, run, "boss")
	postDiscussion(t, e, run, "boss", "resume", "/discussion "+root+" 1 resume @developer Другой вариант")
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "designer", "old-cycle", "@developer Старый ответ"); err == nil {
		t.Fatal("старый ход расходует новый бюджет")
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "forbidden", "/discussion "+root+" 2 close Закрываю"); err == nil {
		t.Fatal("сотрудник решил за Босса")
	}
	if err := e.finish(t.Context(), run, "designer"); err != nil {
		t.Fatal(err)
	}
	claimDiscussion(t, e, run, "designer")
	chat = readChat(t, e, run)
	ids := chat.Room.Actors["designer"].Delivery.IDs
	if len(ids) != 1 || ids[0] != "other" || chat.Room.Discussions[root].Count != 0 {
		t.Fatal("старая очередь ожила", ids)
	}
}

// TestDiscussionConcurrentRetries фиксирует гонку на шестом сообщении:
// два вызова с одним ID считаются один раз; лишние уникальные получают отказ.
func TestDiscussionConcurrentRetries(t *testing.T) {
	e, run, _, _ := peerEngine(t)
	claimDiscussion(t, e, run, "developer")
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			_, _ = runstore.PostActor(t.Context(), e.Root, run, "developer", fmt.Sprintf("message-%d", i%12), "@designer Уточни")
		})
	}
	wg.Wait()
	chat := readChat(t, e, run)
	peers, escalations := 0, 0
	for _, m := range chat.Messages {
		if m.AuthorID == "developer" {
			peers++
		}
		if m.Kind == "discussion_escalation" {
			escalations++
		}
	}
	if peers != 6 || escalations != 1 {
		t.Fatal(peers, escalations)
	}
	for _, d := range chat.Room.Discussions {
		if d.Count != 6 {
			t.Fatal(d)
		}
	}
}

// TestDiscussionLegacyRoom сохраняет исчерпанный бюджет переписки, созданной
// до обновления, не доставляя старый хвост и не создавая повторную эскалацию.
func TestDiscussionLegacyRoom(t *testing.T) {
	e, run, _, _ := peerEngine(t)
	claimDiscussion(t, e, run, "developer")
	for i := range 6 {
		postDiscussion(t, e, run, "developer", fmt.Sprintf("legacy-%d", i), "@designer Уточни")
	}
	if err := e.finish(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error {
		chat.Messages = chat.Messages[:len(chat.Messages)-1]
		for i := range chat.Messages {
			chat.Messages[i].DiscussionID, chat.Messages[i].DiscussionCycle, chat.Messages[i].Suppressed = "", 0, false
		}
		chat.Room.Discussions = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "designer", time.Now().Add(time.Hour))
		if err != nil || ok {
			t.Fatal("старая цепочка продолжилась", ok, err)
		}
	}
	chat := readChat(t, e, run)
	count := 0
	for _, m := range chat.Messages {
		if m.Kind == "discussion_escalation" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("повтор миграции", count)
	}
}

// TestDiscussionCommandsInExistingThread проверяет протокол именно через старый
// team_post: выбор источника, эскалация и решение не требуют новых dynamic tools.
func TestDiscussionCommandsInExistingThread(t *testing.T) {
	e, run, client, now := peerEngine(t)
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "developer-thread", "developer-turn")
		chat := readChat(t, e, run)
		source := chat.Room.Actors["developer"].Delivery.IDs[0]
		for i := range 6 {
			args, _ := json.Marshal(map[string]string{"text": "/reply " + source + " @designer Уточни вариант"})
			if _, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_post", CallID: fmt.Sprint(i), Arguments: args}); err != nil {
				t.Fatal(err)
			}
		}
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "developer")
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "boss-thread", "boss-decision")
		chat := readChat(t, e, run)
		for id := range chat.Room.Discussions {
			args, _ := json.Marshal(map[string]string{"text": "/discussion " + id + " 1 close Используем синий"})
			if _, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_post", CallID: "close", Arguments: args}); err != nil {
				t.Fatal(err)
			}
		}
		return codex.Result{Status: "completed"}, nil
	}
	// Продвигаем часы scheduler после реальных записей с fsync.
	*now = time.Now().Add(time.Second)
	client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "boss-1", LatestTurnStatus: "completed"}
	process(t, e, run, "boss")
	for _, d := range readChat(t, e, run).Room.Discussions {
		if d.State != "closed" {
			t.Fatal(d)
		}
	}
}

// TestDiscussionStoppedTurnDoesNotEscalateAgain проверяет, что завершение
// уже запущенного хода после лимита не создаёт вторую жалобу на отсутствие ответа.
func TestDiscussionStoppedTurnDoesNotEscalateAgain(t *testing.T) {
	e, run, _, _ := peerEngine(t)
	claimDiscussion(t, e, run, "developer")
	for i := range 6 {
		postDiscussion(t, e, run, "developer", fmt.Sprint(i), "@designer Уточни")
		if i == 0 {
			claimDiscussion(t, e, run, "designer")
		}
	}
	if err := e.finish(t.Context(), run, "designer"); err != nil {
		t.Fatal(err)
	}
	chat := readChat(t, e, run)
	addressed := 0
	for _, m := range chat.Messages {
		if m.AuthorID == "system" && m.To == "boss" {
			addressed++
		}
	}
	if addressed != 1 {
		t.Fatal("лишняя эскалация", addressed)
	}
}
