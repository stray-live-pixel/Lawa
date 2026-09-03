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
		// Полная структурная проверка limit выполняется после построения индексов.
		// Здесь ID лишь открывает узкое исключение для skipped-альтернатив того
		// самого неприменённого решения; поддельный proof всё равно будет отклонён.
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
	// ограничивает только исполнения, способные создать Codex turn: иначе один
	// невыбранный маршрут незаметно отнял бы разрешённое реальное посещение.
	numbers, runnableNumbers := make(map[string]int), make(map[string]int)
	lastRunnable, active := make(map[string]Visit), make(map[string]bool)
	afterUses, decisionUses := make(map[afterCause]bool), make(map[decisionCause]bool)
	// skipWaves восстанавливает логическую волну пропуска только из уже
	// сохранённых sourceVisitIds. Отдельного поля в meta.json не требуется:
	// direct decision_skipped начинает волну от реального решения, вложенный
	// decision_skipped наследует её, а полностью пропущенный after объединяет
	// корни своих источников. skipReached запрещает одной волне второй раз
	// обойти тот же step через общий target или цикл.
	skipWaves := make(map[string][]string)
	skipReached := make(map[skipReach]bool)
	advanced := make(map[string]bool)
	cleanupVisits := make(map[string]bool)
	terminalSyntheticVisits := make(map[string]bool)
	seenTerminalSynthetic := false
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
		// Skipped — отдельная причинная ветка без executor, Codex-чата и расхода
		// capacity. Она может быть записана, пока реальный visit того же общего
		// target выполняется по другой причине. No-overlap остаётся обязательным
		// только между visits, которые действительно могут исполнять шаг.
		if active[visit.StepID] && visit.State != scheduler.Skipped {
			return fmt.Errorf("предыдущее посещение шага %q ещё не завершено", visit.StepID)
		}
		if !visitTerminal(visit.State) {
			active[visit.StepID] = true
		}
		roots, triggerErr := validateTriggerWithSkipWaves(
			index, visit, step, s.Workflow.Start, m.Visits[:index], seen, steps,
			afterUses, decisionUses, skipWaves, skipReached, m.RunState != RunRunning, limitDecisionSourceID,
		)
		if triggerErr != nil {
			return fmt.Errorf("посещение %q: %w", visit.VisitID, triggerErr)
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
		if len(roots) != 0 {
			skipWaves[visit.VisitID] = roots
			skipReached[skipReach{waveKey: skipWaveKey(roots), targetStepID: visit.StepID}] = true
		}
		if m.RunState != RunRunning && terminalCleanupWasPending(visit, seen, skipWaves) {
			if seenTerminalSynthetic {
				return fmt.Errorf("посещение %q не могло быть Pending после начала terminal skipped-замыкания", visit.VisitID)
			}
			for _, sourceID := range visit.Trigger.SourceVisitIDs {
				if cleanupVisits[sourceID] || terminalSyntheticVisits[sourceID] {
					return fmt.Errorf("посещение %q не могло быть Pending от post-terminal источника %q", visit.VisitID, sourceID)
				}
			}
			cleanupVisits[visit.VisitID] = true
		}
		if m.RunState != RunRunning && terminalSkippedCreatedAfterOutcome(
			visit, seen, steps, cleanupVisits, terminalSyntheticVisits, limitDecisionSourceID,
		) {
			terminalSyntheticVisits[visit.VisitID] = true
			seenTerminalSynthetic = true
		}
		if m.RunState != RunRunning && visit.State != scheduler.Skipped {
			for _, sourceID := range visit.Trigger.SourceVisitIDs {
				if cleanupVisits[sourceID] || terminalSyntheticVisits[sourceID] {
					return fmt.Errorf("посещение %q использовало post-terminal источник %q", visit.VisitID, sourceID)
				}
			}
		}
		if seenTerminalSynthetic && visit.State != scheduler.Skipped {
			return fmt.Errorf("посещение %q записано после начала terminal skipped-замыкания", visit.VisitID)
		}
		seen[visit.VisitID] = visit
	}
	preTerminalProof := make(map[string]bool)
	if m.RunState != RunRunning {
		if err := validatePreTerminalCleanupPrefix(m.Visits, s.Workflow.Start, steps, cleanupVisits); err != nil {
			return err
		}
		if err := validateTerminalCleanupOccupancy(m.Visits, steps, cleanupVisits); err != nil {
			return err
		}
		preTerminalProof = terminalSourcesBeforeOutcome(m.Visits, seen, cleanupVisits)
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
		if m.StopLimitTrigger.Kind == TriggerDecision && len(m.StopLimitTrigger.SourceVisitIDs) != 1 {
			return fmt.Errorf("остановка по maxVisits содержит неверную форму decision-trigger")
		}
		projectedActive, priorityErr := projectLimitDecisionPriority(
			m.Visits, steps, seen, skipWaves, runnableNumbers, lastRunnable, active, preTerminalProof,
			limitedStep.ID, *m.StopLimitTrigger, m.StopLimitIteration,
		)
		if priorityErr != nil {
			return fmt.Errorf("остановка по maxVisits нарушила порядок planner: %w", priorityErr)
		}
		// Проверяем сохранённую неслучившуюся активацию локально. Полный planner
		// переигрывать нельзя: terminal drain может завершить независимый visit и
		// открыть более ранний маршрут, не меняя уже опубликованный исход.
		if err := validateLimitTrigger(
			limitedStep, *m.StopLimitTrigger, m.StopLimitIteration, m.Visits,
			seen, steps, runnableNumbers, projectedActive, afterUses, decisionUses, skipWaves,
		); err != nil {
			return fmt.Errorf("остановка по maxVisits не подтверждена trigger: %w", err)
		}
		if err := validateUnappliedDecisionAfterPriority(m.Visits, steps, seen, cleanupVisits); err != nil {
			return fmt.Errorf("остановка по maxVisits нарушила причинный порядок decision: %w", err)
		}
		if m.StopLimitTrigger.Kind == TriggerAfter {
			if err := validateAfterLimitPriority(
				s.Workflow.Steps, m.Visits, seen, steps, skipWaves, runnableNumbers,
				lastRunnable, projectedActive, preTerminalProof, limitedStep.ID, *m.StopLimitTrigger,
			); err != nil {
				return fmt.Errorf("остановка по maxVisits нарушила порядок after: %w", err)
			}
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
	if fatalDecision {
		if err := validateImmediateDecisionSourceUses(m.Visits, m.StopVisitID, false); err != nil {
			return fmt.Errorf("ошибка решения уже была продвинута: %w", err)
		}
		if err := validateFatalDecisionPriority(m.Visits, steps, advanced, m.StopVisitID); err != nil {
			return fmt.Errorf("ошибка решения нарушила порядок planner: %w", err)
		}
		if err := validateAppliedDecisionsBeforeTerminalSource(m.Visits, steps, m.StopVisitID); err != nil {
			return fmt.Errorf("ошибка решения подменила порядок decision commit: %w", err)
		}
	}
	if matchingFinishID != "" {
		if err := validateFinishDecisionPriority(m.Visits, steps, seen, skipWaves, matchingFinishID); err != nil {
			return fmt.Errorf("finish нарушает приоритет decision-wave: %w", err)
		}
	}
	if matchingFinishID != "" || limitTerminal {
		for _, visit := range m.Visits {
			if len(steps[visit.StepID].Decisions) == 0 || !terminalCleanupWasPending(visit, seen, skipWaves) {
				continue
			}
			// Finish и maxVisits выбираются только после закрытия decision-wave.
			// Поэтому существующий decision visit нельзя задним числом превратить
			// в rootless cleanup Skipped. After-квитанции, созданные уже closure
			// из одних Skipped sources, helper намеренно не считает прежним Pending.
			return fmt.Errorf("terminal outcome скрыл незавершённый decision visit %q как cleanup Skipped", visit.VisitID)
		}
	}
	if m.RunState != RunRunning {
		if err := validateStoredTerminalSkippedClosure(
			s.Workflow, m.Visits, m.StopLimitStepID, m.StopLimitTrigger,
		); err != nil {
			return fmt.Errorf("terminal skipped-замыкание повреждено: %w", err)
		}
	}
	if m.RunState != RunRunning && matchingFinishID == "" && !limitTerminal && !fatalDecision {
		// Natural terminal возникает только после полного исчерпания causal frontier:
		// у него не бывает Pending, которые terminal-переход мог бы закрыть как
		// Skipped. Поэтому здесь допустимы лишь synthetic decision_skipped и after
		// из полностью пропущенных источников. Повторная running-проверка не даёт
		// подменить start/selected visit и выдать невыполненный run за успешный.
		for _, visit := range m.Visits {
			// Успешный decision-turn ещё не завершает свою причинную работу. Пока
			// выбор отсутствует, конфликтен или не Applied, planner обязан либо
			// сохранить fatal outcome, либо материализовать route/finish. Natural
			// outcome в этот момент позволил бы одной правкой runState скрыть целую
			// ветку без её durable перехода.
			if len(steps[visit.StepID].Decisions) != 0 && visit.State == scheduler.Succeeded &&
				(visit.Decision == nil || visit.Decision.Error != "" || !visit.Decision.Applied) {
				return fmt.Errorf("natural terminal содержит неприменённое решение посещения %q", visit.VisitID)
			}
			if err := validateSkippedVisit(RunRunning, visit, seen); err != nil {
				return fmt.Errorf("natural terminal содержит необоснованный skipped: посещение %q: %w", visit.VisitID, err)
			}
		}
		if err := validateNaturalTerminalQuiescence(s.Workflow, m.Visits, m.RunState, m.StopVisitID); err != nil {
			return fmt.Errorf("natural terminal не подтверждён planner: %w", err)
		}
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

// terminalRootlessAfterReceipt отличает Skipped, который terminal closure создал
// уже после выбора исхода, от существовавшего Pending visit, лишь переведённого
// в cleanup. Такая квитанция всегда имеет after-trigger только из Skipped
// sources и не получает causal roots, если вся её причина была rootless.
func terminalRootlessAfterReceipt(visit Visit, seen map[string]Visit, skipWaves map[string][]string) bool {
	if visit.State != scheduler.Skipped || len(skipWaves[visit.VisitID]) != 0 || visit.Trigger.Kind != TriggerAfter ||
		len(visit.Trigger.SourceVisitIDs) == 0 {
		return false
	}
	for _, sourceID := range visit.Trigger.SourceVisitIDs {
		source, exists := seen[sourceID]
		if !exists || source.State != scheduler.Skipped {
			return false
		}
	}
	return true
}

// terminalCleanupWasPending восстанавливает минимальную pre-terminal
// занятость. Causal Skipped и after-квитанции появились как синтетические
// terminal-записи; остальные rootless Skipped могли возникнуть только из уже
// существовавшего Pending, который markPendingVisitsSkipped закрыл вместе с run.
func terminalCleanupWasPending(visit Visit, seen map[string]Visit, skipWaves map[string][]string) bool {
	return visit.State == scheduler.Skipped && len(skipWaves[visit.VisitID]) == 0 &&
		!terminalRootlessAfterReceipt(visit, seen, skipWaves)
}

// terminalSkippedCreatedAfterOutcome отмечает доказанную post-terminal часть
// замыкания. Direct alternatives terminal finish/decision-limit появляются в
// одном commit с outcome; after-потомки cleanup или уже отмеченной записи —
// только после того, как terminal-переход сделал их источники Skipped. Такой
// visit не может служить причиной якобы существовавшего до outcome Pending.
func terminalSkippedCreatedAfterOutcome(
	visit Visit,
	seen map[string]Visit,
	steps map[string]workflow.Step,
	cleanupVisits map[string]bool,
	terminalSyntheticVisits map[string]bool,
	limitDecisionSourceID string,
) bool {
	if visit.State != scheduler.Skipped {
		return false
	}
	switch visit.Trigger.Kind {
	case TriggerDecisionSkipped:
		if len(visit.Trigger.SourceVisitIDs) != 1 {
			return false
		}
		sourceID := visit.Trigger.SourceVisitIDs[0]
		if sourceID == limitDecisionSourceID || cleanupVisits[sourceID] || terminalSyntheticVisits[sourceID] {
			return true
		}
		source, exists := seen[sourceID]
		if !exists || source.Decision == nil || !source.Decision.Applied {
			return false
		}
		route, exists := steps[source.StepID].Decisions[source.Decision.Key]
		return exists && route.Finish != nil
	case TriggerAfter:
		for _, sourceID := range visit.Trigger.SourceVisitIDs {
			if cleanupVisits[sourceID] || terminalSyntheticVisits[sourceID] {
				return true
			}
		}
	}
	return false
}

// validatePreTerminalCleanupPrefix ретроспективно проверяет обычными running-
// правилами весь префикс до последнего доказанного cleanup. Terminal closure
// только дописывает новые Skipped в конец истории, поэтому каждая более ранняя
// запись уже существовала, когда cleanup ещё был Pending. Это запрещает ранней
// synthetic-квитанции воспользоваться terminal FIFO-bypass, а затем выдать
// обойдённый источник за более поздний cleanup того же старого snapshot.
func validatePreTerminalCleanupPrefix(
	history []Visit,
	start []string,
	steps map[string]workflow.Step,
	cleanupVisits map[string]bool,
) error {
	lastCleanupIndex := -1
	for index, visit := range history {
		if cleanupVisits[visit.VisitID] {
			lastCleanupIndex = index
		}
	}
	if lastCleanupIndex < 0 {
		return nil
	}
	seen := make(map[string]Visit)
	afterUses := make(map[afterCause]bool)
	decisionUses := make(map[decisionCause]bool)
	skipWaves := make(map[string][]string)
	skipReached := make(map[skipReach]bool)
	for index := 0; index <= lastCleanupIndex; index++ {
		visit := history[index]
		if cleanupVisits[visit.VisitID] {
			visit.State = scheduler.Pending
		}
		step := steps[visit.StepID]
		roots, err := validateTriggerWithSkipWaves(
			index, visit, step, start, history[:index], seen, steps,
			afterUses, decisionUses, skipWaves, skipReached, false, "",
		)
		if err != nil {
			return fmt.Errorf("посещение %q не подтверждает pre-terminal history: %w", visit.VisitID, err)
		}
		if err := validateSkippedVisit(RunRunning, visit, seen); err != nil {
			return fmt.Errorf("посещение %q не подтверждает pre-terminal history: %w", visit.VisitID, err)
		}
		if len(roots) != 0 {
			skipWaves[visit.VisitID] = roots
			skipReached[skipReach{waveKey: skipWaveKey(roots), targetStepID: visit.StepID}] = true
		}
		seen[visit.VisitID] = visit
	}
	return nil
}

// validateTerminalCleanupOccupancy мысленно возвращает rootless cleanup в его
// исходное состояние Pending. Такая активация расходовала maxVisits и держала
// no-overlap вплоть до terminal commit. Causal Skipped и созданные замыканием
// after-квитанции в реконструкцию не входят: у них никогда не было executor.
func validateTerminalCleanupOccupancy(
	history []Visit,
	steps map[string]workflow.Step,
	cleanupVisits map[string]bool,
) error {
	counts := make(map[string]int)
	active := make(map[string]string)
	for _, visit := range history {
		cleanup := cleanupVisits[visit.VisitID]
		if visit.State == scheduler.Skipped && !cleanup {
			continue
		}
		if previousID := active[visit.StepID]; previousID != "" {
			return fmt.Errorf("посещение %q не могло существовать одновременно с pre-terminal Pending %q", visit.VisitID, previousID)
		}
		counts[visit.StepID]++
		step := steps[visit.StepID]
		if step.MaxVisits != nil && counts[visit.StepID] > *step.MaxVisits {
			return fmt.Errorf("посещение %q не могло быть Pending: pre-terminal maxVisits=%d шага %q уже исчерпан", visit.VisitID, *step.MaxVisits, visit.StepID)
		}
		if cleanup || !visitTerminal(visit.State) {
			active[visit.StepID] = visit.VisitID
		}
	}
	return nil
}

// terminalSourcesBeforeOutcome собирает причинные факты, устойчивые к drain.
// Любой non-Skipped visit был создан до terminal outcome, а доказанный cleanup
// существовал тогда как Pending. Их trigger мог быть выбран только после того,
// как все sources стали terminal. Сами cleanup не добавляются: до outcome они,
// наоборот, оставались активными и не доказывают свободу своего шага.
func terminalSourcesBeforeOutcome(
	history []Visit,
	seen map[string]Visit,
	cleanupVisits map[string]bool,
) map[string]bool {
	seeds := make([]string, 0)
	for _, visit := range history {
		if visit.State == scheduler.Skipped && !cleanupVisits[visit.VisitID] {
			continue
		}
		seeds = append(seeds, visit.Trigger.SourceVisitIDs...)
	}
	return terminalCausalAncestors(seeds, seen)
}

// validateAppliedDecisionsBeforeTerminalSource проверяет границу атомарных
// decision-коммитов. Если обычное Applied-решение материализовало target уже
// после независимого terminal decision visit, оно не могло быть применено:
// незавершённый источник держал бы wave-barrier, а завершённый finish/fatal
// получил бы приоритет. Единственное допустимое исключение —
// само Applied-решение создало terminal source вместе с соседями своего fanout;
// тогда target-ы одного атомарного коммита законно окружают его в журнале.
func validateAppliedDecisionsBeforeTerminalSource(
	history []Visit,
	steps map[string]workflow.Step,
	terminalSourceID string,
) error {
	terminalIndex := -1
	targetIndices := make(map[decisionCause]int)
	targetVisitIDs := make(map[decisionCause]string)
	for index, visit := range history {
		if visit.VisitID == terminalSourceID {
			terminalIndex = index
		}
		if visit.Trigger.Kind == TriggerDecision && len(visit.Trigger.SourceVisitIDs) == 1 {
			cause := decisionCause{targetStepID: visit.StepID, sourceVisitID: visit.Trigger.SourceVisitIDs[0]}
			targetIndices[cause] = index
			targetVisitIDs[cause] = visit.VisitID
		}
	}
	if terminalIndex < 0 {
		return fmt.Errorf("terminal source %q отсутствует в истории", terminalSourceID)
	}
	for _, visit := range history {
		if visit.Decision == nil || !visit.Decision.Applied {
			continue
		}
		route := steps[visit.StepID].Decisions[visit.Decision.Key]
		if route.Finish != nil {
			continue
		}
		createdTerminalSource := false
		latestTargetIndex := -1
		for _, target := range route.To {
			cause := decisionCause{targetStepID: target, sourceVisitID: visit.VisitID}
			index, exists := targetIndices[cause]
			if !exists {
				// Полноту Applied отдельно проверяет основной валидатор. Здесь
				// сохраняем локальную диагностику и не полагаемся на порядок
				// вызова двух проверок в будущем.
				return fmt.Errorf("Applied-решение %q не материализовало target %q", visit.VisitID, target)
			}
			latestTargetIndex = max(latestTargetIndex, index)
			createdTerminalSource = createdTerminalSource || targetVisitIDs[cause] == terminalSourceID
		}
		if latestTargetIndex > terminalIndex && !createdTerminalSource {
			return fmt.Errorf("Applied-решение %q материализовано после terminal source %q", visit.VisitID, terminalSourceID)
		}
	}
	return nil
}

// validateImmediateDecisionSourceUses запрещает downstream-работу от решения,
// которое само немедленно завершает run. Finish и fatal выбираются до обычной
// after-фазы, поэтому их visit не мог быть потреблён таким trigger. Единственное
// исключение для finish — direct decision_skipped alternatives, создаваемые в
// том же terminal commit; fatal не знает выбранного ключа и не создаёт даже их.
func validateImmediateDecisionSourceUses(history []Visit, sourceVisitID string, allowDecisionSkipped bool) error {
	for _, visit := range history {
		if !slices.Contains(visit.Trigger.SourceVisitIDs, sourceVisitID) {
			continue
		}
		if allowDecisionSkipped && visit.Trigger.Kind == TriggerDecisionSkipped {
			continue
		}
		return fmt.Errorf("посещение %q с trigger %q использовало terminal decision source %q", visit.VisitID, visit.Trigger.Kind, sourceVisitID)
	}
	return nil
}

// validateFatalDecisionPriority сохраняет только устойчивую часть первого
// прохода planner. Decision.Error записывается до terminal outcome и больше не
// меняется, поэтому более ранний durable conflict обязан победить. Напротив,
// независимый Running visit может уже во время terminal drain стать Succeeded
// без choose_decision; финальный журнал не доказывает, что такой missing-choice
// существовал раньше сохранённой причины, и не должен инвалидировать outcome.
// Но downstream-ссылка на такой visit является неизменяемым доказательством:
// planner увидел его terminal ещё до создания потомка и обязан был остановиться
// на отсутствующем выборе до after-фазы и до более поздней fatal-причины.
func validateFatalDecisionPriority(
	history []Visit,
	steps map[string]workflow.Step,
	advanced map[string]bool,
	stopVisitID string,
) error {
	// Missing-choice мог появиться у независимого Running visit уже во время
	// drain. Но если от него сохранился хотя бы один потомок, visit был terminal
	// ещё до отдельного planning commit. Тогда after-фаза недостижима независимо
	// от положения выбранной fatal-причины в append-only истории.
	for _, visit := range history {
		if len(steps[visit.StepID].Decisions) != 0 && visit.State == scheduler.Succeeded &&
			visit.Decision == nil && advanced[visit.VisitID] {
			return fmt.Errorf("посещение %q завершилось без обязательного решения и уже породило downstream visit", visit.VisitID)
		}
	}
	for _, visit := range history {
		if len(steps[visit.StepID].Decisions) == 0 {
			continue
		}
		if visit.VisitID == stopVisitID {
			return nil
		}
		if visit.Decision != nil && visit.Decision.Error != "" {
			return fmt.Errorf("сохранена ошибка %q, но planner первым выбрал бы durable conflict %q", stopVisitID, visit.VisitID)
		}
	}
	return fmt.Errorf("terminal source %q отсутствует среди decision visits", stopVisitID)
}

// validateFinishDecisionPriority восстанавливает монотонную часть состояния до
// terminal commit. Результаты decision-turn неизменяемы после завершения, а
// finish выбирается только после закрытия всей wave и до применения обычных
// routes. Поэтому сохранённый finish обязан быть первым finish по durable order,
// и никакой более ранний fatal/open decision не мог быть скрыт cleanup-флагом.
func validateFinishDecisionPriority(
	history []Visit,
	steps map[string]workflow.Step,
	seen map[string]Visit,
	skipWaves map[string][]string,
	matchingFinishID string,
) error {
	if err := validateImmediateDecisionSourceUses(history, matchingFinishID, true); err != nil {
		return err
	}
	if err := validateAppliedDecisionsBeforeTerminalSource(history, steps, matchingFinishID); err != nil {
		return err
	}
	firstFinishID := ""
	for _, visit := range history {
		step := steps[visit.StepID]
		if len(step.Decisions) == 0 || terminalRootlessAfterReceipt(visit, seen, skipWaves) {
			continue
		}
		decision := visit.Decision
		if decision != nil && decision.Error != "" {
			return fmt.Errorf("посещение %q содержит более приоритетную ошибку решения", visit.VisitID)
		}
		if visit.State == scheduler.Succeeded && decision == nil {
			return fmt.Errorf("посещение %q завершилось без обязательного решения", visit.VisitID)
		}
		if visit.VisitID != matchingFinishID && (decision == nil || !decision.Applied) && !visitTerminal(visit.State) {
			return fmt.Errorf("посещение %q ещё не завершило decision-wave", visit.VisitID)
		}
		if terminalCleanupWasPending(visit, seen, skipWaves) {
			return fmt.Errorf("посещение %q было Pending до terminal cleanup", visit.VisitID)
		}
		if visit.State != scheduler.Succeeded || decision == nil {
			continue
		}
		// Другие Applied routes относятся к уже опубликованным прошлым волнам.
		// Applied matching finish мысленно возвращается в pre-commit состояние;
		// все остальные текущие решения рассматриваются только если ещё unapplied.
		if decision.Applied && visit.VisitID != matchingFinishID {
			continue
		}
		if route := step.Decisions[decision.Key]; route.Finish != nil && firstFinishID == "" {
			firstFinishID = visit.VisitID
		}
	}
	if firstFinishID != matchingFinishID {
		return fmt.Errorf("сохранён finish %q, но planner первым выбрал бы %q", matchingFinishID, firstFinishID)
	}
	return nil
}

// validateNaturalTerminalQuiescence переигрывает чистый planner на уже
// проверенной истории и доказывает, что metadata не скрыла готовую работу.
// Простого отсутствия active visits недостаточно: завершённый source может уже
// открыть after-recovery, а успешный decision — ещё не применённый route.
//
// Старые terminal snapshots могли быть записаны до появления synthetic
// decision_skipped. Для обратной совместимости безопасный skip-only repair
// разрешён только истории вообще без сохранённых Skipped и не моделируется
// дальше: виртуальный backfill мог бы задним числом открыть смешанный join,
// которого старый runtime никогда не видел. Наличие хотя бы одного Skipped уже
// доказывает новую семантику с обязательным полным closure. Любая готовая
// runnable activation, Applied-флаг или cleanup существующего Pending в исходной
// истории также доказывает преждевременный natural outcome.
func validateNaturalTerminalQuiescence(
	w workflow.Workflow,
	visits []Visit,
	runState RunState,
	stopVisitID string,
) error {
	hasStoredSkipped := false
	for _, visit := range visits {
		hasStoredSkipped = hasStoredSkipped || visit.State == scheduler.Skipped
	}
	plan, err := scheduler.PlanAgentGraph(w, agentVisitViews(visits))
	if err != nil {
		return err
	}
	if len(plan.ApplyDecisionVisitIDs) != 0 || len(plan.MarkSkippedVisitIDs) != 0 {
		return fmt.Errorf("история требует применения решения или terminal cleanup")
	}
	activations := append(slices.Clone(plan.DecisionActivations), plan.AfterActivations...)
	for _, activation := range activations {
		if activation.InitialState != scheduler.Skipped {
			return fmt.Errorf("история содержит готовую активацию шага %q", activation.StepID)
		}
	}
	if plan.Terminal == nil {
		if len(activations) != 0 {
			// Это допустимый legacy snapshot, которому не хватает только новых
			// synthetic Skipped; Load не переписывает его задним числом. Уже
			// сохранённый Skipped, напротив, доказывает новый формат поведения,
			// где частичное closure является повреждением, а не legacy-историей.
			if !hasStoredSkipped {
				return nil
			}
			return fmt.Errorf("новая история содержит неполное skipped-замыкание")
		}
		return fmt.Errorf("planner ещё ожидает незавершённую работу")
	}
	if len(activations) != 0 {
		return fmt.Errorf("planner смешал terminal outcome и skipped-backfill")
	}
	expected := workflow.OutcomeSucceeded
	if runState == RunFailed {
		expected = workflow.OutcomeFailed
	}
	if plan.Terminal.Outcome != expected || plan.Terminal.CauseVisitID != stopVisitID || plan.Terminal.LimitStepID != "" {
		return fmt.Errorf("planner получил другой outcome или источник остановки")
	}
	return nil
}

// validateStoredTerminalSkippedClosure отличает legacy terminal snapshots от
// новой семантики по первому сохранённому Skipped. Старый v4 мог вообще не
// материализовать невыбранные ветки и потому остаётся читаемым. Но если хотя бы
// один Skipped уже присутствует, terminal commit обязан был тем же Rename
// закрыть все Pending и довести причинное замыкание до неподвижной точки:
// обычный Advance после outcome больше недоступен и исправить пробел нельзя.
func validateStoredTerminalSkippedClosure(
	w workflow.Workflow,
	visits []Visit,
	limitStepID string,
	limitTrigger *VisitTrigger,
) error {
	hasStoredSkipped := false
	for _, visit := range visits {
		hasStoredSkipped = hasStoredSkipped || visit.State == scheduler.Skipped
	}
	if !hasStoredSkipped {
		return nil
	}
	for _, visit := range visits {
		if visit.State == scheduler.Pending {
			return fmt.Errorf("новый terminal snapshot оставил Pending-посещение %q", visit.VisitID)
		}
	}
	trigger := scheduler.AgentTriggerView{}
	if limitTrigger != nil {
		trigger = scheduler.AgentTriggerView{
			Kind: scheduler.AgentTriggerKind(limitTrigger.Kind), SourceVisitIDs: slices.Clone(limitTrigger.SourceVisitIDs),
			DecisionKey: limitTrigger.DecisionKey,
		}
	}
	closure, err := scheduler.PlanAgentSkippedClosure(w, agentVisitViews(visits), true, limitStepID, trigger)
	if err != nil {
		return err
	}
	if len(closure.ApplyDecisionVisitIDs) != 0 || len(closure.MarkSkippedVisitIDs) != 0 ||
		len(closure.DecisionActivations) != 0 || len(closure.AfterActivations) != 0 || closure.Terminal != nil {
		return fmt.Errorf("новая terminal history содержит неполное skipped-замыкание")
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

// projectLimitDecisionPriority доказывает только монотонную часть decision-фазы
// до заявленного maxVisits. Полный replay после terminal небезопасен: Running
// target мог завершиться во время drain и сейчас лишь выглядеть свободным. Такой
// terminal real visit вне заявленного fanout считается потенциальным прежним
// блокером, поэтому раннее решение консервативно пропускается.
//
// Свобода target доказана, если он входит в заявленную активацию, ещё ни разу не
// исполнялся, принадлежит decision-step после закрытого wave-barrier либо его
// последнее исполнение является причинным предком limit-trigger. На этой узкой
// основе можно безопасно восстановить ранние reservations: они отклоняют
// подменённый поздний limit, но не инвалидируют честный outcome после drain.
func projectLimitDecisionPriority(
	history []Visit,
	steps map[string]workflow.Step,
	seen map[string]Visit,
	skipWaves map[string][]string,
	runnableNumbers map[string]int,
	lastRunnable map[string]Visit,
	active map[string]bool,
	preTerminalProof map[string]bool,
	limitStepID string,
	limitTrigger VisitTrigger,
	limitIteration int,
) (map[string]bool, error) {
	projectedActive := make(map[string]bool, len(active))
	for stepID := range active {
		projectedActive[stepID] = true
	}
	// Rootless cleanup существовал как Pending непосредственно перед terminal
	// commit и тогда занимал step. Синтетические causal skips и after-квитанции,
	// напротив, появились уже после выбора исхода и не резервировали executor.
	for _, visit := range history {
		if terminalCleanupWasPending(visit, seen, skipWaves) {
			projectedActive[visit.StepID] = true
		}
	}

	claimIsDecision := limitTrigger.Kind == TriggerDecision
	claimedFree := map[string]bool{limitStepID: true}
	if claimIsDecision && len(limitTrigger.SourceVisitIDs) == 1 {
		if source, exists := seen[limitTrigger.SourceVisitIDs[0]]; exists {
			if route, routeExists := steps[source.StepID].Decisions[limitTrigger.DecisionKey]; routeExists {
				for _, target := range route.To {
					claimedFree[target] = true
				}
			}
		}
	}
	proofSeeds := slices.Clone(limitTrigger.SourceVisitIDs)
	if last, exists := lastRunnable[limitStepID]; exists {
		proofSeeds = append(proofSeeds, last.VisitID)
	}
	provenTerminal := terminalCausalAncestors(proofSeeds, seen)
	for visitID := range preTerminalProof {
		provenTerminal[visitID] = true
	}
	definitelyFree := func(stepID string) bool {
		if projectedActive[stepID] {
			return false
		}
		if claimedFree[stepID] || runnableNumbers[stepID] == 0 || len(steps[stepID].Decisions) != 0 {
			return true
		}
		last, exists := lastRunnable[stepID]
		return exists && provenTerminal[last.VisitID]
	}
	for _, visit := range history {
		step := steps[visit.StepID]
		if len(step.Decisions) == 0 || visit.Decision == nil || visit.Decision.Applied ||
			visit.Decision.Error != "" || visit.State != scheduler.Succeeded ||
			terminalRootlessAfterReceipt(visit, seen, skipWaves) {
			continue
		}
		route := step.Decisions[visit.Decision.Key]
		if route.Finish != nil {
			// validateLimitDecisionBarrier выдаст более точную ошибку; здесь
			// terminal route не участвует в обычном projected fan-out.
			continue
		}
		blocked, ambiguous := false, false
		for _, target := range route.To {
			if projectedActive[target] {
				blocked = true
				break
			}
			if !definitelyFree(target) {
				// После terminal drain нельзя отличить завершившийся Running
				// visit от уже свободного target. Возможного блокера достаточно,
				// чтобы не объявлять более ранний route неизбежным.
				ambiguous = true
				break
			}
		}
		if blocked || ambiguous {
			continue
		}
		for _, target := range route.To {
			targetStep := steps[target]
			if targetStep.MaxVisits == nil || runnableNumbers[target] < *targetStep.MaxVisits {
				continue
			}
			matchesClaim := claimIsDecision && limitStepID == target && limitIteration == visit.Iteration+1 &&
				limitTrigger.DecisionKey == visit.Decision.Key &&
				slices.Equal(limitTrigger.SourceVisitIDs, []string{visit.VisitID})
			if matchesClaim {
				return projectedActive, nil
			}
			return nil, fmt.Errorf("раньше заявленной причины planner встретил maxVisits шага %q из решения %q", target, visit.VisitID)
		}
		for _, target := range route.To {
			projectedActive[target] = true
		}
	}
	if claimIsDecision {
		return nil, fmt.Errorf("заявленное decision-limit не достигалось с учётом ранних reservations")
	}
	return projectedActive, nil
}

// terminalCausalAncestors возвращает visits, которые гарантированно были
// terminal до появления заданных sources. Trigger всегда ссылается назад на
// уже завершённую причину, поэтому транзитивный обход не зависит от того, какие
// независимые Running visits успели завершиться позже во время terminal drain.
func terminalCausalAncestors(sourceVisitIDs []string, seen map[string]Visit) map[string]bool {
	result := make(map[string]bool)
	stack := slices.Clone(sourceVisitIDs)
	for len(stack) != 0 {
		last := len(stack) - 1
		visitID := stack[last]
		stack = stack[:last]
		if result[visitID] {
			continue
		}
		visit, exists := seen[visitID]
		if !exists {
			continue
		}
		result[visitID] = true
		stack = append(stack, visit.Trigger.SourceVisitIDs...)
	}
	return result
}

// validateUnappliedDecisionAfterPriority проверяет durable after-активации,
// которые не могли появиться при уже готовом более раннем decision-limit.
// DecisionActivation и AfterActivation одного плана записываются именно в таком
// порядке, поэтому одного append-index решения недостаточно: его выбор мог быть
// получен позже. Решение точно существовало на входе after-плана, если оно start
// либо хотя бы один source candidate был записан не раньше decision visit —
// planner не умеет ссылаться на активации, создаваемые в том же плане.
//
// Непосредственно перед candidate все его causal ancestors уже terminal. Если
// они содержат последнюю occupancy каждого target, decision-фаза обязана была
// применить route либо завершить run по заполненной квоте и не дойти до after.
// Сам candidate дополнительно доказывает свободу собственного step. Cleanup,
// ранее строго доказанный как Pending, участвует и в capacity, и в no-overlap.
func validateUnappliedDecisionAfterPriority(
	history []Visit,
	steps map[string]workflow.Step,
	seen map[string]Visit,
	cleanupVisits map[string]bool,
) error {
	indices := make(map[string]int, len(history))
	for index, visit := range history {
		indices[visit.VisitID] = index
	}
	counts := make(map[string]int)
	lastOccupancy := make(map[string]string)
	for candidateIndex, candidate := range history {
		candidateExisted := candidate.State != scheduler.Skipped || cleanupVisits[candidate.VisitID]
		if candidateExisted && candidate.Trigger.Kind == TriggerAfter {
			terminalBeforeCreation := terminalCausalAncestors(candidate.Trigger.SourceVisitIDs, seen)
			for decisionIndex := 0; decisionIndex < candidateIndex; decisionIndex++ {
				decisionVisit := history[decisionIndex]
				decisionStep := steps[decisionVisit.StepID]
				decision := decisionVisit.Decision
				if len(decisionStep.Decisions) == 0 || decisionVisit.State != scheduler.Succeeded ||
					decision == nil || decision.Applied || decision.Error != "" {
					continue
				}
				route := decisionStep.Decisions[decision.Key]
				if route.Finish != nil {
					continue
				}
				decisionPreexisted := decisionVisit.Trigger.Kind == TriggerStart
				for _, sourceID := range candidate.Trigger.SourceVisitIDs {
					decisionPreexisted = decisionPreexisted || indices[sourceID] >= decisionIndex
				}
				if !decisionPreexisted {
					continue
				}
				firstLimitedTarget := ""
				allTargetsFree := true
				for _, target := range route.To {
					targetStep := steps[target]
					if firstLimitedTarget == "" && targetStep.MaxVisits != nil && counts[target] >= *targetStep.MaxVisits {
						firstLimitedTarget = target
					}
					// Создание самого after-candidate доказывает, что его step был
					// свободен непосредственно перед after-фазой, даже когда
					// прежняя occupancy не входит в causal ancestry trigger.
					if target == candidate.StepID {
						continue
					}
					if occupancyID := lastOccupancy[target]; occupancyID != "" && !terminalBeforeCreation[occupancyID] {
						allTargetsFree = false
					}
				}
				if !allTargetsFree {
					continue
				}
				if firstLimitedTarget != "" {
					return fmt.Errorf("посещение %q создано после доказанного decision-limit source %q для шага %q",
						candidate.VisitID, decisionVisit.VisitID, firstLimitedTarget)
				}
				return fmt.Errorf("посещение %q создано раньше обязательного применения решения %q",
					candidate.VisitID, decisionVisit.VisitID)
			}
		}
		if candidateExisted {
			counts[candidate.StepID]++
			lastOccupancy[candidate.StepID] = candidate.VisitID
		}
	}
	return nil
}

// validateAfterLimitPriority проверяет порядок Workflow.Steps только на
// неизменяемо доказуемом pre-terminal frontier. Exact sources заявленного
// after-limit и их причинные предки уже были terminal. Дополнительно любой
// durable runnable-потомок доказывает, что его sources завершились ещё до
// outcome; поэтому последнее исполнение target, использованное таким потомком,
// тоже было свободно до claimed limit. Сам по себе независимый Succeeded visit
// доказательством не является: он мог завершиться лишь во время drain.
func validateAfterLimitPriority(
	workflowSteps []workflow.Step,
	history []Visit,
	seen map[string]Visit,
	steps map[string]workflow.Step,
	skipWaves map[string][]string,
	runnableNumbers map[string]int,
	lastRunnable map[string]Visit,
	projectedActive map[string]bool,
	preTerminalProof map[string]bool,
	limitStepID string,
	limitTrigger VisitTrigger,
) error {
	proofSeeds := slices.Clone(limitTrigger.SourceVisitIDs)
	if last, exists := lastRunnable[limitStepID]; exists {
		proofSeeds = append(proofSeeds, last.VisitID)
	}
	provenTerminal := terminalCausalAncestors(proofSeeds, seen)
	for visitID := range preTerminalProof {
		provenTerminal[visitID] = true
	}
	preTerminalAppendBoundary := -1
	for index, visit := range history {
		if slices.Contains(proofSeeds, visit.VisitID) {
			preTerminalAppendBoundary = max(preTerminalAppendBoundary, index)
		}
	}
	preTerminalUses := make(map[afterCause]bool)
	for visitIndex, visit := range history {
		if visit.Trigger.Kind != TriggerAfter {
			continue
		}
		// Реальный visit и Pending, превращённый в cleanup, точно были
		// созданы до outcome. Synthetic Skipped мог появиться уже в
		// terminal closure; учитываем его только как доказанного предка.
		createdBeforeTerminal := visitIndex < preTerminalAppendBoundary || visit.State != scheduler.Skipped || provenTerminal[visit.VisitID] ||
			terminalCleanupWasPending(visit, seen, skipWaves)
		if !createdBeforeTerminal {
			continue
		}
		step := steps[visit.StepID]
		for index, dependency := range step.After {
			if index >= len(visit.Trigger.SourceVisitIDs) {
				break
			}
			preTerminalUses[afterCause{
				targetStepID: visit.StepID, dependencyStepID: dependency,
				sourceVisitID: visit.Trigger.SourceVisitIDs[index],
			}] = true
		}
	}

	for _, step := range workflowSteps {
		if step.ID == limitStepID {
			// validateLimitTrigger уже полностью доказал локальную причину
			// заявленного шага. Здесь проверяется только существование более
			// раннего неизбежного limit; повторная реконструкция claim по
			// финальному журналу неоднозначна из-за skipped-квитанций.
			return nil
		}
		if len(step.After) == 0 {
			continue
		}
		sources := make([]string, 0, len(step.After))
		iteration, allSkipped, ready := 0, true, true
		for _, dependency := range step.After {
			var source Visit
			found := false
			for _, candidate := range history {
				cause := afterCause{targetStepID: step.ID, dependencyStepID: dependency, sourceVisitID: candidate.VisitID}
				if candidate.StepID != dependency || preTerminalUses[cause] {
					continue
				}
				source, found = candidate, true
				break
			}
			if !found || !provenTerminal[source.VisitID] || !visitTerminal(source.State) {
				ready = false
				break
			}
			sources = append(sources, source.VisitID)
			iteration = max(iteration, source.Iteration)
			allSkipped = allSkipped && source.State == scheduler.Skipped
		}
		if !ready {
			continue
		}
		if projectedActive[step.ID] {
			continue
		}
		last, hasLast := lastRunnable[step.ID]
		targetDefinitelyFree := runnableNumbers[step.ID] == 0 ||
			len(step.Decisions) != 0 || hasLast && provenTerminal[last.VisitID]
		if !targetDefinitelyFree {
			continue
		}
		if allSkipped {
			continue
		}
		if step.MaxVisits == nil || runnableNumbers[step.ID] < *step.MaxVisits {
			// Такая активация была бы накоплена в обычном плане, но более
			// поздний limit всё равно атомарно отбросил бы её.
			continue
		}
		return fmt.Errorf("раньше заявленной причины planner встретил after-limit шага %q", step.ID)
	}
	return fmt.Errorf("заявленный after-limit ссылается на отсутствующий workflow step %q", limitStepID)
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
	runnableNumbers map[string]int,
	active map[string]bool,
	afterUses map[afterCause]bool,
	decisionUses map[decisionCause]bool,
	skipWaves map[string][]string,
) error {
	if active[step.ID] {
		return fmt.Errorf("ограниченный шаг ещё имеет активное посещение")
	}
	switch trigger.Kind {
	case TriggerAfter:
		// Exact source может быть causal Skipped и выглядеть корректно сам по
		// себе, но его mixed join мог появиться лишь в terminal closure после
		// превращения Pending-предка в rootless cleanup. Вся причинная цепочка
		// after-limit обязана существовать до outcome; иначе proof использует
		// результат самой остановки для её обоснования.
		for visitID := range terminalCausalAncestors(trigger.SourceVisitIDs, seen) {
			ancestor := seen[visitID]
			if ancestor.State == scheduler.Skipped && len(skipWaves[visitID]) == 0 {
				return fmt.Errorf("after-limit зависит от rootless terminal cleanup %q", visitID)
			}
		}
		allSkipped := len(trigger.SourceVisitIDs) != 0
		for _, sourceID := range trigger.SourceVisitIDs {
			source, exists := seen[sourceID]
			allSkipped = allSkipped && exists && source.State == scheduler.Skipped
		}
		if allSkipped {
			return fmt.Errorf("полностью пропущенный after не расходует maxVisits")
		}
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
			if firstLimited == "" && targetStep.MaxVisits != nil && runnableNumbers[target] >= *targetStep.MaxVisits {
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

// skipReach адресует decision-target внутри одной причинной волны пропуска.
// After-квитанции дедуплицируются своими FIFO-sources и могут повторить тот же
// step; waveKey строится только из проверенных visitId и не сохраняется в JSON.
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
// корни causal skip-wave для текущего Skipped visit. Непустой результат получает
// только synthetic-пропуск от решения или полностью пропущенный after, уже
// связанный с такой волной. Terminal cleanup остаётся rootless: он завершает
// существующую работу, но не изображает несуществовавший бизнес-выбор.
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
			// FIFO учитывает и ещё не завершённые instances. Terminal closure
			// делает единственное устойчивое исключение: Skipped может обойти
			// реальные sources, которые глобальный итог уже не продвинет. Среди
			// самих Skipped порядок строгий — каждый обязан оставить отдельную
			// FIFO-квитанцию, даже если это лишь rootless terminal cleanup.
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
			sourceRoots, causal := skipWaves[sourceID]
			if causal {
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
		var roots []string
		key := ""
		switch {
		case source.State == scheduler.Succeeded && source.Decision != nil && source.Decision.Error == "" &&
			(source.Decision.Applied || source.VisitID == limitDecisionSourceID):
			roots = []string{source.VisitID}
			key, exists = canonicalSkippedTargetKey(source, visit.StepID, steps)
		case source.State == scheduler.Skipped && len(steps[source.StepID].Decisions) != 0 && len(skipWaves[source.VisitID]) != 0:
			roots = slices.Clone(skipWaves[source.VisitID])
			key, exists = canonicalNestedSkippedTargetKey(steps[source.StepID], visit.StepID)
		default:
			return nil, fmt.Errorf("decision_skipped ссылается не на causal decision-источник")
		}
		if !exists || visit.Trigger.DecisionKey != key {
			return nil, fmt.Errorf("decision_skipped не совпадает с пропущенным target решения")
		}
		cause := decisionCause{visit.StepID, source.VisitID}
		if decisionUses[cause] {
			return nil, fmt.Errorf("decision-источник уже материализовал этот target")
		}
		waveReach := skipReach{waveKey: skipWaveKey(roots), targetStepID: visit.StepID}
		if skipReached != nil && skipReached[waveReach] {
			return nil, fmt.Errorf("волна пропуска уже достигла target %q", visit.StepID)
		}
		decisionUses[cause] = true
		if visit.Iteration != source.Iteration+1 {
			return nil, fmt.Errorf("decision_skipped-target должен увеличить iteration источника")
		}
		return roots, nil
	default:
		return nil, fmt.Errorf("неизвестный trigger %q", visit.Trigger.Kind)
	}
	return nil, nil
}

// canonicalSkippedTargetKey возвращает устойчивую причину synthetic Skipped.
// Один target может присутствовать сразу в нескольких невыбранных routes, но
// branch instance нужен ровно один. Лексикографически первый ключ делает trigger
// воспроизводимым и одновременно позволяет валидатору обнаружить подмену. Если
// выбранный route тоже содержит target, он остаётся достижимым и не пропускается.
func canonicalSkippedTargetKey(source Visit, target string, steps map[string]workflow.Step) (string, bool) {
	if source.Decision == nil || slices.Contains(source.Decision.To, target) {
		return "", false
	}
	step, exists := steps[source.StepID]
	if !exists {
		return "", false
	}
	for _, key := range source.Decision.Skipped {
		if slices.Contains(step.Decisions[key].To, target) {
			return key, true
		}
	}
	return "", false
}

// canonicalNestedSkippedTargetKey выбирает причину target у decision-шага,
// который сам не запускался. Бизнес-решения здесь не существует, поэтому
// недостижимы routes всех ключей; сортировка map-ключей и порядок route.to дают
// тот же стабильный результат после restart.
func canonicalNestedSkippedTargetKey(step workflow.Step, target string) (string, bool) {
	keys := make([]string, 0, len(step.Decisions))
	for key := range step.Decisions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if slices.Contains(step.Decisions[key].To, target) {
			return key, true
		}
	}
	return "", false
}

// canonicalSkipRoots объединяет корни нескольких причинных веток. Сортировка и
// удаление повторов нужны для after: одинаковое множество источников должно дать
// одну wave независимо от порядка перечисления зависимостей.
func canonicalSkipRoots(values []string) []string {
	if len(values) == 0 {
		return nil
	}
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

// skipWaveKey кодирует уже канонические visitId только для внутреннего map-key.
// NUL не допускается валидатором текстовых ID, поэтому разные наборы корней не
// могут склеиться. Поле не попадает ни в metadata, ни в пользовательский вывод.
func skipWaveKey(roots []string) string {
	return strings.Join(roots, "\x00")
}

// validateSkippedVisit отличает synthetic skip работающего графа от pending,
// который уже terminal-переход всего run перевёл в Skipped. Пока run работает,
// обычный decision/start token нельзя потерять, а after обязан быть Skipped
// тогда и только тогда, когда пропущены все его причинные ветки.
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
