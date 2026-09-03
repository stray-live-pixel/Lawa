package runstore

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	TriggerStart           TriggerKind = "start"
	TriggerAfter           TriggerKind = "after"
	TriggerDecision        TriggerKind = "decision"
	TriggerDecisionSkipped TriggerKind = "decision_skipped"
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
	hasLimitProof := m.StopLimitStepID != "" || m.StopLimitTrigger != nil || m.StopLimitIteration != 0
	limitDecisionSourceID := ""
	if m.RunState != RunRunning && m.StopLimitTrigger != nil && m.StopLimitTrigger.Kind == TriggerDecision &&
		len(m.StopLimitTrigger.SourceVisitIDs) == 1 {
		limitDecisionSourceID = m.StopLimitTrigger.SourceVisitIDs[0]
	}
	if m.RunState != RunRunning && m.RunState != RunSucceeded && m.RunState != RunFailed ||
		m.RunState == RunRunning && (m.StopReason != "" || m.StopVisitID != "" || hasLimitProof) ||
		m.RunState != RunRunning && !safeStoredText(m.StopReason, true) {
		return fmt.Errorf("runState не соответствует безопасной причине остановки")
	}

	steps := make(map[string]workflow.Step, len(s.Workflow.Steps))
	for _, step := range s.Workflow.Steps {
		steps[step.ID] = step
	}
	seen, chats := make(map[string]Visit), make(map[string]bool)
	// numbers нумерует все durable branch instances, включая Skipped. Квота же
	// ограничивает только исполнения, способные создать Codex turn: причинная
	// запись об отсутствии запуска не должна отнимать разрешённое посещение.
	numbers, runnableNumbers := make(map[string]int), make(map[string]int)
	lastRunnable, active := make(map[string]Visit), make(map[string]bool)
	afterUses, decisionUses := make(map[afterCause]bool), make(map[decisionCause]bool)
	// skipWaves выводится только из проверенных sourceVisitIds. Один и тот же
	// набор корневых решений может пройти каждый decision-target лишь однажды:
	// это делает synthetic-обход безопасным даже для разрешённых route-циклов.
	skipWaves := make(map[string][]string)
	skipReached := make(map[skipReach]bool)
	advanced := make(map[string]bool)
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
		if visit.State != scheduler.Skipped {
			runnableNumbers[visit.StepID]++
			lastRunnable[visit.StepID] = visit
		}
		if step.MaxVisits != nil && runnableNumbers[visit.StepID] > *step.MaxVisits {
			return fmt.Errorf("посещение %q превышает maxVisits=%d шага %q", visit.VisitID, *step.MaxVisits, visit.StepID)
		}
		if err := validateVisitState(visit, chats); err != nil {
			return fmt.Errorf("посещение %q: %w", visit.VisitID, err)
		}
		// Skipped не занимает executor, поэтому отдельная причинная ветка может
		// быть записана рядом с реальным активным visit общего target. No-overlap
		// остаётся строгим для любых двух действительно запускаемых посещений.
		if active[visit.StepID] && visit.State != scheduler.Skipped {
			return fmt.Errorf("предыдущее посещение шага %q ещё не завершено", visit.StepID)
		}
		if !visitTerminal(visit.State) {
			active[visit.StepID] = true
		}
		roots, err := validateTriggerWithSkipWaves(
			index, visit, step, s.Workflow.Start, m.Visits[:index], seen, steps,
			afterUses, decisionUses, skipWaves, skipReached, m.RunState != RunRunning, limitDecisionSourceID,
		)
		if err != nil {
			return fmt.Errorf("посещение %q: %w", visit.VisitID, err)
		}
		if len(roots) != 0 {
			skipWaves[visit.VisitID] = roots
			skipReached[skipReach{waveKey: skipWaveKey(roots), targetStepID: visit.StepID}] = true
		}
		if err := validateSkippedVisit(m.RunState, visit, seen); err != nil {
			return fmt.Errorf("посещение %q: %w", visit.VisitID, err)
		}
		if err := validateDecision(visit, step); err != nil {
			return fmt.Errorf("посещение %q: %w", visit.VisitID, err)
		}
		for _, sourceID := range visit.Trigger.SourceVisitIDs {
			advanced[sourceID] = true
		}
		seen[visit.VisitID] = visit
	}
	matchingFinishID := ""
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
			if matchingFinishID != "" {
				return fmt.Errorf("run содержит больше одного применённого finish")
			}
			matchingFinishID = visit.VisitID
		}
	}
	stopVisit, hasStopVisit := seen[m.StopVisitID]
	if m.StopVisitID != "" && !hasStopVisit {
		return fmt.Errorf("stopVisitId не ссылается на сохранённое посещение")
	}
	if matchingFinishID != "" && m.StopVisitID != matchingFinishID {
		return fmt.Errorf("terminal finish требует свой visitId как причину остановки")
	}
	limitTerminal := false
	if hasLimitProof {
		if m.StopLimitStepID == "" || m.StopLimitTrigger == nil || m.StopLimitIteration < 1 {
			return fmt.Errorf("остановка по maxVisits требует полный trigger и iteration")
		}
		limitedStep, exists := steps[m.StopLimitStepID]
		if !exists || limitedStep.MaxVisits == nil {
			return fmt.Errorf("stopLimitStepId не ссылается на шаг с maxVisits")
		}
		lastAllowed, hasLastAllowed := lastRunnable[limitedStep.ID]
		if !hasStopVisit || stopVisit.StepID != limitedStep.ID || !hasLastAllowed || lastAllowed.VisitID != stopVisit.VisitID ||
			runnableNumbers[limitedStep.ID] != *limitedStep.MaxVisits {
			return fmt.Errorf("остановка по maxVisits требует последний разрешённый visit ограниченного шага")
		}
		outcome, expected := workflow.OutcomeFailed, RunFailed
		if limitedStep.OnLimit != nil {
			outcome = *limitedStep.OnLimit
		}
		if outcome == workflow.OutcomeSucceeded {
			expected = RunSucceeded
		}
		if m.RunState != expected {
			return fmt.Errorf("onLimit шага %q не совпадает с runState", limitedStep.ID)
		}
		if matchingFinishID != "" {
			return fmt.Errorf("остановка по maxVisits не может одновременно содержать applied finish")
		}
		// Planner допускает route/after/limit только после закрытия текущей
		// decision-wave. Это свойство монотонно после terminal: новые visits и
		// turns запрещены, а незавершённые внешние Result могут лишь закрыть волну.
		if err := validateLimitDecisionBarrier(m.Visits, steps); err != nil {
			return fmt.Errorf("остановка по maxVisits обошла decision-wave: %w", err)
		}
		// Проверяем сохранённую неслучившуюся активацию локально. Полный planner
		// переигрывать нельзя: terminal drain может завершить независимый visit и
		// открыть более ранний маршрут, не меняя уже опубликованный исход.
		if err := validateLimitTrigger(
			limitedStep, *m.StopLimitTrigger, m.StopLimitIteration, m.Visits,
			seen, steps, runnableNumbers, active, afterUses, decisionUses,
		); err != nil {
			return fmt.Errorf("остановка по maxVisits не подтверждена trigger: %w", err)
		}
		limitTerminal = true
	}
	fatalDecision := false
	if hasStopVisit {
		step := steps[stopVisit.StepID]
		fatalDecision = len(step.Decisions) != 0 && (stopVisit.Decision != nil && stopVisit.Decision.Error != "" ||
			stopVisit.Decision == nil && stopVisit.State == scheduler.Succeeded)
	}
	if limitTerminal && fatalDecision {
		return fmt.Errorf("остановка по maxVisits не может одновременно ссылаться на ошибку решения")
	}
	if m.RunState != RunRunning && matchingFinishID == "" {
		switch {
		case limitTerminal:
			// Структурное доказательство проверено выше; активные независимые
			// visits допустимы, потому что limit, как и finish, завершает весь run.
		case fatalDecision:
			if m.RunState != RunFailed {
				return fmt.Errorf("ошибка решения может завершить run только как failed")
			}
		case m.RunState == RunFailed:
			if !hasStopVisit || stopVisit.State != scheduler.Failed || advanced[stopVisit.VisitID] {
				return fmt.Errorf("natural failed требует stopVisitId необработанного Failed-посещения")
			}
		case m.RunState == RunSucceeded:
			if m.StopVisitID != "" {
				return fmt.Errorf("natural succeeded не должен содержать stopVisitId")
			}
			for _, visit := range m.Visits {
				if visit.State == scheduler.Failed && !advanced[visit.VisitID] {
					return fmt.Errorf("natural succeeded содержит необработанное Failed-посещение %q", visit.VisitID)
				}
			}
		}
	}
	if m.RunState != RunRunning && len(active) != 0 && matchingFinishID == "" && !fatalDecision && !limitTerminal {
		return fmt.Errorf("терминальный run с незавершёнными visits требует finish, maxVisits или ошибку решения")
	}
	return nil
}

