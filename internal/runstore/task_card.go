package runstore

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// TaskCard расширяет прежний TeamTask. Revision меняется при изменении условий,
// а Version — при любой операции: параллельные команды не теряют чужую запись.
// Историческая приёмка остаётся в TeamTask; Status описывает текущее обязательство.
type TaskCard struct {
	ScheduleKey          string         `json:"scheduleKey,omitempty"`
	DispatchedKey        string         `json:"dispatchedKey,omitempty"`
	Workspace            *TaskWorkspace `json:"workspace,omitempty"`
	Integration          string         `json:"integration,omitempty"`
	CancellationEvidence string         `json:"cancellationEvidence,omitempty"`
	StaleReason          string         `json:"staleReason,omitempty"`
	Title                string         `json:"title"`
	Expected             string         `json:"expected"`
	Criteria             []string       `json:"criteria"`
	Revision             uint64         `json:"revision"`
	Version              uint64         `json:"version"`
	Status               string         `json:"status"`
	Priority             int            `json:"priority"`
	Dependencies         []string       `json:"dependencies,omitempty"`
	Blocker              string         `json:"blocker,omitempty"`
	Cancellation         string         `json:"cancellation,omitempty"`
	CancelReason         string         `json:"cancelReason,omitempty"`
	Acknowledged         uint64         `json:"acknowledged,omitempty"`
	ReadRevision         uint64         `json:"readRevision,omitempty"`
	DeliveredRevision    uint64         `json:"deliveredRevision,omitempty"`
	Results              []TaskResult   `json:"results,omitempty"`
	Reviews              []TaskReview   `json:"reviews,omitempty"`
	History              []TaskChange   `json:"history"`
}

// TaskChange хранит автора и предыдущие требования, чтобы уточнение не стирало историю.
type TaskChange struct {
	Previous     *TeamTask `json:"previous,omitempty"`
	ID           string    `json:"id"`
	Author       string    `json:"author"`
	Action       string    `json:"action"`
	Date         time.Time `json:"date"`
	FromRevision uint64    `json:"fromRevision"`
	Revision     uint64    `json:"revision"`
	PreviousBody string    `json:"previousBody,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

// TaskResult — заявление исполнителя на конкретной основе, а не автоматический
// вывод из завершения turn. Проверка качества отдельно записывается в TaskReview.
type TaskResult struct {
	Links       []string   `json:"links,omitempty"`
	VersionRef  string     `json:"versionRef,omitempty"`
	Basis       *TaskBasis `json:"basis,omitempty"`
	ID          string     `json:"id"`
	Author      string     `json:"author"`
	Revision    uint64     `json:"revision"`
	Summary     string     `json:"summary"`
	Artifacts   []string   `json:"artifacts"`
	Checks      []string   `json:"checks"`
	Limitations string     `json:"limitations"`
	Date        time.Time  `json:"date"`
}

// TaskReview сохраняет собственное заключение проверяющего об одном результате.
type TaskReview struct {
	ID       string    `json:"id"`
	ResultID string    `json:"resultId"`
	Author   string    `json:"author"`
	Verdict  string    `json:"verdict"`
	Evidence string    `json:"evidence"`
	Date     time.Time `json:"date"`
}

// TaskCommand не содержит автора: его задаёт runtime или человеческий endpoint.
// ID сохраняется клиентом для повтора; Version обязателен, кроме create.
type TaskCommand struct {
	Workspace    *TaskWorkspace `json:"workspace,omitempty"`
	Integration  string         `json:"integration,omitempty"`
	ID           string         `json:"id"`
	Action       string         `json:"action"`
	TaskID       string         `json:"taskId"`
	Version      uint64         `json:"version"`
	Title        string         `json:"title,omitempty"`
	Body         string         `json:"body,omitempty"`
	Expected     string         `json:"expected,omitempty"`
	Criteria     []string       `json:"criteria,omitempty"`
	Assignee     string         `json:"assignee,omitempty"`
	Priority     int            `json:"priority,omitempty"`
	Dependencies []string       `json:"dependencies,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	Result       *TaskResult    `json:"result,omitempty"`
	ResultID     string         `json:"resultId,omitempty"`
	Verdict      string         `json:"verdict,omitempty"`
}

// TaskReceipt подтверждает точный повтор даже после последующих изменений карточки.
// Снимок результата не подменяет актуальную карточку в Room.Tasks.
type TaskReceipt struct {
	Digest string   `json:"digest"`
	Task   TeamTask `json:"task"`
}

