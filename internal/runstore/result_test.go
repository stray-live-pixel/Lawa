//go:build darwin || linux

package runstore

import (
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"os"
	"path/filepath"
	"testing"
)

// Итог становится свойством только вместе с terminal state, переживает закрытие
// writer и не зависит от последующего удаления журнала. Новый turn его очищает.
func TestLegacyResultProperty(t *testing.T) {
	root, initial, run := testLockedRun(t)
	id := initial.Meta.Steps[0].ID
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(run.Update(id, scheduler.Starting, ""))
	must(run.Update(id, scheduler.Running, "chat"))
	must(run.SetTurn(id, "turn-1"))
	text := "Итог: **готово**\n\n- Проверено"
	must(run.AppendEvent(RuntimeEvent{StepID: id, ThreadID: "chat", TurnID: "turn-1", Kind: "item_completed", ItemType: "agentMessage", Content: text}))
	active, err := run.Load()
	must(err)
	if active.Meta.Steps[0].Result != "" {
		t.Fatal("промежуточное сообщение стало итогом")
	}
	must(run.Update(id, scheduler.Succeeded, "chat"))
	final, err := Load(root, initial.Meta.RunID)
	must(err)
	if final.Meta.Steps[0].Result != text {
		t.Fatalf("result потерян: %q", final.Meta.Steps[0].Result)
	}
	must(os.Remove(filepath.Join(root, initial.Meta.RunID, eventsFilename)))
	must(run.Update(id, scheduler.Succeeded, "chat"))
	final, err = run.Load()
	must(err)
	if final.Meta.Steps[0].Result != text {
		t.Fatal("идемпотентный Update потерял durable result")
	}
	must(run.Update(id, scheduler.Running, "chat"))
	must(run.SetTurn(id, "turn-2"))
	must(run.AppendEvent(RuntimeEvent{StepID: id, ThreadID: "chat", TurnID: "turn-2", Kind: "agent_message_delta", ItemType: "agentMessage", Content: "Итог: черновик"}))
	must(run.Update(id, scheduler.Succeeded, "chat"))
	final, err = run.Load()
	must(err)
	if final.Meta.Steps[0].Result != "" {
		t.Fatal("delta или прошлая попытка выданы за новый итог")
	}
}

// Два независимых посещения могут завершаться вперемешку. Каждое получает свой
// Markdown по visit/turn, включая терминальную ошибку, и сохранённый файл читается
// после освобождения lock. Пропущенные кубики такого свойства не получают.
func TestVisitResultProperty(t *testing.T) {
	root, initial, run := testAgentGraphRun(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, visit := range initial.Meta.Visits {
		chat, turn := "chat-"+visit.VisitID, "turn-"+visit.VisitID
		must(run.ReserveVisits([]string{visit.VisitID}))
		must(run.UpdateVisit(visit.VisitID, scheduler.Unknown, chat, ""))
		must(run.SetVisitTurn(visit.VisitID, turn))
		must(run.UpdateVisit(visit.VisitID, scheduler.Running, chat, ""))
		text := "Итог: " + visit.VisitID
		must(run.AppendEvent(RuntimeEvent{VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: chat, TurnID: turn, Kind: "item_completed", ItemType: "agentMessage", Content: text}))
		must(run.UpdateVisit(visit.VisitID, scheduler.Failed, chat, "тестовая ошибка"))
		got, err := run.Load()
		must(err)
		if got.Meta.Visits[i].Result != text {
			t.Fatal("перепутан результат посещения")
		}
	}
	must(run.Close())
	got, err := Load(root, initial.Meta.RunID)
	must(err)
	for _, visit := range got.Meta.Visits {
		if visit.Result != "Итог: "+visit.VisitID {
			t.Fatal("result не сохранён в metadata")
		}
	}
}