// validateLimitDecisionBarrier сохраняет только те приоритеты planner, которые
// можно доказать и после terminal drain. Незавершённое решение, fatal marker или
// explicit finish обязательно предшествовали бы limit; обычные route допустимы,
// поскольку planner мог отбросить уже собранные activations ради атомарного итога.
func validateLimitDecisionBarrier(history []Visit, steps map[string]workflow.Step) error {
	for _, visit := range history {
		step := steps[visit.StepID]
		if len(step.Decisions) == 0 || visit.Decision != nil && visit.Decision.Applied {
			continue
		}
		if visit.Decision != nil && visit.Decision.Error != "" {
			return fmt.Errorf("посещение %q содержит приоритетную ошибку решения", visit.VisitID)
		}
		if !visitTerminal(visit.State) {
			return fmt.Errorf("посещение %q ещё не завершило decision-wave", visit.VisitID)
		}
		if visit.State != scheduler.Succeeded {
			continue
		}
		if visit.Decision == nil {
			return fmt.Errorf("посещение %q завершилось без обязательного решения", visit.VisitID)
		}
		if route := step.Decisions[visit.Decision.Key]; route.Finish != nil {
			return fmt.Errorf("посещение %q содержит более приоритетный explicit finish", visit.VisitID)
		}
	}
	return nil
}

