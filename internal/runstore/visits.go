package runstore

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// RunState — сохранённый итог всего agent-graph run. Он отделён от состояния
// отдельного посещения: технический Failed одного агента может быть нормальным
// входом следующего проверяющего кубика через after.
type RunState string

const (
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
)

// TriggerKind объясняет, почему появилось посещение. Источник сохраняется по
// visitId, а не по stepId: при цикле это единственный однозначный causal link.
type TriggerKind string

const (
	TriggerStart    TriggerKind = "start"
	TriggerAfter    TriggerKind = "after"
	TriggerDecision TriggerKind = "decision"
)

// VisitTrigger хранит неизменяемую причину активации. SourceVisitIDs упорядочены:
// для after они следуют порядку Step.After, для decision допустим один источник.
type VisitTrigger struct {
	Kind           TriggerKind `json:"kind"`
	SourceVisitIDs []string    `json:"sourceVisitIds,omitempty"`
	DecisionKey    string      `json:"decisionKey,omitempty"`
}

// DecisionRecord — durable commit одного вызова агента решения. To/Finish и
// Skipped материализуются из неизменяемого workflow в момент commit, поэтому
// resume не разбирает финальный текст и не вычисляет маршрут повторно. Applied
// станет границей ровно-однократного применения в следующем срезе runtime.
type DecisionRecord struct {
	Key         string                    `json:"key"`
	Explanation string                    `json:"explanation,omitempty"`
	TurnID      string                    `json:"turnId"`
	CallID      string                    `json:"callId"`
	To          []string                  `json:"to,omitempty"`
	Finish      *workflow.TerminalOutcome `json:"finish,omitempty"`
	Skipped     []string                  `json:"skipped,omitempty"`
	Applied     bool                      `json:"applied"`
	Error       string                    `json:"error,omitempty"`
}

// Visit — одна попытка логического шага в истории графа. Visit возрастает без
// пропусков отдельно для каждого StepID; VisitID адресует metadata и файл памяти.
// Attempt возрастает при каждом новом turn того же Codex-чата. Старые посещения
// никогда не переиспользуются для следующего прохода цикла.
type Visit struct {
	VisitID        string          `json:"visitId"`
	StepID         string          `json:"stepId"`
	Visit          int             `json:"visit"`
	Iteration      int             `json:"iteration"`
	Attempt        int             `json:"attempt"`
	Trigger        VisitTrigger    `json:"trigger"`
	State          scheduler.State `json:"state"`
	CodexThreadID  string          `json:"codexThreadId,omitempty"`
	TurnID         string          `json:"turnId,omitempty"`
	TechnicalError string          `json:"technicalError,omitempty"`
	Decision       *DecisionRecord `json:"decision,omitempty"`
}

