package teamreport

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// fixture фиксирует параллельные интервалы: 10 + 10 секунд работы, из них
// 3 секунды разрешения у первого сотрудника. Календарное покрытие — 12 секунд.
func fixture() runstore.TeamChat {
	start := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	at := func(n int) *time.Time { t := start.Add(time.Duration(n) * time.Second); return &t }
	return runstore.TeamChat{RunID: "run", Goal: "secret goal", Metrics: &runstore.TeamMetrics{RecordedFrom: start, Executions: []*runstore.TeamExecution{
		{ID: "a/1", ActorID: "a", MessageIDs: []string{"task"}, ClaimedAt: at(1), StartedAt: at(2), FinishedAt: at(12)},
		{ID: "b/1", ActorID: "b", ClaimedAt: at(2), StartedAt: at(5), FinishedAt: at(15)},
	}}, Messages: []runstore.TeamMessage{
		{ID: "task", Date: start, Text: "secret task"},
		{ID: "r1", Date: *at(12), Kind: "task_result", TaskIDs: []string{"task"}},
		{ID: "back", Date: *at(14), Kind: "task_rework", TaskIDs: []string{"task"}},
		{ID: "r2", Date: *at(18), Kind: "task_result", TaskIDs: []string{"task"}},
		{ID: "accepted", Date: *at(20), Kind: "task_accepted", TaskIDs: []string{"task"}},
	}, Room: &runstore.TeamRoom{AchievedAt: at(20), Tasks: map[string]*runstore.TeamTask{"task": {ID: "task", Assignee: "a", AcceptedAt: at(20), ResultID: "r2"}}}, History: &runstore.TeamHistory{RecordedFrom: start, Frames: []runstore.TeamFrame{
		{At: *at(4), Actors: map[string]runstore.TeamActorView{"a": {Wait: &runstore.TeamWait{Kind: "permission", Source: "runtime", Text: "secret error"}}}},
		{At: *at(7), Actors: map[string]runstore.TeamActorView{}},
		{At: *at(15), Actors: map[string]runstore.TeamActorView{"b": {Wait: &runstore.TeamWait{Kind: "result", Source: "actor"}}}},
		{At: *at(20), Actors: map[string]runstore.TeamActorView{}},
	}}}
}

// Пересечение сотрудников не превращается в календарную сумму. Причина ожидания
// остаётся типом зависимости, а тексты сообщений/ошибок не уходят в отчёт.
func TestParallelResultAndWaitReport(t *testing.T) {
	chat := fixture()
	r := Build(chat, time.Time{})
	if r.ActiveAgentSeconds == nil || *r.ActiveAgentSeconds != 17 || r.KnownActiveCalendarSeconds != 12 || *r.CalendarSeconds != 20 {
		t.Fatalf("время: %+v", r)
	}
	task := r.Tasks[0]
	if *task.FirstResultSeconds != 12 || *task.AcceptedSeconds != 20 || *task.Reworks != 1 || *r.FirstResultSeconds != 12 || *r.FirstAcceptedSeconds != 20 {
		t.Fatalf("результат: %+v", task)
	}
	if len(r.Waits) != 2 || r.Waits[1].Kind != "result" || r.Waits[1].Seconds != 5 {
		t.Fatal(r.Waits)
	}
	if *r.Executions[0].Deliveries[0].DelaySeconds != 2 {
		t.Fatal(r.Executions)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || !strings.Contains(string(data), `"price":null`) || !strings.Contains(string(data), `"usage":null`) {
		t.Fatal(string(data))
	}
	again, _ := json.Marshal(Build(chat, time.Time{}))
	if string(data) != string(again) {
		t.Fatal("нестабильный отчёт")
	}
}

// Старый файл и потерянное окончание не становятся нулём или длинным turn до
// момента перезапуска. Известные интервалы всё ещё доступны как частичный итог.
func TestLegacyAndUnknownFinish(t *testing.T) {
	chat := fixture()
	chat.Metrics.Executions[0].FinishedAt = nil
	end := chat.Messages[len(chat.Messages)-1].Date.Add(time.Hour)
	chat.Metrics.Executions[0].ObservedFinishedAt = &end
	r := Build(chat, time.Time{})
	if r.ActiveAgentSeconds != nil || r.UnknownExecutions != 1 || r.KnownActiveAgentSeconds != 10 {
		t.Fatalf("неизвестное: %+v", r)
	}
	chat.Metrics = nil
	r = Build(chat, time.Time{})
	if r.ActiveAgentSeconds != nil || r.RecordedFrom != nil || r.Tasks[0].Reworks != nil || r.Executions == nil {
		t.Fatalf("legacy: %+v", r)
	}
}

// Явный срез не должен раскрывать будущие границы или итог приёмки. Пустое
// чтение не дописывает растущий интервал и не меняет входной снимок.
func TestAsOfClipsFutureAndDoesNotMutate(t *testing.T) {
	chat := fixture()
	before, _ := json.Marshal(chat)
	r := Build(chat, chat.Messages[0].Date.Add(10*time.Second))
	if r.Executions[0].FinishedAt != nil || r.Tasks[0].AcceptedAt != nil || r.Tasks[0].FirstResultAt != nil || r.ActiveAgentSeconds != nil {
		t.Fatalf("будущее: %+v", r)
	}
	after, _ := json.Marshal(chat)
	if string(before) != string(after) {
		t.Fatal("изменён вход")
	}
}

// Повторные кадры и восстановленные статусы до нативной границы не прибавляют
// ожидание. Неполное покрытие старого запуска не превращается в полный итог.
func TestWaitCoverageAndClockRollback(t *testing.T) {
	chat := fixture()
	chat.History.RecordedFrom = chat.History.Frames[1].At
	r := Build(chat, time.Time{})
	if len(r.Waits) != 1 {
		t.Fatal(r.Waits)
	}
	chat.Metrics.RecordedFrom = chat.Messages[0].Date.Add(time.Second)
	chat.Metrics.Executions[0].FinishedAt = chat.Metrics.Executions[0].ClaimedAt
	r = Build(chat, time.Time{})
	if r.ActiveAgentSeconds != nil || r.Executions[0].ActiveSeconds != nil {
		t.Fatal(r)
	}
}
