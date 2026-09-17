package teamruntime

import (
	"encoding/json/v2"
	"errors"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Старое прошлое восстанавливается только из известных фактов. Сотрудник не
// появляется до приглашения, финал не опережает реплику внутри той же секунды.
func TestRecoveredFrames(t *testing.T) {
	started, completed := int64(100), int64(110)
	chat := runstore.TeamChat{Messages: []runstore.TeamMessage{
		{ID: "goal", AuthorID: "human", Date: time.Unix(99, 0)},
		{ID: "summon-developer", Kind: "system", Date: time.Unix(102, 0)},
		{ID: "task", AuthorID: "boss", To: "developer", Text: "@developer Сделай игру", Date: time.Unix(110, 500000000)},
	}}
	frames := recoveredFrames(chat, map[string][]codex.HistoricalTurn{"boss": {{Status: "completed", StartedAt: &started, CompletedAt: &completed}}}, time.Unix(120, 0))
	if frames[0].Actors["boss"].Status != "unknown" {
		t.Fatal(frames[0])
	}
	var working, summoned, reply, finished bool
	for _, f := range frames {
		if f.At.Before(time.Unix(102, 0)) && len(f.Actors) > 1 {
			t.Fatal("сотрудник из будущего")
		}
		working = working || f.Actors["boss"].Status == "working"
		summoned = summoned || len(f.Actors) == 2
		reply = reply || (f.MessageCount == 3 && f.Actors["boss"].Summary == "Сделай игру")
		finished = finished || (f.At.Equal(time.Unix(111, 0)) && f.Actors["boss"].Status == "monitoring")
	}
	if !working || !summoned || !reply || !finished {
		t.Fatalf("неполная история: %+v", frames)
	}
	// Без timestamp статус не выдумывается, и cutoff не пропускает поздние факты.
	frames = recoveredFrames(chat, map[string][]codex.HistoricalTurn{"boss": {{Status: "completed"}}}, time.Unix(101, 0))
	if frames[len(frames)-1].MessageCount != 1 || frames[len(frames)-1].Actors["boss"].Status != "unknown" {
		t.Fatal(frames)
	}
}

// Импорт не меняет рабочие курсоры и доставку, сохраняет нативные кадры и при
// повторном запросе вообще не читает Codex. Это защита от повторного исполнения.
func TestRecoverHistoryPreservesRuntime(t *testing.T) {
	e, run, _, _ := teamEngine(t)
	before := readChat(t, e, run)
	if err := RecoverHistory(t.Context(), e.Root, run, func(string) ([]codex.HistoricalTurn, error) {
		t.Fatal("у новой команды нет thread")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	after := readChat(t, e, run)
	if !after.History.Recovered || !reflect.DeepEqual(before.Room, after.Room) || !reflect.DeepEqual(before.Messages, after.Messages) || !reflect.DeepEqual(before.History.Frames, after.History.Frames) {
		t.Fatal("импорт изменил runtime")
	}
	if err := RecoverHistory(t.Context(), e.Root, run, func(string) ([]codex.HistoricalTurn, error) {
		t.Fatal("повторный импорт")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Сохранение новой команды — единый журнал: приглашение и сообщение видны
// в одном кадре. Агентам эти кадры не выдаются при чтении общей памяти.
func TestNativeHistoryAndAgentContext(t *testing.T) {
	e, run, _, _ := teamEngine(t)
	if err := e.change(t.Context(), run, "boss", func(a *runstore.TeamActor) { a.Status = "working" }); err != nil {
		t.Fatal(err)
	}
	if err := runstore.SummonDeveloper(t.Context(), e.Root, run, "boss"); err != nil {
		t.Fatal(err)
	}
	chat := readChat(t, e, run)
	frames := chat.History.Frames
	if len(frames) != 3 || len(frames[0].Actors) != 1 || len(frames[2].Actors) != 2 || frames[2].MessageCount != 2 {
		t.Fatal(frames)
	}
	shared, err := runstore.ReadTeamForAgent(e.Root, run)
	if err != nil || shared.History != nil || len(shared.Messages) != 2 {
		t.Fatalf("контекст: %+v %v", shared, err)
	}
}

// Legacy-файл не содержит кадров. Неудачное чтение Codex ничего не пишет;
// успешное добавляет прошлое перед честной точкой начала нативной записи.
func TestRecoverLegacyHistory(t *testing.T) {
	e, run, _, _ := teamEngine(t)
	before := readChat(t, e, run)
	before.History = nil
	before.Messages[0].Date = time.Now().UTC().Add(-time.Hour)
	before.Room.Actors["boss"].ThreadID = "boss-thread"
	data, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Root, run, "team.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = RecoverHistory(t.Context(), e.Root, run, func(string) ([]codex.HistoricalTurn, error) { return nil, errors.New("нет Codex") }); err == nil {
		t.Fatal("потеряна ошибка")
	}
	if readChat(t, e, run).History != nil {
		t.Fatal("частично записанный импорт")
	}
	started, completed := before.Messages[0].Date.Unix()+1, before.Messages[0].Date.Unix()+10
	if err = RecoverHistory(t.Context(), e.Root, run, func(id string) ([]codex.HistoricalTurn, error) {
		if id != "boss-thread" {
			t.Fatal(id)
		}
		return []codex.HistoricalTurn{{Status: "completed", StartedAt: &started, CompletedAt: &completed}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	after := readChat(t, e, run)
	h := after.History
	if h == nil || !h.Recovered || len(h.Frames) < 4 || !h.Frames[len(h.Frames)-1].At.Equal(h.RecordedFrom) || !reflect.DeepEqual(before.Room, after.Room) || !reflect.DeepEqual(before.Messages, after.Messages) {
		t.Fatal("нарушена граница миграции")
	}
}