// validateAgentGraph проверяет v4 как самодостаточный append-only журнал. Все
// ссылки источников обязаны вести назад по Visits: это исключает подмену истории
// и позволяет следующему координатору безопасно продолжить после сбоя процесса.
func (s Snapshot) validateAgentGraph(runID string) error {
	m := s.Meta
	if m.Version != 4 || !validID(m.RunID) || m.RunID != runID || m.RunID == m.ParentRunID ||
		m.ParentRunID != "" && !validID(m.ParentRunID) ||
		m.ChildRequestID != "" && (m.ParentRunID == "" || !validID(m.ChildRequestID)) ||
		!filepathIsSafeAbsolute(m.CWD) || m.InitiatorThreadID != "" || m.Steps != nil ||
		s.Workflow.EffectiveVersion() != workflow.VersionAgentGraph || len(m.Visits) < len(s.Workflow.Start) {
		return fmt.Errorf("повреждены входы, версия или состав meta.json v4")
	}
	if !validText(s.Task) {
		return fmt.Errorf("task.md: постановка должна быть непустым текстом UTF-8")
	}
	if err := s.Workflow.Validate(); err != nil {
		return fmt.Errorf("workflow v4: %w", err)
	}
	if m.RunState != RunRunning && m.RunState != RunSucceeded && m.RunState != RunFailed ||
		m.RunState == RunRunning && m.StopReason != "" || m.RunState != RunRunning && !safeStoredText(m.StopReason, true) {
		return fmt.Errorf("runState не соответствует безопасной причине остановки")
	}

	steps := make(map[string]workflow.Step, len(s.Workflow.Steps))
	for _, step := range s.Workflow.Steps {
		steps[step.ID] = step
	}
	seen, chats := make(map[string]Visit), make(map[string]bool)
	numbers, active := make(map[string]int), make(map[string]bool)
	afterUses, decisionUses := make(map[afterCause]bool), make(map[decisionCause]bool)
	for index, visit := range m.Visits {
		step, exists := steps[visit.StepID]
		if !exists || !validID(visit.VisitID) {
			return fmt.Errorf("посещение %q: неизвестный stepId или неверный visitId", visit.VisitID)
		}
		if _, duplicate := seen[visit.VisitID]; duplicate {
			return fmt.Errorf("посещение %q: повторный visitId", visit.VisitID)
		}
		numbers[visit.StepID]++
		if visit.Visit != numbers[visit.StepID] || visit.Iteration < 1 || visit.Attempt < 0 {
			return fmt.Errorf("посещение %q: нарушена нумерация visit/iteration/attempt", visit.VisitID)
		}
		if err := validateVisitState(visit, chats); err != nil {
			return fmt.Errorf("посещение %q: %w", visit.VisitID, err)
		}
		if active[visit.StepID] {
			return fmt.Errorf("предыдущее посещение шага %q ещё не завершено", visit.StepID)
		}
		if !visitTerminal(visit.State) {
			active[visit.StepID] = true
		}
		if err := validateTrigger(index, visit, step, s.Workflow.Start, m.Visits[:index], seen, afterUses, decisionUses); err != nil {
			return fmt.Errorf("посещение %q: %w", visit.VisitID, err)
		}
		if err := validateDecision(visit, step); err != nil {
			return fmt.Errorf("посещение %q: %w", visit.VisitID, err)
		}
		seen[visit.VisitID] = visit
	}
	matchingFinish := false
	for _, visit := range m.Visits {
		if visit.Decision == nil || !visit.Decision.Applied {
			continue
		}
		for _, target := range visit.Decision.To {
			if !decisionUses[decisionCause{target, visit.VisitID}] {
				return fmt.Errorf("решение посещения %q не материализовало target %q", visit.VisitID, target)
			}
		}
		if visit.Decision.Finish != nil {
			expected := RunFailed
			if *visit.Decision.Finish == workflow.OutcomeSucceeded {
				expected = RunSucceeded
			}
			if m.RunState != expected {
				return fmt.Errorf("finish посещения %q не совпадает с runState", visit.VisitID)
			}
			matchingFinish = true
		}
	}
	if m.RunState != RunRunning && len(active) != 0 && !matchingFinish {
		return fmt.Errorf("терминальный run с незавершёнными visits требует применённый finish")
	}
	return nil
}

// Причины активации сравниваются составными ключами без склейки пользовательских
// stepId: так любые допустимые символы в ID не создают ложных совпадений.
type afterCause struct{ targetStepID, dependencyStepID, sourceVisitID string }
type decisionCause struct{ targetStepID, sourceVisitID string }

// filepathIsSafeAbsolute повторяет общую границу cwd без принятия NUL.
func filepathIsSafeAbsolute(path string) bool {
	return filepath.IsAbs(path) && validText(path) && !strings.ContainsRune(path, 0)
}

// validateVisitState связывает внешний chat/turn с фазой. Thread глобально
// уникален, а turnId и callId являются непрозрачными ID внутри своего thread.
func validateVisitState(visit Visit, chats map[string]bool) error {
	switch visit.State {
	case scheduler.Pending:
		if visit.CodexThreadID != "" || visit.TurnID != "" || visit.Attempt != 0 || visit.TechnicalError != "" || visit.Decision != nil {
			return fmt.Errorf("Pending не может содержать результаты запуска")
		}
	case scheduler.Starting:
		if visit.TurnID != "" || visit.Attempt != 0 || visit.TechnicalError != "" || visit.Decision != nil {
			return fmt.Errorf("Starting не может содержать turn или результат")
		}
	case scheduler.Unknown, scheduler.Running, scheduler.WaitingForApproval, scheduler.Failed, scheduler.Cancelled, scheduler.Succeeded:
		if !safeStoredText(visit.CodexThreadID, true) {
			return fmt.Errorf("состояние %q требует безопасный codexThreadId", visit.State)
		}
	default:
		return fmt.Errorf("неизвестное состояние %q", visit.State)
	}
	if visit.CodexThreadID != "" {
		if !safeStoredText(visit.CodexThreadID, true) || chats[visit.CodexThreadID] {
			return fmt.Errorf("неверный или повторный codexThreadId")
		}
		chats[visit.CodexThreadID] = true
	}
	if (visit.TurnID == "") != (visit.Attempt == 0) || visit.TurnID != "" && !safeStoredText(visit.TurnID, true) {
		return fmt.Errorf("attempt и turnId должны появляться вместе")
	}
	if (visit.State == scheduler.Running || visit.State == scheduler.WaitingForApproval || visit.State == scheduler.Succeeded) && visit.Attempt == 0 {
		return fmt.Errorf("состояние %q требует сохранённый turn", visit.State)
	}
	if visit.TechnicalError != "" && (visit.State != scheduler.Unknown && visit.State != scheduler.Failed && visit.State != scheduler.Cancelled || !safeStoredText(visit.TechnicalError, false)) {
		return fmt.Errorf("technicalError недопустима для состояния или небезопасна")
	}
	return nil
}

