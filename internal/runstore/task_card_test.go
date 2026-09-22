package runstore

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// boardFixture использует обычную директорию и пустой PATH. Ни один сценарий
// карточки не должен требовать VCS, GitHub или запускать внешнюю программу.
func boardFixture(t *testing.T) (string, Snapshot) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	s := teamRun(t, root, "", "Проверить доску без VCS")
	err := UpdateTeam(t.Context(), root, s.Meta.RunID, func(chat *TeamChat) error {
		initializeRoom(chat, workflow.DefaultTeamCharacters())
		chat.Members["developer"] = TeamMember{Name: "Программист"}
		chat.Room.Actors["developer"] = &TeamActor{Status: "working", Delivery: &TeamDelivery{IDs: []string{"initial-goal"}, End: 1}}
		chat.Room.Actors["boss"].Status = "working"
		chat.Room.Actors["boss"].Delivery = &TeamDelivery{IDs: []string{"initial-goal"}, End: 1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return root, s
}

// boardCommand читает актуальную Version только для последовательного happy path.
// Конкурентные тесты ниже передают устаревшую версию явно.
func boardCommand(t *testing.T, root, run, author, action, id string, configure func(*TaskCommand)) TeamTask {
	t.Helper()
	chat, err := ReadTeam(root, run)
	if err != nil {
		t.Fatal(err)
	}
	in := TaskCommand{ID: fmt.Sprintf("%s-%s-%d", id, action, len(chat.Messages)), Action: action, TaskID: id}
	if task := chat.Room.Tasks[id]; task != nil && task.Card != nil {
		in.Version = task.Card.Version
	}
	if configure != nil {
		configure(&in)
	}
	task, err := ApplyTaskCommand(t.Context(), root, run, author, in)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return task
}

// boardCreate создаёт задачу, а не извлекает её из обычного сообщения.
func boardCreate(t *testing.T, root, run, id string) TeamTask {
	return boardCommand(t, root, run, "boss", "create", id, func(in *TaskCommand) {
		in.Title = "Проверка " + id
		in.Body = "Создать проверяемый файл"
		in.Expected = "Готовый файл"
		in.Criteria = []string{"Файл содержит результат"}
		in.Assignee = "developer"
	})
}

// boardWork подтверждает конкретные требования раздельными операциями.
func boardWork(t *testing.T, root, run, id string) {
	for _, action := range []string{"read", "acknowledge", "start"} {
		boardCommand(t, root, run, "developer", action, id, nil)
	}
}

// Полный цикл с возвратом проверяет автора, файловую основу и приёмку Боссом.
// Независимый файл не инвалидирует проверку; связанный запрещает приёмку.
func TestTaskBoardLifecycleWithoutVCS(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "T1")
	boardWork(t, root, run, "T1")
	path := filepath.Join(s.Meta.CWD, "result.txt")
	if err := os.WriteFile(path, []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	report := func() {
		boardCommand(t, root, run, "developer", "report", "T1", func(in *TaskCommand) {
			in.Result = &TaskResult{Summary: "Файл готов", Artifacts: []string{"result.txt"}, Checks: []string{"Прочитан"}, Limitations: "Проверено без VCS"}
		})
	}
	report()
	boardCommand(t, root, run, "boss", "return", "T1", func(in *TaskCommand) { in.Reason = "Добавь вторую строку" })
	boardWork(t, root, run, "T1")
	report()
	chat, _ := ReadTeam(root, run)
	result := chat.Room.Tasks["T1"].Card.Results[1]
	boardCommand(t, root, run, "boss", "review", "T1", func(in *TaskCommand) {
		in.ResultID = result.ID
		in.Verdict = "approve"
		in.Reason = "Содержимое проверено"
	})
	if err := UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		chat.Room.Actors["developer"].Delivery = nil
		chat.Room.Actors["developer"].Status = "idle"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(s.Meta.CWD, "unrelated.txt"), []byte("не влияет"), 0600)
	boardCommand(t, root, run, "boss", "accept", "T1", func(in *TaskCommand) {
		in.ResultID = result.ID
		in.Reason = "Файл соответствует критерию"
		in.Integration = "Результат доступен в result.txt"
	})
	_ = os.WriteFile(path, []byte("changed"), 0600)
	page, err := ReadTasks(root, run, TaskReadOptions{TaskID: "T1"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Task.Card.Status != "in_review" || page.Task.Card.StaleReason == "" || page.Task.AcceptedAt == nil {
		t.Fatalf("потеряна актуальность/история: %+v", page.Task)
	}
	chat, _ = ReadTeam(root, run)
	if len(chat.Room.Tasks) != 1 || len(chat.Room.Tasks["T1"].Card.Results) != 2 {
		t.Fatal("созданы лишние задачи или потеряны результаты")
	}
}

// Повтор запроса после изменения состояния возвращает прежнее подтверждение;
// конкурентные операции одной Version сохраняют ровно одну запись.
func TestTaskBoardRetryConcurrencyAndRights(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	task := boardCreate(t, root, run, "T1")
	in := TaskCommand{ID: "priority", Action: "priority", TaskID: "T1", Version: task.Card.Version, Priority: 10}
	first, err := ApplyTaskCommand(t.Context(), root, run, "boss", in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ApplyTaskCommand(t.Context(), root, run, "boss", in)
	firstJSON, _ := json.Marshal(first)
	againJSON, _ := json.Marshal(again)
	if err != nil || string(firstJSON) != string(againJSON) {
		t.Fatal("повтор изменил результат", err)
	}
	in.Priority = 20
	if _, err = ApplyTaskCommand(t.Context(), root, run, "boss", in); err == nil {
		t.Fatal("изменённый retry принят")
	}
	in.ID = "forbidden"
	in.Version = first.Card.Version
	if _, err = ApplyTaskCommand(t.Context(), root, run, "developer", in); err == nil {
		t.Fatal("исполнитель изменил приоритет")
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() {
			request := in
			request.ID = fmt.Sprint("race-", i)
			_, err := ApplyTaskCommand(t.Context(), root, run, "boss", request)
			out <- err
		})
	}
	wg.Wait()
	close(out)
	success := 0
	for err := range out {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("конкурентная запись", success)
	}
}

// Отмена не означает done; завершение запрещено, пока ход исполнителя активен.
func TestTaskBoardCancellationAndRevision(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "T1")
	boardWork(t, root, run, "T1")
	boardCommand(t, root, run, "boss", "edit", "T1", func(in *TaskCommand) {
		in.Title = "Новые условия"
		in.Body = "Новый текст"
		in.Expected = "Другой результат"
		in.Criteria = []string{"Новый критерий"}
	})
	chat, _ := ReadTeam(root, run)
	task := chat.Room.Tasks["T1"]
	if task.Card.Acknowledged != 0 || task.Card.Revision != 2 || task.Card.History[len(task.Card.History)-1].PreviousBody == "" {
		t.Fatal("уточнение потеряло историю")
	}
	_, err := ApplyTaskCommand(t.Context(), root, run, "developer", TaskCommand{ID: "old-report", TaskID: "T1", Action: "report", Version: task.Card.Version, Result: &TaskResult{Summary: "старый результат"}})
	if err == nil {
		t.Fatal("принят старый результат")
	}
	boardCommand(t, root, run, "boss", "cancel", "T1", func(in *TaskCommand) { in.Reason = "Потеряла актуальность" })
	boardCommand(t, root, run, "developer", "cancel_evidence", "T1", func(in *TaskCommand) {
		in.Reason = "Файлы сохранены архивом, вклад не включён"
	})
	chat, _ = ReadTeam(root, run)
	_, err = ApplyTaskCommand(t.Context(), root, run, "boss", TaskCommand{ID: "early-finish", TaskID: "T1", Action: "cancel_finish", Version: chat.Room.Tasks["T1"].Card.Version, Reason: "Проверено"})
	if err == nil {
		t.Fatal("отмена завершена при работающем исполнителе")
	}
	_ = UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		chat.Room.Actors["developer"].Delivery = nil
		chat.Room.Actors["developer"].Status = "idle"
		return nil
	})
	taskValue := boardCommand(t, root, run, "boss", "cancel_finish", "T1", func(in *TaskCommand) { in.Reason = "Проверен архив и отсутствие вклада" })
	if taskValue.Card.Cancellation != "cancelled" || taskValue.Card.Status == "done" || taskValue.AcceptedAt != nil {
		t.Fatal("отмена принята за успех")
	}
}

