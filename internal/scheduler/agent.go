package scheduler

import (
	"fmt"
	"slices"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// AgentTriggerKind описывает сохранённую причину появления посещения. Тип живёт в
// scheduler, а не в runstore, чтобы чистое ядро не зависело от файлового формата.
// Persistence-слой переводит свои строковые значения в этот тип на границе.
type AgentTriggerKind string

const (
	AgentTriggerStart           AgentTriggerKind = "start"
	AgentTriggerAfter           AgentTriggerKind = "after"
	AgentTriggerDecision        AgentTriggerKind = "decision"
	AgentTriggerDecisionSkipped AgentTriggerKind = "decision_skipped"
)

// AgentTriggerView — минимальная проекция durable trigger, необходимая чистому
// планировщику. SourceVisitIDs сохраняют исходный порядок: зависимости Step.After
// для after и единственный visit решения для decision.
type AgentTriggerView struct {
	Kind           AgentTriggerKind
	SourceVisitIDs []string
	DecisionKey    string
}

// AgentDecisionView содержит только изменяемую часть durable выбора. Разрешённые
// To/Finish планировщик всегда берёт из неизменяемого workflow: это не позволяет
// переданной проекции незаметно подменить пользовательский маршрут.
type AgentDecisionView struct {
	Key     string
	Applied bool
	Error   string
}

// AgentVisitView — упорядоченная проекция append-only истории run. Срез должен
// содержать все visits в durable-порядке: он одновременно задаёт FIFO для after,
// стабильный приоритет решений и причинную границу natural completion.
type AgentVisitView struct {
	VisitID   string
	StepID    string
	Iteration int
	State     State
	Trigger   AgentTriggerView
	Decision  *AgentDecisionView
}

// AgentActivation полностью описывает новое посещение, кроме его случайного
// VisitID. Нулевой InitialState означает прежний Pending; Skipped позволяет
// отдельному pure-плану сохранить недостижимую ветку без запуска агента.
// Persistence-слой генерирует ID и атомарно сохраняет активации с решениями.
type AgentActivation struct {
	StepID       string
	Iteration    int
	Trigger      AgentTriggerView
	InitialState State
}

// AgentTerminal — итог всего run. CauseVisitID связывает failed frontier, fatal
// decision, explicit finish либо исчерпанный лимит с конкретным durable visit.
// LimitStepID дополнительно отличает остановку maxVisits от естественного исхода:
// runstore сможет проверить шаг, квоту и onLimit, не разбирая диагностический
// Reason. Пустой CauseVisitID допустим только у естественного успешного исхода,
// для которого единственного причинного посещения может не существовать.
type AgentTerminal struct {
	Outcome      workflow.TerminalOutcome
	Reason       string
	CauseVisitID string
	LimitStepID  string
	// LimitTrigger и LimitIteration фиксируют не созданную N+1 активацию. Они
	// позволяют persistence проверять причину после drain соседних active visits,
	// не переигрывая изменившийся глобальный frontier.
	LimitTrigger   AgentTriggerView
	LimitIteration int
}

// AgentPlan — детерминированный следующий атомарный переход. Decision-активации
// идут в порядке Visits и route.to, after-активации — в порядке Workflow.Steps.
// Пустой план без Terminal означает ожидание уже активных/resumable visits.
type AgentPlan struct {
	ApplyDecisionVisitIDs []string
	DecisionActivations   []AgentActivation
	AfterActivations      []AgentActivation
	Terminal              *AgentTerminal
}

// PlanAgentGraph вычисляет следующий переход version=2 без I/O и не изменяет
// workflow либо visits. Циклы допустимы только через decision-route и ограничены
// MaxVisits на каждом входящем в цикл шаге решения — это заранее проверяет
// Workflow.Validate. Планировщик считает все созданные visits целевого шага и
// перед созданием N+1 атомарно завершает весь run с OnLimit (по умолчанию failed).
//
// Приоритеты намеренны. Durable Decision.Error, отсутствующий выбор и неизвестный
// ключ немедленно завершают run. Валидные решения ждут, пока все уже существующие
// non-applied decision-visits достигнут Failed/Succeeded: итог параллельной волны
// тогда зависит от порядка Visits, а не скорости ответов. После барьера terminal
// decision не смешивается с новыми visits; обычные route fanout применяются
// атомарно, затем строятся FIFO after. Без действий и active visits исход
// определяет фактический terminal frontier.
func PlanAgentGraph(w workflow.Workflow, visits []AgentVisitView) (AgentPlan, error) {
	if err := w.Validate(); err != nil {
		return AgentPlan{}, fmt.Errorf("visit-aware планировщик: %w", err)
	}
	if w.EffectiveVersion() != workflow.VersionAgentGraph {
		return AgentPlan{}, fmt.Errorf("visit-aware планировщик требует workflow version=2")
	}
	context, err := validateAgentVisitViews(w, visits)
	if err != nil {
		return AgentPlan{}, fmt.Errorf("visit-aware планировщик: %w", err)
	}
	if terminal, exists := firstDecisionFatal(context.steps, visits); exists {
		return terminal, nil
	}
	decisionWaveComplete := agentDecisionWaveComplete(context.steps, visits)
	if decisionWaveComplete {
		if terminal, exists := firstDecisionTerminal(context.steps, visits); exists {
			return terminal, nil
		}
	}

	plan := AgentPlan{}
	projectedActive := make(map[string]bool, len(context.active))
	for stepID := range context.active {
		projectedActive[stepID] = true
	}

	// Один decision fanout является неделимой операцией. Если хотя бы одна цель
	// занята, нельзя применить только свободную часть: повтор после crash иначе не
	// сможет отличить полный маршрут от частично материализованного.
	for _, visit := range visits {
		if !decisionWaveComplete {
			break
		}
		step := context.steps[visit.StepID]
		decision := visit.Decision
		if len(step.Decisions) == 0 || visit.State != Succeeded || decision == nil || decision.Applied || decision.Error != "" {
			continue
		}
		route, exists := step.Decisions[decision.Key]
		if !exists || route.Finish != nil {
			// Unknown и finish уже обработаны firstDecisionTerminal. Условие
			// оставлено защитным, чтобы дальнейшее расширение функции не могло
			// превратить terminal route в обычную активацию.
			continue
		}
		blocked := false
		for _, target := range route.To {
			if projectedActive[target] {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		// Проверяем квоты всего fanout до Applied и первой активации. Если хотя
		// бы одна цель исчерпана, маршрут целиком остаётся unapplied, а terminal
		// commit не может оставить частично созданные соседние targets.
		for _, target := range route.To {
			trigger := AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{visit.VisitID}, DecisionKey: decision.Key}
			if terminal, reached := agentVisitLimitTerminal(context.steps[target], visit.Iteration+1, trigger, context); reached {
				return AgentPlan{Terminal: terminal}, nil
			}
		}
		plan.ApplyDecisionVisitIDs = append(plan.ApplyDecisionVisitIDs, visit.VisitID)
		for _, target := range route.To {
			plan.DecisionActivations = append(plan.DecisionActivations, AgentActivation{
				StepID: target, Iteration: visit.Iteration + 1,
				Trigger: AgentTriggerView{Kind: AgentTriggerDecision, SourceVisitIDs: []string{visit.VisitID}, DecisionKey: decision.Key},
			})
			projectedActive[target] = true
		}
	}
	// After также ждёт decision-wave. Иначе параллельный Running decision мог бы
	// завершиться между сохранением limit-terminal и drain: повторная проверка
	// того же журнала выбрала бы его finish и сделала итог зависимым от timing.
	if !decisionWaveComplete {
		return AgentPlan{}, nil
	}

	// After рассматривается только после direct routes: если оба механизма хотят
	// один step в одном снимке, явный route получает его первым, а FIFO-источники
	// after остаются непотреблёнными до следующего вызова.
	for _, step := range w.Steps {
		if len(step.After) == 0 || projectedActive[step.ID] {
			continue
		}
		sources, iteration, ready := nextAfterSources(step, context)
		if !ready {
			continue
		}
		trigger := AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: sources}
		if terminal, reached := agentVisitLimitTerminal(step, iteration, trigger, context); reached {
			// Terminal нельзя смешивать с routes/after, уже накопленными выше:
			// runstore публикует либо весь обычный переход, либо один итог.
			return AgentPlan{Terminal: terminal}, nil
		}
		plan.AfterActivations = append(plan.AfterActivations, AgentActivation{
			StepID: step.ID, Iteration: iteration,
			Trigger: AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: sources},
		})
		projectedActive[step.ID] = true
	}

	if len(plan.ApplyDecisionVisitIDs) != 0 || len(plan.DecisionActivations) != 0 || len(plan.AfterActivations) != 0 {
		return plan, nil
	}
	if len(context.active) != 0 {
		return AgentPlan{}, nil
	}

	for _, visit := range visits {
		if visit.State == Failed && !context.advanced[visit.VisitID] {
			return AgentPlan{Terminal: &AgentTerminal{
				Outcome:      workflow.OutcomeFailed,
				Reason:       fmt.Sprintf("terminal-посещение %q шага %q завершилось с ошибкой", visit.VisitID, visit.StepID),
				CauseVisitID: visit.VisitID,
			}}, nil
		}
	}
	return AgentPlan{Terminal: &AgentTerminal{
		Outcome: workflow.OutcomeSucceeded,
		Reason:  "workflow достиг естественного завершения",
	}}, nil
}

