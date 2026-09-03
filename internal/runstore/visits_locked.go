//go:build darwin || linux

package runstore

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// ErrAgentDecisionPoisoned сообщает coordinator, что после последнего planning
// commit появился конфликт решения. Новую работу резервировать нельзя: сначала
// AdvanceAgentGraph обязан атомарно сохранить terminal outcome.
var ErrAgentDecisionPoisoned = errors.New("agent-graph содержит конфликтующее решение")

// ReserveVisits атомарно фиксирует намерение запустить всю волну Pending-visits.
// Метод принимает только visitId: stepId неоднозначен после первого прохода
// цикла. До успешной публикации metadata сетевые запросы делать нельзя.
func (r *LockedRun) ReserveVisits(visitIDs []string) error {
	return r.mutateVisits((*os.File).Sync, func(s *Snapshot) (bool, error) {
		if s.Meta.RunState != RunRunning {
			return false, fmt.Errorf("завершённый run не принимает новые посещения")
		}
		// Decision callback может завершиться между AdvanceAgentGraph и этим
		// commit. Проверка выполняется даже для волны одних continuation: иначе
		// пустой список позволил бы отправить новый turn после durable poison.
		for _, visit := range s.Meta.Visits {
			if visit.Decision != nil && visit.Decision.Error != "" {
				return false, ErrAgentDecisionPoisoned
			}
		}
		if len(visitIDs) == 0 {
			return false, nil
		}
		indices := visitIndices(s.Meta.Visits)
		seen := make(map[string]bool, len(visitIDs))
		for _, visitID := range visitIDs {
			index, exists := indices[visitID]
			if !exists {
				return false, fmt.Errorf("нет посещения %q", visitID)
			}
			if seen[visitID] {
				return false, fmt.Errorf("посещение %q повторено в резервировании", visitID)
			}
			seen[visitID] = true
			visit := s.Meta.Visits[index]
			if visit.State != scheduler.Pending || visit.CodexThreadID != "" || visit.Attempt != 0 {
				return false, fmt.Errorf("посещение %q уже запускалось", visitID)
			}
		}
		for visitID := range seen {
			index := indices[visitID]
			s.Meta.Visits[index].State = scheduler.Starting
		}
		return true, nil
	})
}

// ReleaseUnattemptedVisit снимает только подтверждённое локальным клиентом
// резервирование, для которого thread/start не отправлялся. После перезапуска
// одного Starting недостаточно: потерянный ответ мог скрывать созданный чат.
func (r *LockedRun) ReleaseUnattemptedVisit(visitID string) error {
	return r.mutateVisits((*os.File).Sync, func(s *Snapshot) (bool, error) {
		index, err := findVisit(s.Meta.Visits, visitID)
		if err != nil {
			return false, err
		}
		visit := s.Meta.Visits[index]
		if visit.State != scheduler.Starting || visit.CodexThreadID != "" || visit.TurnID != "" || visit.Attempt != 0 || visit.Decision != nil {
			return false, fmt.Errorf("посещение %q: снять можно только неподтверждённое Starting", visitID)
		}
		s.Meta.Visits[index].State = scheduler.Pending
		return true, nil
	})
}

// UpdateVisit сохраняет фазу, неизменяемую связь с Codex-чатом и безопасную
// техническую диагностику. Завершённое посещение неизменяемо; повтор ровно того
// же значения остаётся идемпотентным и не переписывает диск.
func (r *LockedRun) UpdateVisit(visitID string, state scheduler.State, codexThreadID, technicalError string) error {
	return r.updateVisit(visitID, state, codexThreadID, technicalError, (*os.File).Sync)
}