// TaskWorkspace — заявление агента об изоляции. Path является справочной
// ссылкой, не разрешением серверу читать произвольный путь. Проверяемые файлы
// указываются относительно разрешённого cwd заказа; внешние результаты — Links.
type TaskWorkspace struct {
	Path        string `json:"path"`
	Method      string `json:"method"`
	Limitations string `json:"limitations"`
}

// ApplyTaskCommand сохраняет переход под team.lock. Файлы проверяются после
// авторизации; внешний писатель не держит этот lock, поэтому атомарность
// распространяется на карточку, но не на filesystem проекта.
func ApplyTaskCommand(ctx context.Context, root, run, author string, in TaskCommand) (TeamTask, error) {
	var out TeamTask
	snapshot, err := TeamRoot(root, run)
	if err != nil {
		return out, err
	}
	err = UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		var err error
		out, err = applyTaskCommand(chat, author, in, snapshot.Meta.CWD)
		return err
	})
	return out, err
}

// applyTaskCommand вызывается только под team.lock; ошибки не сохраняют снимок.
func applyTaskCommand(chat *TeamChat, author string, in TaskCommand, cwd string) (TeamTask, error) {
	fail := func(s string) (TeamTask, error) { return TeamTask{}, errors.New(s) }
	if chat.Room == nil {
		return fail("нет активной комнаты")
	}
	if !taskText(in.ID, 200) || !taskText(in.TaskID, 200) {
		return fail("нужны ID операции и задачи до 200 байт")
	}
	if len(in.Body) > 32000 || len(in.Reason) > 16000 || len(in.Title) > 500 || len(in.Expected) > 16000 {
		return fail("слишком длинные поля задачи")
	}
	data, _ := json.Marshal(struct {
		Author string
		Input  TaskCommand
	}{author, in})
	if len(data) > 128<<10 {
		return fail("команда задачи превышает 128 КиБ")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if receipt, ok := chat.Room.TaskOperations[in.ID]; ok {
		if receipt.Digest != digest {
			return fail("ID операции занят другим запросом")
		}
		return cloneTask(receipt.Task), nil
	}
	actor := chat.Room.Actors[author]
	if author != "human" && (actor == nil || actor.Status != "working" || actor.Delivery == nil) {
		return fail("нужен активный ход участника")
	}
	if chat.Room.AchievedAt != nil {
		return fail("цель достигнута")
	}
	for _, m := range chat.Messages {
		if m.ID == in.ID {
			return fail("ID операции занят сообщением")
		}
	}
	managed := slices.Contains([]string{"create", "edit", "assign", "priority", "dependencies", "cancel", "return", "accept", "cancel_finish"}, in.Action)
	if author == "human" && !slices.Contains([]string{"create", "edit", "assign", "priority", "dependencies", "cancel"}, in.Action) {
		return fail("человек может явно изменить задачу; приёмку и завершение отмены выполняет Босс")
	}
	if managed && author != "boss" && author != "human" {
		return fail("задачами управляет только Босс")
	}
	original := chat.Room.Tasks[in.TaskID]
	var task TeamTask
	if in.Action == "create" {
		if original != nil || in.Version != 0 {
			return fail("задача существует или задана версия новой задачи")
		}
		task = TeamTask{ID: in.TaskID, Text: in.Body, Assignee: in.Assignee, Card: &TaskCard{Title: in.Title, Expected: in.Expected, Criteria: slices.Clone(in.Criteria), Revision: 1, Status: "todo", Priority: in.Priority, Dependencies: slices.Clone(in.Dependencies)}}
	} else {
		if original == nil {
			return fail("задача не найдена")
		}
		task = cloneTask(*original)
		if task.Card == nil {
			if in.Action != "edit" || in.Version != 0 {
				return fail("legacy-поручение уточняется через edit с version 0 и полными требованиями; исторические данные сохраняются")
			}
			task.Card = &TaskCard{Status: "todo"}
		}
		if in.Version != task.Card.Version {
			return fail("конфликт версии: перечитай карточку")
		}
	}
	c := task.Card
	beforeRevision, beforeBody := c.Revision, task.Text
	if in.Action != "create" && c.Cancellation != "" && in.Action != "cancel_finish" && in.Action != "cancel_evidence" {
		return fail("отмена запрещает новые операции задачи")
	}
	if slices.Contains([]string{"start", "report", "accept"}, in.Action) {
		for _, dep := range c.Dependencies {
			d := chat.Room.Tasks[dep]
			if d == nil || d.Card == nil || d.Card.Status != "done" || d.Card.Cancellation != "" || d.Card.StaleReason != "" || len(d.Card.Results) == 0 {
				return fail("зависимость не принята или устарела")
			}
			result := d.Card.Results[len(d.Card.Results)-1]
			if len(result.Artifacts) > 0 {
				if err := CheckTaskBasis(cwd, result.Basis); err != nil {
					return TeamTask{}, fmt.Errorf("зависимость %s: %w", dep, err)
				}
			}
		}
	}
	assigned := task.Assignee == author
	switch in.Action {
	case "create":
	case "edit":
		task.Text, c.Title, c.Expected, c.Criteria = in.Body, in.Title, in.Expected, slices.Clone(in.Criteria)
		c.Revision++
		c.StaleReason = "Изменены требования или назначение"
		c.Status = "todo"
		c.Acknowledged = 0
	case "assign":
		if previous := chat.Room.Actors[task.Assignee]; previous != nil && previous.Delivery != nil && (c.Status != "todo" || previous.Delivery.TaskID == task.ID) {
			return fail("сначала останови прежнего исполнителя и подтверди завершение его хода")
		}
		task.Assignee = in.Assignee
		c.Revision++
		c.StaleReason = "Изменены требования или назначение"
		c.Status = "todo"
		c.Acknowledged = 0
	case "priority":
		c.Priority = in.Priority
	case "dependencies":
		c.Dependencies = slices.Clone(in.Dependencies)
		c.Revision++
		c.StaleReason = "Изменены требования или назначение"
		c.Status = "todo"
		c.Acknowledged = 0
	case "read":
		if !assigned {
			return fail("ознакомление фиксирует назначенный исполнитель")
		}
		c.ReadRevision = c.Revision
	case "acknowledge":
		if !assigned || c.ReadRevision != c.Revision {
			return fail("сначала прочитай актуальную revision назначенной задачи")
		}
		c.Acknowledged = c.Revision
	case "start":
		for _, other := range chat.Room.Tasks {
			if other.ID != task.ID && other.Assignee == author && other.Card != nil && other.Card.Status == "in_progress" && other.Card.Cancellation == "" && other.Card.Blocker == "" {
				return fail("сначала заверши или заблокируй текущую задачу")
			}
		}
		if !assigned || c.Acknowledged != c.Revision || c.Status != "todo" || c.Blocker != "" {
			return fail("задача не готова: нужны назначение, актуальное подтверждение и отсутствие блокера")
		}
		for _, id := range c.Dependencies {
			d := chat.Room.Tasks[id]
			if d == nil || d.Card == nil || d.Card.Status != "done" || d.Card.Cancellation != "" {
				return fail("зависимость ещё не принята")
			}
		}
		c.Status = "in_progress"
	case "workspace":
		if !assigned && author != "boss" {
			return fail("рабочую папку указывает исполнитель или Босс")
		}
		if in.Workspace == nil || !taskText(in.Workspace.Path, 4000) || !taskText(in.Workspace.Method, 4000) || !taskText(in.Workspace.Limitations, 4000) {
			return fail("укажи путь, способ изоляции и ограничения (в том числе отсутствие изоляции)")
		}
		copy := *in.Workspace
		c.Workspace = &copy
	case "block":
		if !assigned && author != "boss" {
			return fail("блокер меняет исполнитель или Босс")
		}
		c.Blocker = in.Reason
		// После снятия препятствия работа снова проходит выдачу scheduler;
		// во время самой блокировки основной статус сохранялся.
		if c.Blocker == "" && c.Status == "in_progress" {
			c.Status = "todo"
		}
	case "report":
		if !assigned || c.Status != "in_progress" || c.Acknowledged != c.Revision || in.Result == nil {
			return fail("нужен результат исполнителя актуальной задачи в работе")
		}
		r := *in.Result
		if !taskText(r.Summary, 16000) || len(r.Artifacts)+len(r.Links) == 0 || len(r.Checks) == 0 || !taskText(r.Limitations, 16000) {
			return fail("укажи результат, артефакты, проверки и ограничения")
		}
		if r.Basis != nil || r.Author != "" || r.ID != "" || r.Revision != 0 || !r.Date.IsZero() {
			return fail("основу и автора результата записывает сервер")
		}
		if len(r.Artifacts) > 0 {
			basis, err := CaptureTaskBasis(cwd, r.Artifacts)
			if err != nil {
				return TeamTask{}, err
			}
			r.Basis = &basis
		}
		if len(r.Links) > 100 || len(r.Checks) > 100 {
			return fail("слишком много ссылок или проверок")
		}
		for _, value := range append(slices.Clone(r.Links), r.Checks...) {
			if !taskText(value, 4000) {
				return fail("пустая или слишком длинная ссылка/проверка")
			}
		}
		c.StaleReason = ""
		r.ID, r.Author, r.Revision, r.Date = in.ID, author, c.Revision, time.Now().UTC()
		c.Results = append(c.Results, r)
		c.Status = "in_review"
	case "review":
		if assigned || c.Status != "in_review" || !taskText(in.Reason, 16000) || !slices.Contains([]string{"approve", "changes_requested"}, in.Verdict) {
			return fail("нужно независимое заключение о результате на проверке")
		}
		if len(c.Results) == 0 || c.Results[len(c.Results)-1].ID != in.ResultID {
			return fail("нужен последний результат задачи")
		}
		r := c.Results[len(c.Results)-1]
		if r.Revision != c.Revision {
			return fail("результат относится к старой revision")
		}
		if len(r.Artifacts) > 0 {
			if err := CheckTaskBasis(cwd, r.Basis); err != nil {
				return TeamTask{}, err
			}
		}
		c.Reviews = append(c.Reviews, TaskReview{ID: in.ID, ResultID: in.ResultID, Author: author, Verdict: in.Verdict, Evidence: in.Reason, Date: time.Now().UTC()})
	case "return":
		if c.Status != "in_review" || !taskText(in.Reason, 16000) {
			return fail("для возврата нужны результат на проверке и причина")
		}
		c.Status = "todo"
		c.Acknowledged = 0
	case "cancel":
		if !taskText(in.Reason, 16000) {
			return fail("укажи причину отмены")
		}
		// Завершённая отмена появится только после подтверждённого исключения вклада.
		c.Cancellation = "cancel_requested"
		c.CancelReason = in.Reason
	case "accept":
		if c.Status != "in_review" || c.Blocker != "" || len(c.Results) == 0 || len(c.Reviews) == 0 || !taskText(in.Reason, 16000) || !taskText(in.Integration, 16000) {
			return fail("нужны результат на проверке, заключение, проверка Босса и описание включения результата")
		}
		r := c.Results[len(c.Results)-1]
		review := c.Reviews[len(c.Reviews)-1]
		if r.ID != in.ResultID || r.Revision != c.Revision || review.ResultID != r.ID || review.Verdict != "approve" {
			return fail("нет актуального положительного заключения")
		}
		if a := chat.Room.Actors[task.Assignee]; a != nil && a.Delivery != nil {
			return fail("сначала дождись завершения хода исполнителя")
		}
		if len(r.Artifacts) > 0 {
			if err := CheckTaskBasis(cwd, r.Basis); err != nil {
				return TeamTask{}, err
			}
		}
		for _, id := range c.Dependencies {
			d := chat.Room.Tasks[id]
			if d == nil || d.Card == nil || d.Card.Status != "done" || d.Card.Cancellation != "" || d.Card.StaleReason != "" {
				return fail("зависимость потеряла актуальность")
			}
		}
		now := time.Now().UTC()
		task.AcceptedAt = &now
		task.ResultID = r.ID
		task.Evidence = in.Reason
		c.Status = "done"
		c.Integration = in.Integration
		c.StaleReason = ""
	case "cancel_evidence":
		if !assigned && author != "boss" {
			return fail("исключение вклада подтверждает исполнитель или Босс")
		}
		if c.Cancellation != "cancel_requested" || !taskText(in.Reason, 16000) {
			return fail("нужны запрос отмены и доказательства безопасного исключения/архива")
		}
		c.CancellationEvidence = in.Reason
	case "cancel_finish":
		if c.Cancellation != "cancel_requested" || c.CancellationEvidence == "" || !taskText(in.Reason, 16000) {
			return fail("Босс завершает отмену после доказательств исключения вклада")
		}
		if a := chat.Room.Actors[task.Assignee]; a != nil && a.Delivery != nil {
			return fail("сначала дождись остановки исполнителя")
		}
		c.Cancellation = "cancelled"
	default:
		return fail("неизвестная операция задачи")
	}
	if !taskText(task.Text, 32000) || !taskText(c.Title, 500) || !taskText(c.Expected, 16000) || len(c.Criteria) == 0 || len(c.Criteria) > 50 {
		return fail("нужны название, описание, ожидаемый результат и 1–50 критериев")
	}
	for _, s := range c.Criteria {
		if !taskText(s, 4000) {
			return fail("критерий пуст или слишком длинный")
		}
	}
	if task.Assignee != "" && (task.Assignee == "boss" || chat.Room.Actors[task.Assignee] == nil) {
		return fail("исполнитель должен быть приглашённым сотрудником")
	}
	if c.Priority < -100 || c.Priority > 100 || len(c.Dependencies) > 100 || len(in.Reason) > 16000 {
		return fail("превышены границы приоритета, зависимостей или причины")
	}
	if err := validateTaskDependencies(chat, task); err != nil {
		return TeamTask{}, err
	}
	if slices.Contains([]string{"create", "edit", "assign", "dependencies", "return"}, in.Action) || in.Action == "block" && c.Blocker == "" {
		c.ScheduleKey = in.ID
	}
	c.Version++
	change := TaskChange{ID: in.ID, Author: author, Action: in.Action, Date: time.Now().UTC(), FromRevision: beforeRevision, Revision: c.Revision, Reason: in.Reason}
	if original != nil && managed {
		previous := compactTask(*original)
		change.Previous = &previous
	}
	if in.Action == "edit" {
		change.PreviousBody = beforeBody
	}
	c.History = append(c.History, change)
	if chat.Room.Tasks == nil {
		chat.Room.Tasks = map[string]*TeamTask{}
	}
	chat.Room.Tasks[task.ID] = &task
	chat.Room.TaskBoardVersion = 1
	if chat.Room.TaskOperations == nil {
		chat.Room.TaskOperations = map[string]TaskReceipt{}
	}
	chat.Room.TaskOperations[in.ID] = TaskReceipt{Digest: digest, Task: compactTask(task)}
	to := ""
	if (managed || in.Action == "block" && author == "boss") && task.Assignee != "" {
		to = task.Assignee
	}
	if in.Action == "report" || in.Action == "review" || in.Action == "block" || in.Action == "cancel_evidence" {
		to = "boss"
	}
	m := TeamMessage{ID: in.ID, AuthorID: author, To: to, Kind: "task_change", Date: change.Date, TaskID: task.ID, TaskRevision: c.Revision, Text: fmt.Sprintf("Задача %s: %s, revision %d → %d. %s", task.ID, in.Action, beforeRevision, c.Revision, in.Reason)}
	if in.Action == "report" && len(c.History) > 0 {
		m.ReplyTo = c.History[0].ID
		for _, source := range chat.Messages {
			if source.ID == task.ID {
				m.ReplyTo = source.ID
				break
			}
		}
	}
	snapshot := compactTask(task)
	m.TaskSnapshot = &snapshot
	chat.Messages = append(chat.Messages, m)
	wakeForMessage(chat, m)
	if slices.Contains([]string{"edit", "assign", "dependencies", "cancel", "return"}, in.Action) {
		invalidateTaskDependents(chat, task.ID)
	}
	wakeReadyTasks(chat)
	return compactTask(task), nil
}

// taskText проверяет конечные строки до сохранения в общий контекст.
func taskText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= max && utf8.ValidString(s)
}