// agentPlanningContext — проверенные индексы одного снимка. terminalByStep
// сохраняет порядок Visits, поэтому поиск следующего after-источника не зависит
// от map iteration. usedAfter разделён по target/dependency/source: одно событие
// может честно разбудить несколько разных downstream-кубиков.
type agentPlanningContext struct {
	steps          map[string]workflow.Step
	active         map[string]bool
	visitCount     map[string]int
	lastVisit      map[string]AgentVisitView
	terminalByStep map[string][]AgentVisitView
	usedAfter      map[agentAfterCause]bool
	advanced       map[string]bool
}

type agentAfterCause struct {
	targetStepID     string
	dependencyStepID string
	sourceVisitID    string
}

type agentDecisionCause struct {
	targetStepID  string
	sourceVisitID string
}

// validateAgentVisitViews защищает чистую публичную границу от неполной или
// переставленной проекции. Runstore выполняет более подробную проверку metadata,
// но planner не полагается на невыраженные свойства другого пакета.
func validateAgentVisitViews(w workflow.Workflow, visits []AgentVisitView) (agentPlanningContext, error) {
	context := agentPlanningContext{
		steps: make(map[string]workflow.Step, len(w.Steps)), active: make(map[string]bool),
		visitCount: make(map[string]int), lastVisit: make(map[string]AgentVisitView),
		terminalByStep: make(map[string][]AgentVisitView), usedAfter: make(map[agentAfterCause]bool),
		advanced: make(map[string]bool),
	}
	for _, step := range w.Steps {
		context.steps[step.ID] = step
	}
	if len(visits) < len(w.Start) {
		return agentPlanningContext{}, fmt.Errorf("история не содержит весь начальный префикс workflow.start")
	}

	seen := make(map[string]AgentVisitView, len(visits))
	decisionUses := make(map[agentDecisionCause]bool)
	for index, visit := range visits {
		step, exists := context.steps[visit.StepID]
		if !exists || visit.VisitID == "" || visit.Iteration < 1 {
			return agentPlanningContext{}, fmt.Errorf("visits[%d]: неверный visitId, stepId или iteration", index)
		}
		if _, duplicate := seen[visit.VisitID]; duplicate {
			return agentPlanningContext{}, fmt.Errorf("visits[%d]: повторный visitId %q", index, visit.VisitID)
		}
		if !knownAgentState(visit.State) {
			return agentPlanningContext{}, fmt.Errorf("посещение %q: неизвестное состояние %q", visit.VisitID, visit.State)
		}
		context.visitCount[visit.StepID]++
		if step.MaxVisits != nil && context.visitCount[visit.StepID] > *step.MaxVisits {
			return agentPlanningContext{}, fmt.Errorf("посещение %q превышает maxVisits=%d шага %q", visit.VisitID, *step.MaxVisits, visit.StepID)
		}
		context.lastVisit[visit.StepID] = visit
		if context.active[visit.StepID] {
			return agentPlanningContext{}, fmt.Errorf("посещение %q появилось до завершения предыдущего visit шага %q", visit.VisitID, visit.StepID)
		}
		if !agentVisitTerminal(visit.State) {
			context.active[visit.StepID] = true
		}
		if visit.Decision != nil {
			if len(step.Decisions) == 0 || visit.Decision.Applied && (visit.State != Succeeded || visit.Decision.Error != "") {
				return agentPlanningContext{}, fmt.Errorf("посещение %q содержит решение в несовместимом состоянии", visit.VisitID)
			}
		}

		if index < len(w.Start) {
			if visit.StepID != w.Start[index] || visit.Trigger.Kind != AgentTriggerStart || visit.Iteration != 1 {
				return agentPlanningContext{}, fmt.Errorf("visits[%d] не совпадает с начальным префиксом workflow.start", index)
			}
		}
		switch visit.Trigger.Kind {
		case AgentTriggerStart:
			if index >= len(w.Start) || len(visit.Trigger.SourceVisitIDs) != 0 || visit.Trigger.DecisionKey != "" {
				return agentPlanningContext{}, fmt.Errorf("посещение %q: start допустим только в начальном префиксе", visit.VisitID)
			}
		case AgentTriggerAfter:
			if err := validateAgentAfterTrigger(visit, step, seen, context.terminalByStep, context.usedAfter); err != nil {
				return agentPlanningContext{}, err
			}
		case AgentTriggerDecision:
			if err := validateAgentDecisionTrigger(visit, seen, decisionUses, context.steps); err != nil {
				return agentPlanningContext{}, err
			}
		default:
			return agentPlanningContext{}, fmt.Errorf("посещение %q: неизвестный trigger %q", visit.VisitID, visit.Trigger.Kind)
		}
		for _, sourceID := range visit.Trigger.SourceVisitIDs {
			context.advanced[sourceID] = true
		}
		seen[visit.VisitID] = visit
		if agentVisitTerminal(visit.State) {
			context.terminalByStep[visit.StepID] = append(context.terminalByStep[visit.StepID], visit)
		}
	}
	// Applied является durable-обещанием, что persistence опубликовал весь
	// переход одним commit. Проверяем обещание после полного прохода: targets
	// законно расположены позже source visit, поэтому раньше доказать полноту
	// fanout невозможно. Applied finish в эту функцию попадать не должен вовсе:
	// он атомарно переводит run в terminal, а planner вычисляет только следующий
	// переход ещё работающего снимка.
	for _, visit := range visits {
		if visit.Decision == nil || !visit.Decision.Applied {
			continue
		}
		route, exists := context.steps[visit.StepID].Decisions[visit.Decision.Key]
		if !exists {
			return agentPlanningContext{}, fmt.Errorf("посещение %q: применено неизвестное решение %q", visit.VisitID, visit.Decision.Key)
		}
		if route.Finish != nil {
			return agentPlanningContext{}, fmt.Errorf("посещение %q: применённый finish требует уже terminal run", visit.VisitID)
		}
		for _, target := range route.To {
			if !decisionUses[agentDecisionCause{target, visit.VisitID}] {
				return agentPlanningContext{}, fmt.Errorf("посещение %q: применённое решение не материализовало target %q", visit.VisitID, target)
			}
		}
	}
	return context, nil
}