// updateVisit принимает Sync для регрессионных тестов атомарной публикации.
func (r *LockedRun) updateVisit(visitID string, state scheduler.State, chat, diagnostic string, syncFile func(*os.File) error) error {
	return r.mutateVisits(syncFile, func(s *Snapshot) (bool, error) {
		index, err := findVisit(s.Meta.Visits, visitID)
		if err != nil {
			return false, err
		}
		old := s.Meta.Visits[index]
		if old.State == state && old.CodexThreadID == chat && old.TechnicalError == diagnostic {
			return false, nil
		}
		if old.State == scheduler.Pending && state == scheduler.Starting && s.Meta.RunState != RunRunning {
			return false, fmt.Errorf("завершённый run не принимает новые посещения")
		}
		if visitTerminal(old.State) {
			return false, fmt.Errorf("посещение %q уже завершено и неизменяемо", visitID)
		}
		if old.State == scheduler.Pending && (state != scheduler.Pending && state != scheduler.Starting || chat != "") {
			return false, fmt.Errorf("посещение %q: сначала сохраните Starting без ID чата", visitID)
		}
		if old.State != scheduler.Pending && state == scheduler.Pending ||
			old.State != scheduler.Pending && old.State != scheduler.Starting && state == scheduler.Starting ||
			old.CodexThreadID != "" && old.CodexThreadID != chat {
			return false, fmt.Errorf("посещение %q: нельзя сбросить запуск или изменить известный ID чата", visitID)
		}
		s.Meta.Visits[index].State = state
		s.Meta.Visits[index].CodexThreadID = chat
		s.Meta.Visits[index].TechnicalError = diagnostic
		return true, nil
	})
}

// SetVisitTurn сохраняет очередной distinct turn и увеличивает Attempt. Повтор
// того же turnId — идемпотентный replay; новый ID после terminal запрещён.
func (r *LockedRun) SetVisitTurn(visitID, turnID string) error {
	if !safeStoredText(turnID, true) {
		return errors.New("нужен безопасный непустой turn-id")
	}
	return r.mutateVisits((*os.File).Sync, func(s *Snapshot) (bool, error) {
		index, err := findVisit(s.Meta.Visits, visitID)
		if err != nil {
			return false, err
		}
		visit := s.Meta.Visits[index]
		if visit.TurnID == turnID {
			return false, nil
		}
		if s.Meta.RunState != RunRunning {
			return false, fmt.Errorf("завершённый run не принимает новый turn")
		}
		if visit.Decision != nil && (visit.State != scheduler.Cancelled || visit.Decision.Applied || visit.Decision.Error != "") {
			return false, fmt.Errorf("посещение %q уже сохранило решение и не принимает новый turn", visitID)
		}
		if visitTerminal(visit.State) {
			return false, fmt.Errorf("посещение %q уже завершено и не принимает новый turn", visitID)
		}
		if visit.CodexThreadID == "" || visit.State == scheduler.Pending || visit.State == scheduler.Starting {
			return false, fmt.Errorf("посещение %q ещё не связано с чатом Codex", visitID)
		}
		s.Meta.Visits[index].TurnID = turnID
		s.Meta.Visits[index].Attempt++
		return true, nil
	})
}

// CommitDecision атомарно сохраняет ровно один разрешённый выбор конкретного
// visit/thread/turn. Возвращаем материализованную копию маршрута для ответа tool,
// но единственным источником истины остаётся meta.json. Точный replay безопасен.
func (r *LockedRun) CommitDecision(visitID, codexThreadID, turnID, key, explanation, callID string) (DecisionRecord, error) {
	return r.commitDecision(visitID, codexThreadID, turnID, key, explanation, callID, (*os.File).Sync)
}