// Scheduler ждёт приёмки зависимости, сортирует равные приоритеты по ID и
// не выдаёт карточку повторно при восстановлении delivery.
func TestTaskBoardDependenciesAndDispatch(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "A")
	boardCreate(t, root, run, "B")
	boardCreate(t, root, run, "C")
	boardCommand(t, root, run, "boss", "dependencies", "B", func(in *TaskCommand) { in.Dependencies = []string{"A"} })
	chat, _ := ReadTeam(root, run)
	_, err := ApplyTaskCommand(t.Context(), root, run, "boss", TaskCommand{ID: "cycle", Action: "dependencies", TaskID: "A", Version: chat.Room.Tasks["A"].Card.Version, Dependencies: []string{"B"}})
	if err == nil {
		t.Fatal("цикл принят")
	}
	boardCommand(t, root, run, "boss", "priority", "C", func(in *TaskCommand) { in.Priority = 10 })
	_ = UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		a := chat.Room.Actors["developer"]
		a.Delivery = nil
		a.Status = "idle"
		a.NextCheck = time.Now().Add(-time.Second)
		return nil
	})
	claimed, err := ClaimTeamDelivery(t.Context(), root, run, "developer", time.Now())
	if !claimed || err != nil {
		t.Fatal(claimed, err)
	}
	chat, _ = ReadTeam(root, run)
	if chat.Room.Actors["developer"].Delivery.TaskID != "C" {
		t.Fatal("нарушен приоритет")
	}
	if again, err := ClaimTeamDelivery(t.Context(), root, run, "developer", time.Now()); again || err != nil {
		t.Fatal("дубликат доставки", err)
	}
	if taskReady(&chat, chat.Room.Tasks["B"]) {
		t.Fatal("зависимая задача запущена до приёмки")
	}
}

