package teamruntime

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// peerEngine создаёт заказ с двумя приглашёнными коллегами. Только разработчик
// получает обязательное поручение; дизайнер отвечает на технические вопросы.
func peerEngine(t *testing.T) (*Engine, string, *fakeClient, *time.Time) {
	t.Helper()
	characters := workflow.DefaultTeamCharacters()
	characters["designer"] = workflow.Character{Name: "Дизайнер", History: "Проектирует интерфейсы", Instructions: "Уточняй требования"}
	definition := workflow.Workflow{ID: "office", Characters: characters, Steps: []workflow.Step{{ID: "boss", Type: "agent", Character: "boss", Prompt: "Работай", DependsOn: []string{}}}}
	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	s, err := runstore.Create(root, runstore.Input{Order: true, Team: true, CWD: t.TempDir(), Task: "Сделать интерфейс", WorkflowJSON: data})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	client := &fakeClient{}
	e := &Engine{Root: root, Client: client, Now: func() time.Time { return now }}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "boss-thread", "boss-1")
		tool(t, c, "team_summon", `{"id":"developer"}`)
		tool(t, c, "team_summon", `{"id":"designer"}`)
		tool(t, c, "team_post", `{"text":"@developer Сделай интерфейс"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, s.Meta.RunID, "boss")
	return e, s.Meta.RunID, client, &now
}

// Вопрос и ответ проходят через прежний team_post. Босс видит переписку, но
// не просыпается; после рестарта разработчик продолжает свой thread и сдаёт
// результат Боссу, даже когда входящая порция содержит только ответ коллеги.
func TestPeerConversationAndExistingThread(t *testing.T) {
	e, run, client, now := peerEngine(t)
	initial := readChat(t, e, run)
	bossCheck := initial.Room.Actors["boss"].NextCheck
	var taskID string
	for id := range initial.Room.Tasks {
		taskID = id
	}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "developer-thread", "developer-1")
		for _, text := range []string{"@human Привет", "@unknown Вопрос", "@developer Сам себе", "@system Вопрос", "Без адресата"} {
			args, _ := json.Marshal(map[string]string{"text": text})
			if _, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_post", CallID: text, Arguments: args}); err == nil {
				t.Fatalf("разрешён неверный адресат: %s", text)
			}
		}
		for _, text := range []string{"/goal Новая цель", "/complete @human Готово", "/retry designer"} {
			args, _ := json.Marshal(map[string]string{"text": text})
			if _, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_post", CallID: text, Arguments: args}); err == nil {
				t.Fatalf("коллеге разрешено управление: %s", text)
			}
		}
		tool(t, c, "team_post", `{"text":"@designer Какой цвет выбрать?"}`)
		tool(t, c, "team_post", `{"text":"@designer Какой цвет выбрать?"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "developer")
	chat := readChat(t, e, run)
	question := chat.Messages[len(chat.Messages)-1]
	if question.AuthorID != "developer" || question.To != "designer" || question.Kind != "question" || question.ReplyTo != taskID || !slices.Equal(question.ReplyToIDs, []string{taskID}) {
		t.Fatal("потерян источник вопроса", question)
	}
	if len(chat.Messages) != len(initial.Messages)+1 || len(chat.Room.Tasks) != 1 || !chat.Room.Actors["boss"].NextCheck.Equal(bossCheck) {
		t.Fatal("повтор вопроса, новое обязательство или пробуждение Босса", chat)
	}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "designer-thread", "designer-1")
		if !strings.Contains(c.Text, "не создаёт обязательного поручения") {
			t.Fatal("нет границ полномочий коллеги")
		}
		tool(t, c, "team_post", `{"text":"@developer Синий"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "designer")
	chat = readChat(t, e, run)
	reply := chat.Messages[len(chat.Messages)-1]
	if reply.AuthorID != "designer" || reply.To != "developer" || reply.Kind != "reply" || reply.ReplyTo != question.ID || reply.NotifyBoss || !chat.Room.Actors["boss"].NextCheck.Equal(bossCheck) {
		t.Fatal("ответ потерял связь или разбудил Босса", reply)
	}
	if claimed, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", now.Add(10*time.Minute)); err != nil || claimed {
		t.Fatal("переписка запустила Босса", claimed, err)
	}
	// Новый Engine читает сохранённую комнату; прежние tools и thread достаточны.
	next := &Engine{Root: e.Root, Client: client, Now: e.Now}
	client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "developer-1", LatestTurnStatus: "completed"}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "developer-thread", "developer-2")
		if !strings.Contains(c.Text, reply.ID) || !strings.Contains(c.Text, "Можно напрямую задавать вопросы") {
			t.Fatal("старый thread не получил новые сообщения и правила")
		}
		tool(t, c, "team_post", `{"text":"@boss Синий интерфейс готов, проверен"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, next, run, "developer")
	chat = readChat(t, next, run)
	report := chat.Messages[len(chat.Messages)-1]
	if report.To != "boss" || report.ReplyTo != reply.ID || !slices.Equal(client.continued, []string{"developer-thread"}) || len(chat.Room.Tasks) != 1 || chat.Room.Tasks[taskID].AcceptedAt != nil {
		t.Fatal("потерян thread, отчёт или исходное обязательство", chat, client.continued)
	}
}

// Конкурентные обращения к занятому коллеге сохраняются один раз. Они не
// меняют текущую доставку, а после завершения приходят одной следующей порцией.
func TestConcurrentPeerMessagesWhileBusy(t *testing.T) {
	e, run, _, now := peerEngine(t)
	claim := func(actor string) {
		t.Helper()
		ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, actor, *now)
		if err != nil || !ok {
			t.Fatal(actor, ok, err)
		}
	}
	claim("developer")
	question, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "first", "@designer Какой цвет?")
	if err != nil {
		t.Fatal(err)
	}
	claim("designer")
	before := readChat(t, e, run).Room.Actors["designer"].Delivery
	const count = 8
	errs := make(chan error, count*2)
	var wg sync.WaitGroup
	for i := range count * 2 {
		wg.Go(func() {
			_, err := runstore.PostActor(t.Context(), e.Root, run, "developer", fmt.Sprintf("concurrent-%d", i%count), "@designer Уточни контраст")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	chat := readChat(t, e, run)
	if len(chat.Messages) != before.End+count || chat.Room.Actors["designer"].Delivery.End != before.End || !slices.Equal(chat.Room.Actors["designer"].Delivery.IDs, []string{question.ID}) {
		t.Fatal("повтор или изменение активной доставки", chat)
	}
	if _, err = runstore.PostActor(t.Context(), e.Root, run, "designer", "answer", "@developer Синий"); err != nil {
		t.Fatal(err)
	}
	if err = e.finish(t.Context(), run, "designer"); err != nil {
		t.Fatal(err)
	}
	claim("designer")
	chat = readChat(t, e, run)
	if len(chat.Room.Actors["designer"].Delivery.IDs) != count || len(chat.Room.Tasks) != 1 {
		t.Fatal("неполная следующая порция или лишние поручения", chat)
	}
	// Повтор после фиксации доставки не добавляет сообщение и не меняет таймер.
	previousCheck := chat.Room.Actors["designer"].NextCheck
	if _, err = runstore.PostActor(t.Context(), e.Root, run, "developer", "concurrent-0", "@designer Уточни контраст"); err != nil {
		t.Fatal(err)
	}
	after := readChat(t, e, run)
	if len(after.Messages) != len(chat.Messages) || !after.Room.Actors["designer"].NextCheck.Equal(previousCheck) {
		t.Fatal("повтор изменил очередь")
	}
}