func validateAgentAfterTrigger(visit AgentVisitView, step workflow.Step, seen map[string]AgentVisitView, terminalByStep map[string][]AgentVisitView, used map[agentAfterCause]bool) error {
	if len(step.After) == 0 || len(visit.Trigger.SourceVisitIDs) != len(step.After) || visit.Trigger.DecisionKey != "" {
		return fmt.Errorf("посещение %q: неверная форма after-trigger", visit.VisitID)
	}
	maxIteration := 0
	for index, dependency := range step.After {
		sourceID := visit.Trigger.SourceVisitIDs[index]
		source, exists := seen[sourceID]
		if !exists || source.StepID != dependency || !agentVisitTerminal(source.State) {
			return fmt.Errorf("посещение %q: неверный after-источник %q", visit.VisitID, sourceID)
		}
		expected := ""
		for _, candidate := range terminalByStep[dependency] {
			cause := agentAfterCause{step.ID, dependency, candidate.VisitID}
			if !used[cause] {
				expected = candidate.VisitID
				break
			}
		}
		cause := agentAfterCause{step.ID, dependency, sourceID}
		if sourceID != expected || used[cause] {
			return fmt.Errorf("посещение %q: after нарушает FIFO зависимости %q", visit.VisitID, dependency)
		}
		used[cause] = true
		maxIteration = max(maxIteration, source.Iteration)
	}
	if visit.Iteration != maxIteration {
		return fmt.Errorf("посещение %q: after должен наследовать максимальную iteration источников", visit.VisitID)
	}
	return nil
}