// Сервер не читает внешние пути и не теряет архивную связь после сжатия.
func TestTaskArtifactsAndLinkedChat(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "T1")
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("private"), 0600)
	_ = os.Symlink(outside, filepath.Join(s.Meta.CWD, "link"))
	for _, path := range []string{outside, "../secret", "link"} {
		if _, err := CaptureTaskBasis(s.Meta.CWD, []string{path}); err == nil {
			t.Fatalf("прочитан запрещённый путь %s", path)
		}
	}
	m, err := PostLinkedActor(t.Context(), root, run, "developer", "note", "Общее сообщение", TeamMessageLinks{})
	if err != nil || m.To != "" {
		t.Fatal(err)
	}
	reply, err := PostLinkedActor(t.Context(), root, run, "human", "reply", "Комментарий", TeamMessageLinks{TaskID: "T1", ReplyTo: "T1-create-1"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ReplyTo == "" {
		t.Fatal("потерян ответ")
	}
	page, err := ReadTasks(root, run, TaskReadOptions{TaskID: "T1", Section: "messages", Limit: 1})
	if err != nil || page.NextOffset == nil {
		t.Fatal("нет продолжения обсуждения", err)
	}
	dataBefore, _ := os.ReadFile(filepath.Join(root, run, "team.json"))
	_, _ = ReadTasks(root, run, TaskReadOptions{TaskID: "T1"})
	dataAfter, _ := os.ReadFile(filepath.Join(root, run, "team.json"))
	if string(dataBefore) != string(dataAfter) {
		t.Fatal("GET изменил историю")
	}
	if strings.Contains(string(dataAfter), "private") {
		t.Fatal("утечка файла")
	}
}

// После приёмки отмена сохраняет исторический результат, но исключает зависимые
// задачи из готовых. Одновременные report/cancel не могут закрыть отменённую работу.
func TestTaskCancellationRaceAndAcceptedDependency(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "A")
	boardCreate(t, root, run, "B")
	boardCommand(t, root, run, "boss", "dependencies", "B", func(in *TaskCommand) { in.Dependencies = []string{"A"} })
	boardWork(t, root, run, "A")
	chat, _ := ReadTeam(root, run)
	version := chat.Room.Tasks["A"].Card.Version
	var wg sync.WaitGroup
	out := make(chan error, 2)
	wg.Go(func() {
		_, err := ApplyTaskCommand(t.Context(), root, run, "boss", TaskCommand{ID: "cancel-race", Action: "cancel", TaskID: "A", Version: version, Reason: "Больше не нужно"})
		out <- err
	})
	wg.Go(func() {
		_, err := ApplyTaskCommand(t.Context(), root, run, "developer", TaskCommand{ID: "report-race", Action: "report", TaskID: "A", Version: version, Result: &TaskResult{Summary: "Готово", Links: []string{"https://example.invalid/result"}, Checks: []string{"Проверена ссылка"}, Limitations: "Внешний ресурс не хэшируется сервером"}})
		out <- err
	})
	wg.Wait()
	close(out)
	success := 0
	for err := range out {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("оба перехода сохранились", success)
	}
	chat, _ = ReadTeam(root, run)
	if chat.Room.Tasks["A"].Card.Cancellation == "" {
		boardCommand(t, root, run, "boss", "cancel", "A", func(in *TaskCommand) { in.Reason = "Больше не нужно" })
	}
	chat, _ = ReadTeam(root, run)
	if taskReady(&chat, chat.Room.Tasks["B"]) {
		t.Fatal("отменённая зависимость считается готовой")
	}
	if chat.Room.Tasks["B"].Card.StaleReason == "" {
		t.Fatal("нет причины перепланирования")
	}
}