// validateLimitTrigger доказывает конкретную неслучившуюся активацию N+1, не
// сравнивая её с глобальным приоритетом изменившегося после terminal frontier.
// Terminal visits неизменяемы, а новые visits после остановки запрещены, поэтому
// локальные source/route/FIFO-связи остаются истинными после drain соседей.
func validateLimitTrigger(
	step workflow.Step,
	trigger VisitTrigger,
	iteration int,
	history []Visit,
	seen map[string]Visit,
	steps map[string]workflow.Step,
	numbers map[string]int,
	active map[string]bool,
	afterUses map[afterCause]bool,
	decisionUses map[decisionCause]bool,
) error {
	if active[step.ID] {
		return fmt.Errorf("ограниченный шаг ещё имеет активное посещение")
	}
	switch trigger.Kind {
	case TriggerAfter:
		proof := Visit{VisitID: "limit-proof", StepID: step.ID, Iteration: iteration, Trigger: trigger}
		return validateTrigger(len(history), proof, step, nil, history, seen, steps, afterUses, decisionUses)
	case TriggerDecision:
		if len(trigger.SourceVisitIDs) != 1 || trigger.DecisionKey == "" {
			return fmt.Errorf("decision-limit требует один источник и ключ")
		}
		source, exists := seen[trigger.SourceVisitIDs[0]]
		if !exists || source.State != scheduler.Succeeded || source.Decision == nil || source.Decision.Applied || source.Decision.Error != "" ||
			source.Decision.Key != trigger.DecisionKey {
			return fmt.Errorf("decision-limit ссылается не на готовое неприменённое решение")
		}
		route, exists := steps[source.StepID].Decisions[trigger.DecisionKey]
		if !exists || route.Finish != nil || !slices.Contains(route.To, step.ID) ||
			decisionUses[decisionCause{step.ID, source.VisitID}] || iteration != source.Iteration+1 {
			return fmt.Errorf("decision-limit не совпадает с маршрутом, target или iteration")
		}
		firstLimited := ""
		for _, target := range route.To {
			if active[target] {
				return fmt.Errorf("fanout decision-limit заблокирован активным target %q", target)
			}
			targetStep := steps[target]
			if firstLimited == "" && targetStep.MaxVisits != nil && numbers[target] >= *targetStep.MaxVisits {
				firstLimited = target
			}
		}
		if firstLimited != step.ID {
			return fmt.Errorf("decision-limit не является первой исчерпанной целью route.to")
		}
		return nil
	default:
		return fmt.Errorf("maxVisits не может быть вызван trigger %q", trigger.Kind)
	}
}