func validateAgentDecisionTrigger(visit AgentVisitView, seen map[string]AgentVisitView, uses map[agentDecisionCause]bool, steps map[string]workflow.Step) error {
	if len(visit.Trigger.SourceVisitIDs) != 1 || visit.Trigger.DecisionKey == "" {
		return fmt.Errorf("посещение %q: неверная форма decision-trigger", visit.VisitID)
	}
	source, exists := seen[visit.Trigger.SourceVisitIDs[0]]
	if !exists || source.State != Succeeded || source.Decision == nil || !source.Decision.Applied || source.Decision.Error != "" || source.Decision.Key != visit.Trigger.DecisionKey {
		return fmt.Errorf("посещение %q: неверный decision-источник", visit.VisitID)
	}
	route, exists := steps[source.StepID].Decisions[source.Decision.Key]
	if !exists || route.Finish != nil || !slices.Contains(route.To, visit.StepID) {
		return fmt.Errorf("посещение %q: источник не выбирал этот target", visit.VisitID)
	}
	cause := agentDecisionCause{visit.StepID, source.VisitID}
	if uses[cause] {
		return fmt.Errorf("посещение %q: decision-target уже материализован", visit.VisitID)
	}
	uses[cause] = true
	if visit.Iteration != source.Iteration+1 {
		return fmt.Errorf("посещение %q: decision-target должен увеличить iteration", visit.VisitID)
	}
	return nil
}