// Независимые карточки действительно могут резервироваться одновременно разными
// сотрудниками. Один actor по-прежнему защищён собственным lock runtime.
func TestTaskIndependentClaims(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	err := UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		chat.Room.Catalog["tester"] = workflow.Character{Name: "Проверяющий", History: "Тест", Instructions: "Проверяй"}
		chat.Room.Actors["tester"] = &TeamActor{Status: "idle", NextCheck: time.Now()}
		chat.Members["tester"] = TeamMember{Name: "Проверяющий"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	boardCreate(t, root, run, "A")
	boardCreate(t, root, run, "B")
	boardCommand(t, root, run, "boss", "assign", "B", func(in *TaskCommand) { in.Assignee = "tester" })
	_ = UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		chat.Room.Actors["developer"].Delivery = nil
		chat.Room.Actors["developer"].Status = "idle"
		chat.Room.Actors["developer"].NextCheck = time.Now()
		return nil
	})
	var wg sync.WaitGroup
	for _, actor := range []string{"developer", "tester"} {
		wg.Go(func() {
			ok, err := ClaimTeamDelivery(t.Context(), root, run, actor, time.Now().Add(time.Second))
			if err != nil {
				t.Errorf("%s: %v %v", actor, ok, err)
			}
		})
	}
	wg.Wait()
	for _, actor := range []string{"developer", "tester"} {
		// Повторный тик после резервирования более приоритетной выдачи.
		_, err := ClaimTeamDelivery(t.Context(), root, run, actor, time.Now().Add(10*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
	chat, _ := ReadTeam(root, run)
	if chat.Room.Actors["developer"].Delivery.TaskID != "A" || chat.Room.Actors["tester"].Delivery.TaskID != "B" {
		t.Fatal("неверное назначение")
	}
}

// Удаление/изменение проверенного файла отклоняет приёмку, а повтор уже
// выполненной команды не перечитывает файл и не создаёт новую запись.
func TestTaskRejectsStaleEvidenceAndRetriesReport(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "T1")
	boardWork(t, root, run, "T1")
	path := filepath.Join(s.Meta.CWD, "artifact")
	_ = os.WriteFile(path, []byte("checked"), 0600)
	chat, _ := ReadTeam(root, run)
	request := TaskCommand{ID: "report", Action: "report", TaskID: "T1", Version: chat.Room.Tasks["T1"].Card.Version, Result: &TaskResult{Summary: "Готово", Artifacts: []string{"artifact"}, Checks: []string{"Прочитан"}, Limitations: "Без VCS"}}
	if _, err := ApplyTaskCommand(t.Context(), root, run, "developer", request); err != nil {
		t.Fatal(err)
	}
	boardCommand(t, root, run, "boss", "review", "T1", func(in *TaskCommand) {
		in.ResultID = "report"
		in.Verdict = "approve"
		in.Reason = "Проверено"
	})
	_ = os.WriteFile(path, []byte("different"), 0600)
	if _, err := ApplyTaskCommand(t.Context(), root, run, "developer", request); err != nil {
		t.Fatal("retry отчёта зависит от нового файла", err)
	}
	_ = UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		chat.Room.Actors["developer"].Delivery = nil
		chat.Room.Actors["developer"].Status = "idle"
		return nil
	})
	chat, _ = ReadTeam(root, run)
	_, err := ApplyTaskCommand(t.Context(), root, run, "boss", TaskCommand{ID: "accept", Action: "accept", TaskID: "T1", Version: chat.Room.Tasks["T1"].Card.Version, ResultID: "report", Reason: "Проверено", Integration: "Файл на месте"})
	if err == nil || !strings.Contains(err.Error(), "устарела") {
		t.Fatal("приняты изменённые доказательства", err)
	}
}

