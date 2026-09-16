package runstore

import (
	"errors"
	"fmt"
	"os"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// AssignedWorkflowFilename — снимок доступного Боссу процесса. Это обычный
// workflow, который можно передать run_child; не вторая копия управляющего run.
const AssignedWorkflowFilename = "assigned-workflow.json"

// Order отделяет жизнь заказа от технического завершения ответа Босса. Исходный
// заказ остаётся в task.md, последующие реплики Чела хранятся дословно здесь.
// Pending означает намерение отправить новый turn: после сбоя его нельзя
// повторять вслепую. Предыдущий TurnID остаётся в Step до подтверждения нового.
type Order struct {
	Pending  bool           `json:"pending,omitempty"`
	Messages []OrderMessage `json:"messages,omitempty"`
}

// OrderMessage связывает точный текст Чела с подтверждённым turn Босса.
type OrderMessage struct {
	Text   string `json:"text"`
	TurnID string `json:"turnId,omitempty"`
}

// validateOrder не разрешает превратить произвольный многошаговый граф в диалог
// с помощью ручной правки meta.json. Завершение ответа не закрывает Order.
func (s Snapshot) validateOrder() error {
	if s.Meta.Order == nil {
		return nil
	}
	if s.Meta.Version != 3 || len(s.Workflow.Steps) != 1 || len(s.Meta.Steps) != 1 ||
		s.Workflow.Steps[0].ID != "boss" || s.Workflow.Steps[0].Character != "boss" {
		return errors.New("order требует единственного кубика личности boss")
	}
	for i, message := range s.Meta.Order.Messages {
		waiting := i == len(s.Meta.Order.Messages)-1 && s.Meta.Order.Pending
		if !validText(message.Text) || (message.TurnID == "") != waiting || (message.TurnID != "" && !validText(message.TurnID)) {
			return errors.New("повреждена история сообщений Чела")
		}
	}
	if s.Meta.Order.Pending && len(s.Meta.Order.Messages) == 0 {
		return errors.New("нет ожидающего сообщения Чела")
	}
	return nil
}

// ReserveMessage атомарно фиксирует реплику до сети. Lock всего run не позволяет
// двум интерактивным чатам одновременно продолжить одного Босса. При любой
// ошибке записи владелец становится непригодным для дальнейших запросов.
func (r *LockedRun) ReserveMessage(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	if s.Meta.Order == nil || s.Meta.Order.Pending || !validText(text) {
		return errors.New("заказ не готов к новому сообщению")
	}
	step := &s.Meta.Steps[0]
	if step.CodexThreadID == "" || step.TurnID == "" || (step.State != scheduler.Succeeded && step.State != scheduler.Failed && step.State != scheduler.Cancelled) {
		return errors.New("сначала дождитесь завершения текущего ответа Босса")
	}
	s.Meta.Order.Messages = append(s.Meta.Order.Messages, OrderMessage{Text: text})
	s.Meta.Order.Pending, step.State, step.Result = true, scheduler.Unknown, ""
	if err = s.validate(r.runID); err != nil {
		return err
	}
	if err = saveMetadata(r.dir, s.Meta, (*os.File).Sync); err != nil {
		r.failed = fmt.Errorf("сохранить сообщение Чела: %w", err)
		return r.failed
	}
	return nil
}

// ReleaseUnsentMessage снимает намерение только при доказанном отказе ДО
// turn/start. Состояние старого turn сверяется перед следующей попыткой.
// Для сетевой неопределённости этот метод вызывать запрещено.
func (r *LockedRun) ReleaseUnsentMessage() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	if s.Meta.Order == nil || !s.Meta.Order.Pending {
		return errors.New("нет неотправленного сообщения")
	}
	s.Meta.Order.Messages = s.Meta.Order.Messages[:len(s.Meta.Order.Messages)-1]
	s.Meta.Order.Pending = false
	if err = saveMetadata(r.dir, s.Meta, (*os.File).Sync); err != nil {
		r.failed = err
	}
	return err
}