// firstDecisionFatal ищет доказанную ошибку до wave-barrier. Durable poison,
// отсутствующий choose_decision и неизвестный ключ не являются конкурирующими
// исходами волны, поэтому не должны ждать зависший параллельный агент.
func firstDecisionFatal(steps map[string]workflow.Step, visits []AgentVisitView) (AgentPlan, bool) {
	for _, visit := range visits {
		step := steps[visit.StepID]
		if len(step.Decisions) == 0 {
			continue
		}
		decision := visit.Decision
		if decision != nil && decision.Error != "" {
			return fatalDecisionPlan(visit, "durable решение содержит конфликт: "+visit.Decision.Error), true
		}
		if visit.State != Succeeded {
			continue
		}
		if decision == nil {
			return fatalDecisionPlan(visit, "успешный turn завершился без choose_decision"), true
		}
		if _, exists := step.Decisions[decision.Key]; !exists {
			return fatalDecisionPlan(visit, fmt.Sprintf("сохранён неизвестный ключ решения %q", decision.Key)), true
		}
	}
	return AgentPlan{}, false
}

// agentDecisionWaveComplete отделяет порядок Visits от скорости внешних Result.
// Applied visits принадлежат прошлым волнам; каждый текущий decision visit должен
// дойти именно до Failed/Succeeded. Cancelled и промежуточные состояния ещё можно
// продолжить новым turn, поэтому они сохраняют барьер закрытым.
func agentDecisionWaveComplete(steps map[string]workflow.Step, visits []AgentVisitView) bool {
	for _, visit := range visits {
		if len(steps[visit.StepID].Decisions) == 0 || visit.Decision != nil && visit.Decision.Applied {
			continue
		}
		if !agentVisitTerminal(visit.State) {
			return false
		}
	}
	return true
}