// Пустой map может отсутствовать в JSON из-за omitempty. Новая комната при
// повторном чтении всё равно не должна превращать обычные реплики в поручения.
func TestTaskBoardGeneralMessagesNeverBecomeTasks(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	for _, message := range []struct{ author, id, text string }{{"boss", "question", "@developer Как дела?"}, {"human", "human-question", "@developer Уточни срок"}, {"developer", "general", "Общий факт"}} {
		if _, err := PostActor(t.Context(), root, run, message.author, message.id, message.text); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		chat, err := ReadTeam(root, run)
		if err != nil || len(chat.Room.Tasks) != 0 {
			t.Fatal("обычные сообщения стали поручениями", err)
		}
		for _, m := range chat.Messages {
			if m.ID == "general" && (m.ReplyTo != "" || m.To != "" || m.TaskID != "") {
				t.Fatal("общему посту выдумана связь")
			}
		}
	}
}

// Явное уточнение legacy-поручения сохраняет его ID, исходный текст и прошлую
// приёмку. Version 0 здесь означает отсутствие прежней проверенной revision.
func TestTaskLegacyExplicitRevision(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	at := time.Now().UTC()
	_ = UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		chat.Room.TaskBoardVersion = 0
		chat.Room.Tasks["old"] = &TeamTask{ID: "old", Assignee: "developer", Text: "Старый текст", AcceptedAt: &at, ResultID: "old-result", Evidence: "Историческая проверка"}
		return nil
	})
	task, err := ApplyTaskCommand(t.Context(), root, run, "boss", TaskCommand{ID: "revise-old", Action: "edit", TaskID: "old", Version: 0, Title: "Актуальная задача", Body: "Новый текст", Expected: "Новый результат", Criteria: []string{"Проверить заново"}})
	if err != nil || task.ID != "old" || task.Card.Revision != 1 || task.Card.Status != "todo" || task.AcceptedAt == nil {
		t.Fatal("потеряна совместимость", err)
	}
	chat, _ := ReadTeam(root, run)
	history := chat.Room.Tasks["old"].Card.History
	if len(history) != 1 || history[0].FromRevision != 0 || history[0].PreviousBody != "Старый текст" || history[0].Previous.Card != nil {
		t.Fatal("выдумана старая revision")
	}
}

// Босс тоже отвечает по явной ссылке: служебный /reply предназначен только
// сотрудникам и не должен блокировать новый структурированный контракт.
func TestTaskBossLinkedReplyAndExactRetry(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "T1")
	links := TeamMessageLinks{TaskID: "T1", ReplyTo: "T1-create-1"}
	first, err := PostLinkedActor(t.Context(), root, run, "boss", "boss-reply", "@developer Уточнение без новой задачи", links)
	if err != nil {
		t.Fatal(err)
	}
	again, err := PostLinkedActor(t.Context(), root, run, "boss", "boss-reply", "@developer Уточнение без новой задачи", links)
	if err != nil || first.ID != again.ID {
		t.Fatal("повтор не идемпотентен", err)
	}
	chat, _ := ReadTeam(root, run)
	if len(chat.Room.Tasks) != 1 {
		t.Fatal("ответ создал задачу")
	}
}

// Блокер сохраняет основной статус, а снятие вновь ставит назначенную работу
// в очередь без ручного сообщения человеку или нового обязательства.
func TestTaskBlockerReschedulesWork(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "A")
	boardWork(t, root, run, "A")
	blocked := boardCommand(t, root, run, "developer", "block", "A", func(in *TaskCommand) { in.Reason = "Нужен входной файл" })
	if blocked.Card.Status != "in_progress" {
		t.Fatal("блокер потерял основной статус")
	}
	boardCommand(t, root, run, "boss", "block", "A", nil)
	chat, _ := ReadTeam(root, run)
	if !taskReady(&chat, chat.Room.Tasks["A"]) {
		t.Fatal("снятие блокера не создаёт готовность")
	}
}