func (r *LockedRun) commitDecision(visitID, chat, turnID, key, explanation, callID string, syncFile func(*os.File) error) (DecisionRecord, error) {
	if !safeStoredText(callID, true) || !validText(key) || !safeStoredText(explanation, false) {
		return DecisionRecord{}, fmt.Errorf("решение содержит небезопасный key, explanation или callId")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return DecisionRecord{}, err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return DecisionRecord{}, err
	}
	if s.Meta.Version != 4 {
		return DecisionRecord{}, fmt.Errorf("операция посещений требует meta.json v4")
	}
	index, err := findVisit(s.Meta.Visits, visitID)
	if err != nil {
		return DecisionRecord{}, err
	}
	visit := s.Meta.Visits[index]
	if visit.CodexThreadID != chat || visit.TurnID != turnID || chat == "" || turnID == "" {
		return DecisionRecord{}, fmt.Errorf("посещение %q не принадлежит указанным thread/turn", visitID)
	}
	step, exists := workflowStep(s, visit.StepID)
	if !exists {
		return DecisionRecord{}, fmt.Errorf("нет шага %q", visit.StepID)
	}
	route, routeExists := step.Decisions[key]
	var desired DecisionRecord
	if routeExists {
		desired = DecisionRecord{Key: key, Explanation: explanation, TurnID: turnID, CallID: callID, To: slices.Clone(route.To), Finish: cloneOutcome(route.Finish)}
		for skipped := range step.Decisions {
			if skipped != key {
				desired.Skipped = append(desired.Skipped, skipped)
			}
		}
		sort.Strings(desired.Skipped)
	}
	if visit.Decision != nil {
		existing := cloneDecision(*visit.Decision)
		if routeExists && existing.Error == "" && sameDecisionPayload(existing, desired) {
			return existing, nil
		}
		conflict := fmt.Errorf("посещение %q уже сохранило другое решение", visitID)
		// После применения или terminal нельзя менять даже диагностику. До этой
		// границы одним commit сохраняем poison marker, чтобы resume не применил
		// первый маршрут после обнаруженного второго противоречивого tool call.
		// RunState меняет planner: callback не вправе завершить run, пока соседний
		// уже созданный Codex-turn ещё сохраняет свой ID для адресного interrupt.
		if s.Meta.RunState != RunRunning || visitTerminal(visit.State) || existing.Applied || existing.Error != "" {
			return existing, conflict
		}
		existing.Error = "агент повторно вызвал решение с другим callId или payload"
		s.Meta.Visits[index].Decision = &existing
		if err = s.validate(r.runID); err != nil {
			return DecisionRecord{}, err
		}
		if err = saveMetadata(r.dir, s.Meta, syncFile); err != nil {
			return existing, errors.Join(conflict, r.recordVisitSaveFailure(err))
		}
		return existing, conflict
	}
	if s.Meta.RunState != RunRunning {
		return DecisionRecord{}, fmt.Errorf("завершённый run не принимает новое решение")
	}
	if !routeExists {
		return DecisionRecord{}, fmt.Errorf("шаг %q не разрешает решение %q", visit.StepID, key)
	}
	if visitTerminal(visit.State) {
		return DecisionRecord{}, fmt.Errorf("посещение %q уже завершено без решения", visitID)
	}
	s.Meta.Visits[index].Decision = &desired
	if err = s.validate(r.runID); err != nil {
		return DecisionRecord{}, err
	}
	if err = saveMetadata(r.dir, s.Meta, syncFile); err != nil {
		return DecisionRecord{}, r.recordVisitSaveFailure(err)
	}
	return cloneDecision(desired), nil
}

// mutateVisits сериализует read-modify-validate-publish. Ошибка до save не
// отравляет владельца; ошибка save оставляет неизвестной сторону Rename и потому
// запрещает все следующие операции до Close и повторной сверки с диском/Codex.
func (r *LockedRun) mutateVisits(syncFile func(*os.File) error, mutate func(*Snapshot) (bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	if s.Meta.Version != 4 {
		return fmt.Errorf("операция посещений требует meta.json v4")
	}
	changed, err := mutate(&s)
	if err != nil || !changed {
		return err
	}
	if err = s.validate(r.runID); err != nil {
		return err
	}
	if err = saveMetadata(r.dir, s.Meta, syncFile); err != nil {
		return r.recordVisitSaveFailure(err)
	}
	return nil
}

func (r *LockedRun) recordVisitSaveFailure(err error) error {
	r.failed = fmt.Errorf("сохранение посещений запуска %q: %w; остановите новые запросы и восстановите состояние после повторного открытия", r.runID, err)
	return r.failed
}

func visitIndices(visits []Visit) map[string]int {
	result := make(map[string]int, len(visits))
	for index, visit := range visits {
		result[visit.VisitID] = index
	}
	return result
}

func findVisit(visits []Visit, visitID string) (int, error) {
	for index, visit := range visits {
		if visit.VisitID == visitID {
			return index, nil
		}
	}
	return -1, fmt.Errorf("нет посещения %q", visitID)
}

func workflowStep(s Snapshot, stepID string) (workflow.Step, bool) {
	for _, step := range s.Workflow.Steps {
		if step.ID == stepID {
			return step, true
		}
	}
	return workflow.Step{}, false
}

func cloneOutcome(value *workflow.TerminalOutcome) *workflow.TerminalOutcome {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneDecision(value DecisionRecord) DecisionRecord {
	value.To, value.Skipped, value.Finish = slices.Clone(value.To), slices.Clone(value.Skipped), cloneOutcome(value.Finish)
	return value
}

func sameDecisionPayload(left, right DecisionRecord) bool {
	return left.Key == right.Key && left.Explanation == right.Explanation && left.TurnID == right.TurnID && left.CallID == right.CallID &&
		slices.Equal(left.To, right.To) && sameOutcome(left.Finish, right.Finish) && slices.Equal(left.Skipped, right.Skipped)
}
