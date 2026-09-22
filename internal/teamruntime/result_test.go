package teamruntime

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/teamreport"
)

// Полный рабочий цикл: явный результат, возврат Босса, исправление и отдельная
// приёмка. Повторы RPC не дублируют сообщения, задачи или число возвратов.
func TestResultReworkAndAcceptance(t *testing.T) {
	e, run, _, now := assignedTeam(t)
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(6*time.Minute)); err != nil || !ok {
		t.Fatal(err)
	}
	post := func(author, id, text string) runstore.TeamMessage {
		t.Helper()
		m, err := runstore.PostActor(t.Context(), e.Root, run, author, id, text)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := post("developer", "result", "/result @boss Готов файл game.go; проверка go test")
	if first.Kind != "task_result" || len(first.TaskIDs) != 1 || first.TaskIDs[0] != "assignment" {
		t.Fatal(first)
	}
	post("developer", "result", "/result @boss Готов файл game.go; проверка go test")
	if err := e.finish(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	returned := post("boss", "rework", "/rework result @developer Исправь управление")
	if returned.Kind != "task_rework" || returned.ResultID != "result" {
		t.Fatal(returned)
	}
	post("boss", "rework", "/rework result @developer Исправь управление")
	// Старый отчёт не закрывает возвращённое обязательство, даже когда
	// сотрудник уже idle, а новая доставка ещё не началась.
	if _, err := runstore.AcceptTeamTasks(t.Context(), e.Root, run, "boss", "stale-accept", []string{"assignment"}, "result", "Старая проверка"); err == nil {
		t.Fatal("принят возвращённый результат до исправления")
	}
	if len(readChat(t, e, run).Room.Tasks) != 1 {
		t.Fatal("возврат создал новое обязательство")
	}
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(7*time.Minute)); err != nil || !ok {
		t.Fatal(err)
	}
	post("developer", "result2", "/result @boss Управление исправлено, тесты прошли")
	if err := e.finish(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.AcceptTeamTasks(t.Context(), e.Root, run, "boss", "accept", []string{"assignment"}, "result2", "Проверил управление"); err != nil {
		t.Fatal(err)
	}
	r := teamreport.Build(readChat(t, e, run), time.Time{})
	if len(r.Tasks) != 1 || r.Tasks[0].Reworks == nil || *r.Tasks[0].Reworks != 1 || r.Tasks[0].AcceptedAt == nil || !r.Tasks[0].FirstResultAt.Equal(first.Date) {
		t.Fatal(r.Tasks)
	}
	// Принятые поручения остаются в журнале, но не повторяются в обычном контексте.
	// Поздний возврат уже принятого результата не возобновляет поручение.
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "late", "/rework result2 @developer Ещё правка"); err == nil {
		t.Fatal("возвращено принятое поручение")
	}
	shared, err := runstore.ReadTeamContext(e.Root, run, runstore.TeamReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(shared)
	if strings.Contains(string(data), `"metrics"`) || strings.Contains(string(data), `"history"`) || len(shared.Room.Tasks) != 0 || !strings.Contains(string(data), `"task_rework"`) {
		t.Fatal("потеряна рабочая память")
	}
}

// Команды не расширяют авторство: сотрудник не возвращает результат коллеги,
// Босс не заявляет готовность за сотрудника; неверный ID не меняет историю.
func TestResultCommandsRejectInvalidReferences(t *testing.T) {
	e, run, _, now := assignedTeam(t)
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(6*time.Minute)); err != nil || !ok {
		t.Fatal(err)
	}
	before := len(readChat(t, e, run).Messages)
	for _, in := range []struct{ author, text string }{
		{"boss", "/result @boss Готово"},
		{"developer", "/result @human Готово"},
		{"developer", "/rework missing @boss Исправь"},
		{"boss", "/rework missing @developer Исправь"},
	} {
		if _, err := runstore.PostActor(t.Context(), e.Root, run, in.author, "invalid", in.text); err == nil {
			t.Fatal(in)
		}
	}
	if len(readChat(t, e, run).Messages) != before {
		t.Fatal("частичная запись отказа")
	}
}