func validateTrigger(index int, visit Visit, step workflow.Step, start []string, history []Visit, seen map[string]Visit, afterUses map[afterCause]bool, decisionUses map[decisionCause]bool) error {
	if index < len(start) {
		if visit.StepID != start[index] || visit.Trigger.Kind != TriggerStart || visit.Visit != 1 || visit.Iteration != 1 {
			return fmt.Errorf("начальные посещения должны совпадать с порядком workflow.start")
		}
	}
	switch visit.Trigger.Kind {
	case TriggerStart:
		if index >= len(start) || len(visit.Trigger.SourceVisitIDs) != 0 || visit.Trigger.DecisionKey != "" {
			return fmt.Errorf("start допустим только в начальном префиксе без источников")
		}
	case TriggerAfter:
		if len(step.After) == 0 || len(visit.Trigger.SourceVisitIDs) != len(step.After) || visit.Trigger.DecisionKey != "" {
			return fmt.Errorf("after должен содержать источник для каждого Step.After")
		}
		maxIteration := 0
		for i, sourceID := range visit.Trigger.SourceVisitIDs {
			source, exists := seen[sourceID]
			if !exists || !visitTerminal(source.State) || source.StepID != step.After[i] {
				return fmt.Errorf("after-источник %q не является нужным завершённым посещением", sourceID)
			}
			cause := afterCause{visit.StepID, step.After[i], sourceID}
			expected := ""
			for _, candidate := range history {
				candidateCause := afterCause{visit.StepID, step.After[i], candidate.VisitID}
				if candidate.StepID == step.After[i] && visitTerminal(candidate.State) && !afterUses[candidateCause] {
					expected = candidate.VisitID
					break
				}
			}
			if sourceID != expected || afterUses[cause] {
				return fmt.Errorf("after должен потреблять самый ранний неиспользованный источник")
			}
			afterUses[cause] = true
			maxIteration = max(maxIteration, source.Iteration)
		}
		if visit.Iteration != maxIteration {
			return fmt.Errorf("after должен наследовать максимальную iteration источников")
		}
	case TriggerDecision:
		if len(visit.Trigger.SourceVisitIDs) != 1 || visit.Trigger.DecisionKey == "" {
			return fmt.Errorf("decision требует один источник и ключ")
		}
		source, exists := seen[visit.Trigger.SourceVisitIDs[0]]
		if !exists || source.State != scheduler.Succeeded || source.Decision == nil || !source.Decision.Applied ||
			source.Decision.Key != visit.Trigger.DecisionKey || !slices.Contains(source.Decision.To, visit.StepID) {
			return fmt.Errorf("decision-источник не применял этот маршрут к шагу")
		}
		cause := decisionCause{visit.StepID, source.VisitID}
		if decisionUses[cause] {
			return fmt.Errorf("decision-источник уже материализовал этот target")
		}
		decisionUses[cause] = true
		if visit.Iteration != source.Iteration+1 {
			return fmt.Errorf("decision-target должен увеличить iteration источника")
		}
	default:
		return fmt.Errorf("неизвестный trigger %q", visit.Trigger.Kind)
	}
	return nil
}

func validateDecision(visit Visit, step workflow.Step) error {
	if visit.Decision == nil {
		// Succeeded описывает только фактический terminal turn Codex. Отсутствие
		// обязательного выбора является бизнес-ошибкой всего workflow, которую
		// visit-aware планировщик сохраняет отдельно в RunState/StopReason.
		return nil
	}
	d := visit.Decision
	route, exists := step.Decisions[d.Key]
	if !exists || visit.Attempt == 0 || !safeStoredText(d.TurnID, true) || !safeStoredText(d.CallID, true) || !safeStoredText(d.Explanation, false) ||
		!slices.Equal(d.To, route.To) || !sameOutcome(d.Finish, route.Finish) {
		return fmt.Errorf("решение не совпадает с разрешённым маршрутом workflow")
	}
	keys := make([]string, 0, len(step.Decisions)-1)
	for key := range step.Decisions {
		if key != d.Key {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if !slices.Equal(d.Skipped, keys) || d.Applied && visit.State != scheduler.Succeeded ||
		d.Error != "" && (!safeStoredText(d.Error, false) || d.Applied) {
		return fmt.Errorf("решение содержит неверные skipped/error")
	}
	return nil
}

func visitTerminal(state scheduler.State) bool {
	return state == scheduler.Failed || state == scheduler.Succeeded
}

func sameOutcome(left, right *workflow.TerminalOutcome) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

// safeStoredText не даёт непроверенной диагностике сохранить управляющие
// символы для будущего terminal/dashboard. Ограничение защищает и размер meta.
func safeStoredText(value string, required bool) bool {
	if !utf8.ValidString(value) || len(value) > 4096 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	return !required || strings.TrimSpace(value) != ""
}