// agentVisitViews копирует только семантику, которой владеет scheduler. Внешние
// thread/turn, память и диагностика остаются деталями persistence/coordinator.
// Helper живёт рядом с полной проверкой исходной истории, которую затем передаёт
// pure planner атомарная операция AdvanceAgentGraph.
func agentVisitViews(visits []Visit) []scheduler.AgentVisitView {
	views := make([]scheduler.AgentVisitView, 0, len(visits))
	for _, visit := range visits {
		view := scheduler.AgentVisitView{
			VisitID: visit.VisitID, StepID: visit.StepID, Iteration: visit.Iteration, State: visit.State,
			Trigger: scheduler.AgentTriggerView{
				Kind: scheduler.AgentTriggerKind(visit.Trigger.Kind), SourceVisitIDs: slices.Clone(visit.Trigger.SourceVisitIDs),
				DecisionKey: visit.Trigger.DecisionKey,
			},
		}
		if visit.Decision != nil {
			view.Decision = &scheduler.AgentDecisionView{Key: visit.Decision.Key, Applied: visit.Decision.Applied, Error: visit.Decision.Error}
		}
		views = append(views, view)
	}
	return views
}

// Причины активации сравниваются составными ключами без склейки пользовательских
// stepId: так любые допустимые символы в ID не создают ложных совпадений.
type afterCause struct{ targetStepID, dependencyStepID, sourceVisitID string }
type decisionCause struct{ targetStepID, sourceVisitID string }