// cloneTask не оставляет общих slices между receipt и изменяемой карточкой.
func cloneTask(t TeamTask) TeamTask {
	data, _ := json.Marshal(t)
	var copy TeamTask
	_ = json.Unmarshal(data, &copy)
	return copy
}

// validateTaskDependencies проверяет весь путь через предложенную карточку;
// неизвестный ID, повтор и цикл отклоняются до изменения общего снимка.
func validateTaskDependencies(chat *TeamChat, task TeamTask) error {
	seen := map[string]bool{}
	for _, id := range task.Card.Dependencies {
		if seen[id] || id == task.ID || chat.Room.Tasks[id] == nil {
			return errors.New("неизвестная, повторная зависимость или ссылка на себя")
		}
		seen[id] = true
	}
	active, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if active[id] {
			return errors.New("цикл зависимостей задач")
		}
		if done[id] {
			return nil
		}
		active[id] = true
		t := chat.Room.Tasks[id]
		if id == task.ID {
			t = &task
		}
		if t != nil && t.Card != nil {
			for _, dep := range t.Card.Dependencies {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		active[id] = false
		done[id] = true
		return nil
	}
	return visit(task.ID)
}

// TaskPage возвращает ограниченную страницу карточек в стабильном порядке ID.
// GET не меняет историю и не подтверждает ознакомление от имени сотрудника.
func TaskPage(chat TeamChat, after string, limit int) ([]TeamTask, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", errors.New("limit должен быть 1–100")
	}
	ids := []string{}
	if chat.Room != nil {
		for id := range chat.Room.Tasks {
			if id > after {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	next := ""
	if len(ids) > limit {
		ids = ids[:limit]
		next = ids[len(ids)-1]
	}
	out := make([]TeamTask, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneTask(*chat.Room.Tasks[id]))
	}
	return out, next, nil
}