// Блокер до start должен дойти до Босса даже после завершения хода сотрудника.
// Иначе курсор поглощает единственное уведомление, а повторный сигнал не создаётся.
func TestTaskTodoBlockerReachesBossAfterTurn(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "A")
	if err := UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		boss := chat.Room.Actors["boss"]
		boss.Delivery = nil
		boss.Status = "idle"
		boss.Cursor = len(chat.Messages)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	boardCommand(t, root, run, "developer", "block", "A", func(in *TaskCommand) { in.Reason = "Нужен входной файл" })
	state, err := ReadTeam(root, run)
	if err != nil {
		t.Fatal(err)
	}
	blockID := state.Messages[len(state.Messages)-1].ID
	if err := UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		developer := chat.Room.Actors["developer"]
		developer.Delivery = nil
		developer.Status = "idle"
		developer.Cursor = len(chat.Messages)
		NotifyTeamResultReady(chat, "developer", blockID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	claimed, err := ClaimTeamDelivery(t.Context(), root, run, "boss", now)
	if err != nil || !claimed {
		t.Fatalf("Босс не получил блокер: claimed=%v err=%v", claimed, err)
	}
	chat, err := ReadTeam(root, run)
	if err != nil {
		t.Fatal(err)
	}
	delivery := chat.Room.Actors["boss"].Delivery
	if len(delivery.IDs) != 1 || delivery.IDs[0] != blockID || delivery.TaskID != "" {
		t.Fatalf("ожидалось одно уведомление, не назначение задачи: %+v", delivery)
	}
	if claimed, err := ClaimTeamDelivery(t.Context(), root, run, "boss", now); err != nil || claimed {
		t.Fatalf("повторная доставка: %v, %v", claimed, err)
	}
	if claimed, err := ClaimTeamDelivery(t.Context(), root, run, "developer", now); err != nil || claimed {
		t.Fatalf("заблокированная работа выдана исполнителю: %v, %v", claimed, err)
	}
}

// Приёмка ждёт завершения хода своей задачи. Независимая следующая задача
// того же сотрудника не должна задерживать готовый результат и его зависимости.
func TestTaskAcceptWhileAssigneeWorksOnAnotherTask(t *testing.T) {
	root, s := boardFixture(t)
	run := s.Meta.RunID
	boardCreate(t, root, run, "A")
	boardCreate(t, root, run, "B")
	boardWork(t, root, run, "A")
	boardCommand(t, root, run, "developer", "report", "A", func(in *TaskCommand) {
		in.Result = &TaskResult{Summary: "Результат готов", Links: []string{"https://example.invalid/result"}, Checks: []string{"Проверен"}, Limitations: "Без файлов"}
	})
	state, err := ReadTeam(root, run)
	if err != nil {
		t.Fatal(err)
	}
	resultID := state.Room.Tasks["A"].Card.Results[0].ID
	boardCommand(t, root, run, "boss", "review", "A", func(in *TaskCommand) {
		in.ResultID, in.Verdict, in.Reason = resultID, "approve", "Результат проверен"
	})
	for _, taskID := range []string{"A", ""} {
		if err := UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
			chat.Room.Actors["developer"].Delivery.TaskID = taskID
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		chat, err := ReadTeam(root, run)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ApplyTaskCommand(t.Context(), root, run, "boss", TaskCommand{
			ID: "premature-accept-" + taskID, Action: "accept", TaskID: "A", Version: chat.Room.Tasks["A"].Card.Version,
			ResultID: resultID, Reason: "Проверено", Integration: "Результат учтён",
		})
		if err == nil {
			t.Fatal("приёмка до завершения своего или неопределённого хода")
		}
	}
	if err := UpdateTeam(t.Context(), root, run, func(chat *TeamChat) error {
		actor := chat.Room.Actors["developer"]
		actor.Delivery, actor.Status, actor.Cursor = nil, "idle", len(chat.Messages)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := ClaimTeamDelivery(t.Context(), root, run, "developer", time.Now().Add(time.Hour)); err != nil || !claimed {
		t.Fatalf("следующая задача не выдана: %v, %v", claimed, err)
	}
	chat, err := ReadTeam(root, run)
	if err != nil {
		t.Fatal(err)
	}
	if chat.Room.Actors["developer"].Delivery.TaskID != "B" {
		t.Fatal("ожидалась задача B")
	}
	accepted := boardCommand(t, root, run, "boss", "accept", "A", func(in *TaskCommand) {
		in.ResultID, in.Reason, in.Integration = resultID, "Проверено", "Результат учтён"
	})
	if accepted.Card.Status != "done" {
		t.Fatal("готовый результат не принят")
	}
}