// skipReach адресует decision-target внутри одной причинной волны. After имеет
// собственную FIFO-причину и потому не дедуплицируется этим ключом.
type skipReach struct{ waveKey, targetStepID string }

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
	case scheduler.Skipped:
		if visit.CodexThreadID != "" || visit.TurnID != "" || visit.Attempt != 0 || visit.TechnicalError != "" || visit.Decision != nil {
			return fmt.Errorf("Skipped не может содержать результаты запуска")
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

func validateTrigger(
	index int,
	visit Visit,
	step workflow.Step,
	start []string,
	history []Visit,
	seen map[string]Visit,
	steps map[string]workflow.Step,
	afterUses map[afterCause]bool,
	decisionUses map[decisionCause]bool,
) error {
	_, err := validateTriggerWithSkipWaves(
		index, visit, step, start, history, seen, steps,
		afterUses, decisionUses, nil, nil, false, "",
	)
	return err
}

// validateTriggerWithSkipWaves проверяет durable trigger и одновременно выводит
// корни causal skip-wave. Корень — реальный visit решения; вложенный пропущенный
// decision и полностью пропущенный after наследуют тот же канонический набор.
func validateTriggerWithSkipWaves(
	index int,
	visit Visit,
	step workflow.Step,
	start []string,
	history []Visit,
	seen map[string]Visit,
	steps map[string]workflow.Step,
	afterUses map[afterCause]bool,
	decisionUses map[decisionCause]bool,
	skipWaves map[string][]string,
	skipReached map[skipReach]bool,
	terminalRun bool,
	limitDecisionSourceID string,
) ([]string, error) {
	if index < len(start) {
		if visit.StepID != start[index] || visit.Trigger.Kind != TriggerStart || visit.Visit != 1 || visit.Iteration != 1 {
			return nil, fmt.Errorf("начальные посещения должны совпадать с порядком workflow.start")
		}
	}
	switch visit.Trigger.Kind {
	case TriggerStart:
		if index >= len(start) || len(visit.Trigger.SourceVisitIDs) != 0 || visit.Trigger.DecisionKey != "" {
			return nil, fmt.Errorf("start допустим только в начальном префиксе без источников")
		}
	case TriggerAfter:
		if len(step.After) == 0 || len(visit.Trigger.SourceVisitIDs) != len(step.After) || visit.Trigger.DecisionKey != "" {
			return nil, fmt.Errorf("after должен содержать источник для каждого Step.After")
		}
		maxIteration := 0
		allSkipped := true
		causalSkip := false
		var roots []string
		for i, sourceID := range visit.Trigger.SourceVisitIDs {
			source, exists := seen[sourceID]
			if !exists || !visitTerminal(source.State) || source.StepID != step.After[i] {
				return nil, fmt.Errorf("after-источник %q не является нужным завершённым посещением", sourceID)
			}
			cause := afterCause{visit.StepID, step.After[i], sourceID}
			expected := ""
			foundSource := false
			bypassRealSources := terminalRun && visit.State == scheduler.Skipped && source.State == scheduler.Skipped
			for _, candidate := range history {
				candidateCause := afterCause{visit.StepID, step.After[i], candidate.VisitID}
				if candidate.StepID != step.After[i] || afterUses[candidateCause] {
					continue
				}
				if expected == "" {
					expected = candidate.VisitID
				}
				if candidate.VisitID == sourceID {
					foundSource = true
					break
				}
				if candidate.State == scheduler.Skipped {
					bypassRealSources = false
				}
			}
			if !foundSource || sourceID != expected && !bypassRealSources || afterUses[cause] {
				return nil, fmt.Errorf("after должен потреблять самый ранний неиспользованный источник")
			}
			afterUses[cause] = true
			maxIteration = max(maxIteration, source.Iteration)
			allSkipped = allSkipped && source.State == scheduler.Skipped
			if sourceRoots, causal := skipWaves[sourceID]; causal {
				causalSkip = true
				roots = append(roots, sourceRoots...)
			}
		}
		if visit.Iteration != maxIteration {
			return nil, fmt.Errorf("after должен наследовать максимальную iteration источников")
		}
		if visit.State != scheduler.Skipped && allSkipped {
			return nil, fmt.Errorf("after с полностью Skipped-источниками сам должен быть Skipped")
		}
		if visit.State == scheduler.Skipped && allSkipped && causalSkip {
			return canonicalSkipRoots(roots), nil
		}
	case TriggerDecision:
		if len(visit.Trigger.SourceVisitIDs) != 1 || visit.Trigger.DecisionKey == "" {
			return nil, fmt.Errorf("decision требует один источник и ключ")
		}
		source, exists := seen[visit.Trigger.SourceVisitIDs[0]]
		if !exists || source.State != scheduler.Succeeded || source.Decision == nil || !source.Decision.Applied ||
			source.Decision.Key != visit.Trigger.DecisionKey || !slices.Contains(source.Decision.To, visit.StepID) {
			return nil, fmt.Errorf("decision-источник не применял этот маршрут к шагу")
		}
		cause := decisionCause{visit.StepID, source.VisitID}
		if decisionUses[cause] {
			return nil, fmt.Errorf("decision-источник уже материализовал этот target")
		}
		decisionUses[cause] = true
		if visit.Iteration != source.Iteration+1 {
			return nil, fmt.Errorf("decision-target должен увеличить iteration источника")
		}
	case TriggerDecisionSkipped:
		if visit.State != scheduler.Skipped || len(visit.Trigger.SourceVisitIDs) != 1 || visit.Trigger.DecisionKey == "" {
			return nil, fmt.Errorf("decision_skipped требует Skipped-target, один источник и ключ")
		}
		source, exists := seen[visit.Trigger.SourceVisitIDs[0]]
		if !exists {
			return nil, fmt.Errorf("decision_skipped ссылается на неизвестный источник")
		}
		selectedKey, key := "", ""
		var roots []string
		switch source.State {
		case scheduler.Succeeded:
			if source.Decision == nil || source.Decision.Error != "" ||
				!source.Decision.Applied && source.VisitID != limitDecisionSourceID {
				return nil, fmt.Errorf("decision_skipped ссылается не на применённое решение")
			}
			selectedKey = source.Decision.Key
			roots = []string{source.VisitID}
		case scheduler.Skipped:
			if len(steps[source.StepID].Decisions) == 0 || len(skipWaves[source.VisitID]) == 0 {
				return nil, fmt.Errorf("decision_skipped ссылается не на causal decision-источник")
			}
			roots = slices.Clone(skipWaves[source.VisitID])
		default:
			return nil, fmt.Errorf("decision_skipped ссылается не на terminal decision-источник")
		}
		key, exists = canonicalSkippedTargetKey(steps[source.StepID], selectedKey, visit.StepID)
		if !exists || visit.Trigger.DecisionKey != key {
			return nil, fmt.Errorf("decision_skipped не совпадает с пропущенным target решения")
		}
		cause := decisionCause{visit.StepID, source.VisitID}
		if decisionUses[cause] {
			return nil, fmt.Errorf("decision-источник уже материализовал этот target")
		}
		if visit.Iteration != source.Iteration+1 {
			return nil, fmt.Errorf("decision_skipped-target должен увеличить iteration источника")
		}
		waveReach := skipReach{waveKey: skipWaveKey(roots), targetStepID: visit.StepID}
		if skipReached != nil && skipReached[waveReach] {
			return nil, fmt.Errorf("волна пропуска уже достигла target %q", visit.StepID)
		}
		decisionUses[cause] = true
		return roots, nil
	default:
		return nil, fmt.Errorf("неизвестный trigger %q", visit.Trigger.Kind)
	}
	return nil, nil
}

// canonicalSkippedTargetKey выбирает стабильную причину synthetic Skipped.
// Для реального решения рассматриваются только невыбранные ключи; для уже
// пропущенного decision — все routes. Общий target выбранного route не пропускается.
func canonicalSkippedTargetKey(step workflow.Step, selectedKey, target string) (string, bool) {
	if selectedKey != "" && slices.Contains(step.Decisions[selectedKey].To, target) {
		return "", false
	}
	keys := make([]string, 0, len(step.Decisions))
	for key := range step.Decisions {
		if key != selectedKey {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if slices.Contains(step.Decisions[key].To, target) {
			return key, true
		}
	}
	return "", false
}

// canonicalSkipRoots превращает объединение корней join в детерминированное
// множество. Одинаковая причинная волна не зависит от порядка Step.After.
func canonicalSkipRoots(values []string) []string {
	unique := make(map[string]bool, len(values))
	for _, value := range values {
		unique[value] = true
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// skipWaveKey кодирует множество visitId без неоднозначного разделителя.
func skipWaveKey(roots []string) string {
	var result strings.Builder
	for _, root := range roots {
		result.WriteString(strconv.Itoa(len(root)))
		result.WriteByte(':')
		result.WriteString(root)
	}
	return result.String()
}

// validateSkippedVisit отличает synthetic skip работающего графа от Pending,
// который terminal-переход всего run закрыл без запуска Codex.
func validateSkippedVisit(runState RunState, visit Visit, seen map[string]Visit) error {
	if runState != RunRunning || visit.Trigger.Kind == TriggerDecisionSkipped {
		return nil
	}
	if visit.Trigger.Kind != TriggerAfter {
		if visit.State == scheduler.Skipped {
			return fmt.Errorf("работающий run допускает Skipped только из невыбранного route или полностью пропущенного after")
		}
		return nil
	}
	allSkipped := len(visit.Trigger.SourceVisitIDs) != 0
	for _, sourceID := range visit.Trigger.SourceVisitIDs {
		source, exists := seen[sourceID]
		allSkipped = allSkipped && exists && source.State == scheduler.Skipped
	}
	if (visit.State == scheduler.Skipped) != allSkipped {
		return fmt.Errorf("Skipped after должен иметь только Skipped-источники")
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
	return state == scheduler.Failed || state == scheduler.Skipped || state == scheduler.Succeeded
}

func sameOutcome(left, right *workflow.TerminalOutcome) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

const maxStoredTextBytes = 4096

// safeStoredText не даёт непроверенной диагностике сохранить управляющие
// символы для будущего terminal/dashboard. Ограничение защищает и размер meta.
func safeStoredText(value string, required bool) bool {
	if !utf8.ValidString(value) || len(value) > maxStoredTextBytes || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	return !required || strings.TrimSpace(value) != ""
}
