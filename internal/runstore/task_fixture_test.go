package runstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// TestWriteTaskBoardFixture создаёт синтетический архив с карточкой и ответом.
// Оба актёра blocked: даже тестовая адресная реплика не запускает модель.
// LAWA_TASK_BOARD_FIXTURE_ROOT=/tmp/empty go test ./internal/runstore -run '^TestWriteTaskBoardFixture$' -v
func TestWriteTaskBoardFixture(t *testing.T) {
	root := os.Getenv("LAWA_TASK_BOARD_FIXTURE_ROOT")
	if root == "" {
		t.Skip("нужен явный временный root")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAWA_COMPACTION_FIXTURE_ROOT", root)
	TestWriteCompactionFixture(t)
	data, err := os.ReadFile(filepath.Join(root, "fixture-run-id"))
	if err != nil {
		t.Fatal(err)
	}
	run := string(data)
	err = UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		task := TeamTask{ID: "T-1", Assignee: "developer", Text: "Проверить связь результата с поручением.\n\nИсходное сообщение находится в архиве. Это синтетический пример без запуска модели.", Card: &TaskCard{Title: "Связать сообщения с задачей", Expected: "Рабочий переход к исходному поручению", Criteria: []string{"Архив открывается", "Фокус находится на исходном сообщении", "Ответ сохраняет taskId и replyTo"}, Revision: 2, Version: 5, Status: "in_review"}}
		initial := cloneTask(task)
		initial.Card.Revision = 1
		initial.Card.Status = "todo"
		chat.Messages[0].TaskID = "T-1"
		chat.Messages[0].TaskRevision = 1
		chat.Messages[0].TaskSnapshot = &initial
		chat.Messages[0].Text = "Исходное поручение: проверить переход из ответа в архив."
		last := &chat.Messages[len(chat.Messages)-1]
		last.TaskID = "T-1"
		last.TaskRevision = 2
		last.TaskSnapshot = &task
		last.ReplyTo = chat.Messages[0].ID
		last.Text = "Переход к исходному поручению готов. Проверьте архив, ответ и карточку задачи."
		chat.Room.TaskBoardVersion = 1
		chat.Room.Tasks = map[string]*TeamTask{"T-1": &task}
		chat.Room.Actors["boss"].Status = "blocked"
		chat.Room.Actors["developer"] = &TeamActor{Status: "blocked", Cursor: len(chat.Messages)}
		chat.Room.Catalog["developer"] = workflow.Character{Name: "Программист", History: "Синтетический пример", Instructions: "Не запускать модель"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Карточка: /office?run=%s; все актёры blocked", run)
}
