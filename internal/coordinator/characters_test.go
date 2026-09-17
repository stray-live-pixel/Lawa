package coordinator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// TestCharacterMemoryAcrossSteps проверяет реальное сохранение памяти и права
// в обоих runtime: один персонаж не пишет из двух чатов одновременно, другая
// личность работает параллельно, а следующее поручение видит прежний опыт.
func TestCharacterMemoryAcrossSteps(t *testing.T) {
	for _, version := range []string{"legacy", "v2"} {
		t.Run(version, func(t *testing.T) {
			prefix, dependency := "", `"dependsOn":[]`
			if version == "v2" {
				prefix, dependency = `"version":2,"start":["first","second","other"],`, `"after":[]`
			}
			root, initial, run := createAgentPreparationRun(t, `{`+prefix+`"id":"people","characters":{
"alice":{"name":"Алиса","history":"Исследователь","instructions":"Проверяй факты"},
"bob":{"name":"Боб","history":"Разработчик","instructions":"Проверяй результат"}},"steps":[
{"id":"first","character":"alice","type":"agent","prompt":"Исследуй",`+dependency+`},
{"id":"second","character":"alice","type":"agent","prompt":"Примени опыт",`+dependency+`},
{"id":"other","character":"bob","type":"agent","prompt":"Разработай",`+dependency+`}]}`)
			root, _ = filepath.EvalSymlinks(root)
			// Адаптер оставляет в проверках общий наблюдаемый контракт, но вызывает
			// настоящий механизм резервирования legacy или посещений version 2.
			prepare := func() map[string]codex.Command {
				t.Helper()
				commands := map[string]codex.Command{}
				if version == "legacy" {
					prepared, err := Prepare(run, root)
					if err != nil {
						t.Fatal(err)
					}
					for _, work := range prepared.Launches {
						commands[work.StepID] = work.Command
						_ = work.lease.Release()
					}
				} else {
					prepared, err := prepareAgentVisits(run, root, nil, false, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					for _, work := range prepared.Work {
						commands[work.StepID] = work.Command
						_ = work.lease.Release()
					}
				}
				return commands
			}
			first := prepare()
			if len(first) != 2 || first["first"].Text == "" || first["other"].Text == "" {
				t.Fatalf("нарушена последовательность личности или параллельность разных: %v", first)
			}
			memory := filepath.Join(root, initial.Meta.RunID, "memory", "character-alice.md")
			if got := first["first"].Permissions.WritePaths; len(got) != 2 || got[1] != memory || !strings.Contains(first["first"].Text, "Исследователь") {
				t.Fatalf("потеряны личность или её права: %v", got)
			}
			if err := os.WriteFile(memory, []byte("Проверенный факт"), 0o600); err != nil {
				t.Fatal(err)
			}
			if pending := prepare(); len(pending) != 0 {
				t.Fatal("второй чат той же личности запущен до завершения первого")
			}
			if version == "legacy" {
				if err := run.Update("first", scheduler.Succeeded, "chat-alice"); err != nil {
					t.Fatal(err)
				}
			} else {
				completeAgentVisit(t, run, initial.Meta.Visits[0].VisitID, "chat-alice", scheduler.Succeeded, "")
			}
			second := prepare()
			if len(second) != 1 || !strings.Contains(second["second"].Text, memory) || second["second"].Permissions.WritePaths[1] != memory {
				t.Fatal("следующее поручение потеряло личность или путь памяти")
			}
			if data, err := os.ReadFile(memory); err != nil || string(data) != "Проверенный факт" {
				t.Fatalf("память личности сброшена: %q, %v", data, err)
			}
		})
	}
}

// TestCharacterContinuationReservation воспроизводит окно до turn/started:
// первый continue уже отправлен, но на диске ещё Cancelled. Второе поручение
// той же личности не должно запускаться из-за задержки серверного события.
func TestCharacterContinuationReservation(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
"version":2,"id":"resume-person","start":["first","second"],
"characters":{"alice":{"name":"Алиса","history":"Исследователь","instructions":"Проверяй факты"}},
"steps":[{"id":"first","character":"alice","type":"agent","prompt":"Начни","after":[]},
{"id":"second","character":"alice","type":"agent","prompt":"Продолжи","after":[]}]}`)
	first := initial.Meta.Visits[0].VisitID
	if err := run.ReserveVisits([]string{first}); err != nil {
		t.Fatal(err)
	}
	completeAgentVisit(t, run, first, "chat", scheduler.Cancelled, "Остановлен")
	prepared, err := prepareAgentVisits(run, root, nil, true, map[string]bool{}, nil)
	if err != nil || len(prepared.Work) != 1 || prepared.Work[0].VisitID != first {
		t.Fatalf("не подготовлено продолжение: %+v, %v", prepared, err)
	}
	defer prepared.Work[0].lease.Release()
	next, err := prepareAgentVisits(run, root, nil, true, map[string]bool{first: true}, nil)
	if err != nil || len(next.Work) != 0 {
		t.Fatalf("запущен конкурентный писатель памяти: %+v, %v", next, err)
	}
}