// firstDecisionTerminal вызывается только для завершённой decision-wave и
// возвращает её первый terminal-переход в порядке Visits. Обычные routes здесь
// не материализуются: если visit требует завершения, накопление активаций даже
// более раннего route безопасно не начинается.
func firstDecisionTerminal(steps map[string]workflow.Step, visits []AgentVisitView) (AgentPlan, bool) {
	for _, visit := range visits {
		step := steps[visit.StepID]
		if len(step.Decisions) == 0 {
			continue
		}
		decision := visit.Decision
		if visit.State != Succeeded || decision == nil || decision.Applied {
			continue
		}
		route := step.Decisions[decision.Key]
		if route.Finish != nil {
			outcome := *route.Finish
			return AgentPlan{
				ApplyDecisionVisitIDs: []string{visit.VisitID},
				Terminal: &AgentTerminal{
					Outcome:      outcome,
					Reason:       fmt.Sprintf("решение %q шага %q выбрало finish %q", decision.Key, visit.StepID, outcome),
					CauseVisitID: visit.VisitID,
				},
			}, true
		}
	}
	return AgentPlan{}, false
}

func fatalDecisionPlan(visit AgentVisitView, detail string) AgentPlan {
	return AgentPlan{Terminal: &AgentTerminal{
		Outcome:      workflow.OutcomeFailed,
		Reason:       fmt.Sprintf("посещение %q шага решения %q: %s", visit.VisitID, visit.StepID, detail),
		CauseVisitID: visit.VisitID,
	}}
}

// nextAfterSources выбирает по одному самому раннему ещё не потреблённому
// terminal visit каждой зависимости. Барьер готов только целиком; отсутствие
// одного источника не расходует уже найденные источники остальных зависимостей.
func nextAfterSources(step workflow.Step, context agentPlanningContext) ([]string, int, bool) {
	sources := make([]string, 0, len(step.After))
	maxIteration := 0
	for _, dependency := range step.After {
		found := false
		for _, candidate := range context.terminalByStep[dependency] {
			cause := agentAfterCause{step.ID, dependency, candidate.VisitID}
			if context.usedAfter[cause] {
				continue
			}
			sources = append(sources, candidate.VisitID)
			maxIteration = max(maxIteration, candidate.Iteration)
			found = true
			break
		}
		if !found {
			return nil, 0, false
		}
	}
	return sources, maxIteration, true
}

// agentVisitLimitTerminal вызывается только непосредственно перед созданием
// очередного посещения step и после проверки no-overlap. Поэтому count==limit
// означает отказ именно от N+1, а CauseVisitID всегда указывает на последний
// разрешённый visit N. Положительность лимита уже доказана Workflow.Validate.
func agentVisitLimitTerminal(step workflow.Step, nextIteration int, trigger AgentTriggerView, context agentPlanningContext) (*AgentTerminal, bool) {
	if step.MaxVisits == nil || context.visitCount[step.ID] < *step.MaxVisits {
		return nil, false
	}
	last := context.lastVisit[step.ID]
	outcome := workflow.OutcomeFailed
	reason := fmt.Sprintf("шаг %q достиг maxVisits=%d; посещение %d с iteration=%d не создано; onLimit не задан, workflow завершён как %q",
		step.ID, *step.MaxVisits, context.visitCount[step.ID]+1, nextIteration, outcome)
	if step.OnLimit != nil {
		outcome = *step.OnLimit
		reason = fmt.Sprintf("шаг %q достиг maxVisits=%d; посещение %d с iteration=%d не создано; применён onLimit %q",
			step.ID, *step.MaxVisits, context.visitCount[step.ID]+1, nextIteration, outcome)
	}
	trigger.SourceVisitIDs = slices.Clone(trigger.SourceVisitIDs)
	return &AgentTerminal{
		Outcome: outcome, Reason: reason, CauseVisitID: last.VisitID, LimitStepID: step.ID,
		LimitTrigger: trigger, LimitIteration: nextIteration,
	}, true
}

func knownAgentState(state State) bool {
	switch state {
	case Pending, Starting, Unknown, Running, WaitingForApproval, Failed, Cancelled, Succeeded:
		return true
	default:
		return false
	}
}

func agentVisitTerminal(state State) bool {
	return state == Failed || state == Succeeded
}
