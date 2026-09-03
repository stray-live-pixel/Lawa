package scheduler

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// PlanAgentSkippedClosure строит один слой только terminal Skipped-посещений.
// Вызывающая сторона назначает им VisitID, добавляет к переданному append-only
// журналу и повторяет вызов до пустого плана. Благодаря этому вложенные решения
// и after-барьеры замыкаются без временных ссылок между ещё не созданными visits.
//
// terminal разрешает промежуточный снимок атомарной остановки: применённый
// finish, rootless cleanup Skipped и точный не применённый источник maxVisits.
// limitStepID с limitTrigger описывают активацию N+1, которую terminal outcome
// заменил целиком. Функция никогда не планирует исполняемую работу и не меняет
// обычный PlanAgentGraph: все возвращённые активации имеют InitialState=Skipped,
// а ApplyDecisionVisitIDs и Terminal остаются пустыми.
func PlanAgentSkippedClosure(
	w workflow.Workflow,
	visits []AgentVisitView,
	terminal bool,
	limitStepID string,
	limitTrigger AgentTriggerView,
) (AgentPlan, error) {
	if err := w.Validate(); err != nil {
		return AgentPlan{}, fmt.Errorf("замыкание skipped: %w", err)
	}
	if w.EffectiveVersion() != workflow.VersionAgentGraph {
		return AgentPlan{}, fmt.Errorf("замыкание skipped требует workflow version=2")
	}

	limitDecisionSourceID := ""
	if limitStepID != "" && limitTrigger.Kind == AgentTriggerDecision && len(limitTrigger.SourceVisitIDs) == 1 {
		limitDecisionSourceID = limitTrigger.SourceVisitIDs[0]
	}
	context, err := validateAgentSkippedViews(w, visits, terminal, limitDecisionSourceID)
	if err != nil {
		return AgentPlan{}, fmt.Errorf("замыкание skipped: %w", err)
	}
	if err := reserveAgentSkippedLimit(context, visits, terminal, limitStepID, limitTrigger); err != nil {
		return AgentPlan{}, fmt.Errorf("замыкание skipped: %w", err)
	}

	plan := AgentPlan{}
	projectedReached := cloneAgentSkipReached(context.skipReached)
	for _, visit := range visits {
		step := context.steps[visit.StepID]
		if len(step.Decisions) == 0 {
			continue
		}
		switch visit.State {
		case Succeeded:
			if visit.Decision == nil || visit.Decision.Error != "" ||
				!visit.Decision.Applied && visit.VisitID != limitDecisionSourceID {
				continue
			}
			plan.DecisionActivations = append(plan.DecisionActivations,
				missingDecisionSkippedActivations(
					step, visit, visit.Decision.Key, []string{visit.VisitID}, projectedReached,
				)...)
		case Skipped:
			if roots, causal := context.skipRoots[visit.VisitID]; causal {
				plan.DecisionActivations = append(plan.DecisionActivations,
					missingDecisionSkippedActivations(step, visit, "", roots, projectedReached)...)
			}
		}
	}

	// After не дедуплицируется по wave/target: каждый visit является durable-
	// квитанцией конкретного FIFO-набора. Статический after-граф ацикличен, поэтому
	// такие квитанции конечны, а decision-циклы отдельно ограничивает skipReached.
	for _, step := range w.Steps {
		if len(step.After) == 0 {
			continue
		}
		sources, iteration, allSkipped, ready := nextSkippedAfterSources(step, context)
		if terminal {
			sources, iteration, allSkipped, ready = nextTerminalSkippedAfterSources(step, context)
		}
		if !ready || !allSkipped {
			continue
		}
		plan.AfterActivations = append(plan.AfterActivations, AgentActivation{
			StepID: step.ID, Iteration: iteration, InitialState: Skipped,
			Trigger: AgentTriggerView{Kind: AgentTriggerAfter, SourceVisitIDs: sources},
		})
	}
	return plan, nil
}

// agentSkippedContext содержит только индексы, необходимые skip-only API.
// visitsByStep включает активные и terminal instances в durable-порядке: в
// работающем run поздний Skipped не вправе обойти более ранний активный source.
type agentSkippedContext struct {
	steps        map[string]workflow.Step
	active       map[string]bool
	visitCount   map[string]int
	lastVisit    map[string]AgentVisitView
	visitsByStep map[string][]AgentVisitView
	usedAfter    map[agentAfterCause]bool
	decisionUses map[agentDecisionCause]bool
	skipRoots    map[string][]string
	skipReached  map[agentSkipCause]bool
	advanced     map[string]bool
}

// agentSkipCause ограничивает decision-fanout одной причинной волны. Одинаковый
// набор корневых реальных решений может достичь target лишь однажды, что
// схлопывает shared targets и гарантированно останавливает self-loop и SCC.
type agentSkipCause struct {
	waveKey      string
	targetStepID string
}

func validateAgentSkippedViews(
	w workflow.Workflow,
	visits []AgentVisitView,
	terminal bool,
	limitDecisionSourceID string,
) (agentSkippedContext, error) {
	context := agentSkippedContext{
		steps: make(map[string]workflow.Step, len(w.Steps)), active: make(map[string]bool),
		visitCount: make(map[string]int), lastVisit: make(map[string]AgentVisitView),
		visitsByStep: make(map[string][]AgentVisitView),
		usedAfter:    make(map[agentAfterCause]bool), decisionUses: make(map[agentDecisionCause]bool),
		skipRoots: make(map[string][]string), skipReached: make(map[agentSkipCause]bool),
		advanced: make(map[string]bool),
	}
	for _, step := range w.Steps {
		context.steps[step.ID] = step
	}
	if len(visits) < len(w.Start) {
		return agentSkippedContext{}, fmt.Errorf("история не содержит весь начальный префикс workflow.start")
	}

	seen := make(map[string]AgentVisitView, len(visits))
	for index, visit := range visits {
		step, exists := context.steps[visit.StepID]
		if !exists || visit.VisitID == "" || visit.Iteration < 1 {
			return agentSkippedContext{}, fmt.Errorf("visits[%d]: неверный visitId, stepId или iteration", index)
		}
		if _, duplicate := seen[visit.VisitID]; duplicate {
			return agentSkippedContext{}, fmt.Errorf("visits[%d]: повторный visitId %q", index, visit.VisitID)
		}
		if !knownAgentSkippedState(visit.State) {
			return agentSkippedContext{}, fmt.Errorf("посещение %q: неизвестное состояние %q", visit.VisitID, visit.State)
		}
		// Skipped подтверждает отсутствие запуска, поэтому не расходует maxVisits и
		// может сосуществовать с выполняющимся visit того же шага.
		if visit.State != Skipped {
			context.visitCount[visit.StepID]++
			context.lastVisit[visit.StepID] = visit
			if step.MaxVisits != nil && context.visitCount[visit.StepID] > *step.MaxVisits {
				return agentSkippedContext{}, fmt.Errorf("посещение %q превышает maxVisits=%d шага %q", visit.VisitID, *step.MaxVisits, visit.StepID)
			}
		}
		if context.active[visit.StepID] && visit.State != Skipped {
			return agentSkippedContext{}, fmt.Errorf("посещение %q появилось до завершения предыдущего visit шага %q", visit.VisitID, visit.StepID)
		}
		if !agentSkippedVisitTerminal(visit.State) {
			context.active[visit.StepID] = true
		}
		if visit.Decision != nil && (len(step.Decisions) == 0 ||
			visit.Decision.Applied && (visit.State != Succeeded || visit.Decision.Error != "")) {
			return agentSkippedContext{}, fmt.Errorf("посещение %q содержит решение в несовместимом состоянии", visit.VisitID)
		}

		if index < len(w.Start) &&
			(visit.StepID != w.Start[index] || visit.Trigger.Kind != AgentTriggerStart || visit.Iteration != 1) {
			return agentSkippedContext{}, fmt.Errorf("visits[%d] не совпадает с начальным префиксом workflow.start", index)
		}
		switch visit.Trigger.Kind {
		case AgentTriggerStart:
			if index >= len(w.Start) || len(visit.Trigger.SourceVisitIDs) != 0 || visit.Trigger.DecisionKey != "" {
				return agentSkippedContext{}, fmt.Errorf("посещение %q: start допустим только в начальном префиксе", visit.VisitID)
			}
			if visit.State == Skipped && !terminal {
				return agentSkippedContext{}, fmt.Errorf("посещение %q: running run не может пропустить start без terminal-причины", visit.VisitID)
			}
		case AgentTriggerAfter:
			allSkipped, err := validateAgentSkippedAfter(
				visit, step, seen, context.visitsByStep, context.usedAfter, terminal,
			)
			if err != nil {
				return agentSkippedContext{}, err
			}
			if visit.State == Skipped && allSkipped {
				roots, causal := agentAfterSkipRoots(visit.Trigger.SourceVisitIDs, context.skipRoots)
				if !causal && !terminal {
					return agentSkippedContext{}, fmt.Errorf("посещение %q: skipped after потерял причинную волну", visit.VisitID)
				}
				if causal {
					context.skipRoots[visit.VisitID] = roots
					context.skipReached[agentSkipCause{waveKey: agentSkipWaveKey(roots), targetStepID: visit.StepID}] = true
				}
			}
		case AgentTriggerDecision:
			if visit.State == Skipped && !terminal {
				return agentSkippedContext{}, fmt.Errorf("посещение %q: выбранный decision-target может стать skipped только при terminal run", visit.VisitID)
			}
			if err := validateAgentDecisionTrigger(visit, seen, context.decisionUses, context.steps); err != nil {
				return agentSkippedContext{}, err
			}
		case AgentTriggerDecisionSkipped:
			roots, err := validateAgentDecisionSkippedTrigger(
				visit, seen, context.decisionUses, context.steps, context.skipRoots,
				context.skipReached, terminal, limitDecisionSourceID,
			)
			if err != nil {
				return agentSkippedContext{}, err
			}
			context.skipRoots[visit.VisitID] = roots
		default:
			return agentSkippedContext{}, fmt.Errorf("посещение %q: неизвестный trigger %q", visit.VisitID, visit.Trigger.Kind)
		}
		seen[visit.VisitID] = visit
		context.visitsByStep[visit.StepID] = append(context.visitsByStep[visit.StepID], visit)
		for _, sourceID := range visit.Trigger.SourceVisitIDs {
			context.advanced[sourceID] = true
		}
	}

	// Applied — durable-обещание полного перехода. Выбранные runnable targets
	// проверяются после всего прохода, потому что законно расположены позже source.
	for _, visit := range visits {
		if visit.Decision == nil || !visit.Decision.Applied {
			continue
		}
		route, exists := context.steps[visit.StepID].Decisions[visit.Decision.Key]
		if !exists {
			return agentSkippedContext{}, fmt.Errorf("посещение %q: применено неизвестное решение %q", visit.VisitID, visit.Decision.Key)
		}
		if route.Finish != nil && !terminal {
			return agentSkippedContext{}, fmt.Errorf("посещение %q: применённый finish требует уже terminal run", visit.VisitID)
		}
		if route.Finish != nil {
			continue
		}
		for _, target := range route.To {
			if !context.decisionUses[agentDecisionCause{targetStepID: target, sourceVisitID: visit.VisitID}] {
				return agentSkippedContext{}, fmt.Errorf("посещение %q: применённое решение не материализовало target %q", visit.VisitID, target)
			}
		}
	}
	return context, nil
}

func validateAgentSkippedAfter(
	visit AgentVisitView,
	step workflow.Step,
	seen map[string]AgentVisitView,
	visitsByStep map[string][]AgentVisitView,
	used map[agentAfterCause]bool,
	terminal bool,
) (bool, error) {
	if len(step.After) == 0 || len(visit.Trigger.SourceVisitIDs) != len(step.After) || visit.Trigger.DecisionKey != "" {
		return false, fmt.Errorf("посещение %q: неверная форма after-trigger", visit.VisitID)
	}
	maxIteration, allSkipped := 0, true
	for index, dependency := range step.After {
		sourceID := visit.Trigger.SourceVisitIDs[index]
		source, exists := seen[sourceID]
		if !exists || source.StepID != dependency || !agentSkippedVisitTerminal(source.State) {
			return false, fmt.Errorf("посещение %q: неверный after-источник %q", visit.VisitID, sourceID)
		}
		expected, foundSource := "", false
		bypassReal := terminal && visit.State == Skipped && source.State == Skipped
		for _, candidate := range visitsByStep[dependency] {
			cause := agentAfterCause{targetStepID: step.ID, dependencyStepID: dependency, sourceVisitID: candidate.VisitID}
			if used[cause] {
				continue
			}
			if expected == "" {
				expected = candidate.VisitID
			}
			if candidate.VisitID == sourceID {
				foundSource = true
				break
			}
			// Terminal closure может обойти только реальные sources. Более ранний
			// Skipped всё равно обязан сначала оставить свою after-квитанцию.
			if candidate.State == Skipped {
				bypassReal = false
			}
		}
		cause := agentAfterCause{targetStepID: step.ID, dependencyStepID: dependency, sourceVisitID: sourceID}
		if !foundSource || sourceID != expected && !bypassReal || used[cause] {
			return false, fmt.Errorf("посещение %q: after нарушает FIFO зависимости %q", visit.VisitID, dependency)
		}
		used[cause] = true
		maxIteration = max(maxIteration, source.Iteration)
		allSkipped = allSkipped && source.State == Skipped
	}
	if visit.Iteration != maxIteration {
		return false, fmt.Errorf("посещение %q: after должен наследовать максимальную iteration источников", visit.VisitID)
	}
	if visit.State != Skipped && allSkipped || visit.State == Skipped && !allSkipped && !terminal {
		return false, fmt.Errorf("посещение %q: skipped after должен иметь только skipped-источники", visit.VisitID)
	}
	return allSkipped, nil
}

func validateAgentDecisionSkippedTrigger(
	visit AgentVisitView,
	seen map[string]AgentVisitView,
	uses map[agentDecisionCause]bool,
	steps map[string]workflow.Step,
	skipRoots map[string][]string,
	skipReached map[agentSkipCause]bool,
	terminal bool,
	limitDecisionSourceID string,
) ([]string, error) {
	if visit.State != Skipped || len(visit.Trigger.SourceVisitIDs) != 1 || visit.Trigger.DecisionKey == "" {
		return nil, fmt.Errorf("посещение %q: неверная форма skipped decision-trigger", visit.VisitID)
	}
	source, exists := seen[visit.Trigger.SourceVisitIDs[0]]
	if !exists {
		return nil, fmt.Errorf("посещение %q: неверный источник skipped decision", visit.VisitID)
	}
	selectedKey := ""
	var roots []string
	switch source.State {
	case Succeeded:
		if source.Decision == nil || source.Decision.Error != "" ||
			!source.Decision.Applied && (!terminal || source.VisitID != limitDecisionSourceID) {
			return nil, fmt.Errorf("посещение %q: неверный источник skipped decision", visit.VisitID)
		}
		if _, exists := steps[source.StepID].Decisions[source.Decision.Key]; !exists {
			return nil, fmt.Errorf("посещение %q: источник выбрал неизвестное решение", visit.VisitID)
		}
		selectedKey, roots = source.Decision.Key, []string{source.VisitID}
	case Skipped:
		if len(steps[source.StepID].Decisions) == 0 {
			return nil, fmt.Errorf("посещение %q: skipped-источник не является шагом решения", visit.VisitID)
		}
		var causal bool
		roots, causal = skipRoots[source.VisitID]
		if !causal {
			return nil, fmt.Errorf("посещение %q: skipped-источник не принадлежит причинной волне", visit.VisitID)
		}
	default:
		return nil, fmt.Errorf("посещение %q: неверный источник skipped decision", visit.VisitID)
	}

	wantKey := ""
	for _, candidate := range decisionSkippedTargets(steps[source.StepID], selectedKey) {
		if candidate.StepID == visit.StepID {
			wantKey = candidate.DecisionKey
			break
		}
	}
	if wantKey == "" || visit.Trigger.DecisionKey != wantKey {
		return nil, fmt.Errorf("посещение %q: skipped decision не соответствует невыбранному target", visit.VisitID)
	}
	cause := agentDecisionCause{targetStepID: visit.StepID, sourceVisitID: source.VisitID}
	if uses[cause] {
		return nil, fmt.Errorf("посещение %q: decision-target уже материализован", visit.VisitID)
	}
	if visit.Iteration != source.Iteration+1 {
		return nil, fmt.Errorf("посещение %q: skipped decision-target должен увеличить iteration", visit.VisitID)
	}
	waveCause := agentSkipCause{waveKey: agentSkipWaveKey(roots), targetStepID: visit.StepID}
	if skipReached[waveCause] {
		return nil, fmt.Errorf("посещение %q: target уже достигнут в той же skipped-волне", visit.VisitID)
	}
	uses[cause], skipReached[waveCause] = true, true
	return slices.Clone(roots), nil
}

func reserveAgentSkippedLimit(
	context agentSkippedContext,
	visits []AgentVisitView,
	terminal bool,
	limitStepID string,
	trigger AgentTriggerView,
) error {
	if limitStepID == "" {
		if trigger.Kind != "" || len(trigger.SourceVisitIDs) != 0 || trigger.DecisionKey != "" {
			return fmt.Errorf("limit-trigger задан без limitStepId")
		}
		return nil
	}
	if !terminal {
		return fmt.Errorf("maxVisits-trigger допустим только после terminal outcome")
	}
	limited, exists := context.steps[limitStepID]
	if !exists || limited.MaxVisits == nil {
		return fmt.Errorf("неизвестный шаг maxVisits %q", limitStepID)
	}
	switch trigger.Kind {
	case AgentTriggerDecision:
		if len(trigger.SourceVisitIDs) != 1 || trigger.DecisionKey == "" {
			return fmt.Errorf("неверная форма decision-limit trigger")
		}
		var source AgentVisitView
		for _, visit := range visits {
			if visit.VisitID == trigger.SourceVisitIDs[0] {
				source = visit
				break
			}
		}
		route, routeExists := context.steps[source.StepID].Decisions[trigger.DecisionKey]
		if source.VisitID == "" || source.State != Succeeded || source.Decision == nil || source.Decision.Applied ||
			source.Decision.Error != "" || source.Decision.Key != trigger.DecisionKey || !routeExists ||
			route.Finish != nil || !slices.Contains(route.To, limitStepID) ||
			context.active[limitStepID] || context.visitCount[limitStepID] < *limited.MaxVisits {
			return fmt.Errorf("decision-limit trigger не подтверждён историей")
		}
	case AgentTriggerAfter:
		if len(limited.After) == 0 || len(trigger.SourceVisitIDs) != len(limited.After) || trigger.DecisionKey != "" {
			return fmt.Errorf("неверная форма after-limit trigger")
		}
		sources, _, allSkipped, ready := nextSkippedAfterSources(limited, context)
		if !ready || allSkipped || !slices.Equal(sources, trigger.SourceVisitIDs) ||
			context.active[limitStepID] || context.visitCount[limitStepID] < *limited.MaxVisits {
			return fmt.Errorf("after-limit trigger не подтверждён историей")
		}
		// Точный FIFO-набор уже был потреблён попыткой создать N+1. Резервируем
		// его только для limited target, чтобы следующие causal Skipped не застряли.
		for index, dependency := range limited.After {
			cause := agentAfterCause{targetStepID: limitStepID, dependencyStepID: dependency, sourceVisitID: trigger.SourceVisitIDs[index]}
			if context.usedAfter[cause] {
				return fmt.Errorf("after-limit source уже использован")
			}
			context.usedAfter[cause] = true
		}
	default:
		return fmt.Errorf("maxVisits не поддерживает trigger %q", trigger.Kind)
	}
	return nil
}

// decisionSkippedTargets возвращает по одному target всех невыбранных routes.
// Ключи сортируются, порядок route.to сохраняется, target выбранного route всегда
// побеждает совпадающую альтернативу, а общий target альтернатив создаётся раз.
func decisionSkippedTargets(step workflow.Step, selectedKey string) []agentSkippedTarget {
	selected := make(map[string]bool)
	if route, exists := step.Decisions[selectedKey]; exists {
		for _, target := range route.To {
			selected[target] = true
		}
	}
	keys := make([]string, 0, len(step.Decisions))
	for key := range step.Decisions {
		if key != selectedKey {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	seen := make(map[string]bool)
	result := make([]agentSkippedTarget, 0)
	for _, key := range keys {
		for _, target := range step.Decisions[key].To {
			if selected[target] || seen[target] {
				continue
			}
			seen[target] = true
			result = append(result, agentSkippedTarget{StepID: target, DecisionKey: key})
		}
	}
	return result
}

type agentSkippedTarget struct {
	StepID      string
	DecisionKey string
}

func missingDecisionSkippedActivations(
	step workflow.Step,
	source AgentVisitView,
	selectedKey string,
	roots []string,
	reached map[agentSkipCause]bool,
) []AgentActivation {
	result := make([]AgentActivation, 0)
	waveKey := agentSkipWaveKey(roots)
	for _, target := range decisionSkippedTargets(step, selectedKey) {
		cause := agentSkipCause{waveKey: waveKey, targetStepID: target.StepID}
		if reached[cause] {
			continue
		}
		reached[cause] = true
		result = append(result, AgentActivation{
			StepID: target.StepID, Iteration: source.Iteration + 1, InitialState: Skipped,
			Trigger: AgentTriggerView{
				Kind: AgentTriggerDecisionSkipped, SourceVisitIDs: []string{source.VisitID}, DecisionKey: target.DecisionKey,
			},
		})
	}
	return result
}

func nextSkippedAfterSources(step workflow.Step, context agentSkippedContext) ([]string, int, bool, bool) {
	sources := make([]string, 0, len(step.After))
	maxIteration, allSkipped := 0, true
	for _, dependency := range step.After {
		found := false
		for _, candidate := range context.visitsByStep[dependency] {
			cause := agentAfterCause{targetStepID: step.ID, dependencyStepID: dependency, sourceVisitID: candidate.VisitID}
			if context.usedAfter[cause] {
				continue
			}
			if !agentSkippedVisitTerminal(candidate.State) {
				return nil, 0, false, false
			}
			sources = append(sources, candidate.VisitID)
			maxIteration = max(maxIteration, candidate.Iteration)
			allSkipped = allSkipped && candidate.State == Skipped
			found = true
			break
		}
		if !found {
			return nil, 0, false, false
		}
	}
	return sources, maxIteration, allSkipped, true
}

func nextTerminalSkippedAfterSources(step workflow.Step, context agentSkippedContext) ([]string, int, bool, bool) {
	sources := make([]string, 0, len(step.After))
	maxIteration := 0
	for _, dependency := range step.After {
		found := false
		for _, candidate := range context.visitsByStep[dependency] {
			cause := agentAfterCause{targetStepID: step.ID, dependencyStepID: dependency, sourceVisitID: candidate.VisitID}
			if context.usedAfter[cause] || candidate.State != Skipped {
				continue
			}
			sources = append(sources, candidate.VisitID)
			maxIteration = max(maxIteration, candidate.Iteration)
			found = true
			break
		}
		if !found {
			return nil, 0, false, false
		}
	}
	return sources, maxIteration, true, true
}

func agentAfterSkipRoots(sourceIDs []string, rootsByVisit map[string][]string) ([]string, bool) {
	groups := make([][]string, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		if roots, exists := rootsByVisit[sourceID]; exists {
			groups = append(groups, roots)
		}
	}
	if len(groups) == 0 {
		return nil, false
	}
	return mergeAgentSkipRoots(groups...), true
}

func mergeAgentSkipRoots(groups ...[]string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, group := range groups {
		for _, root := range group {
			if !seen[root] {
				seen[root] = true
				result = append(result, root)
			}
		}
	}
	slices.Sort(result)
	return result
}

// agentSkipWaveKey кодирует набор roots однозначно даже для VisitID с любыми
// разделителями: длина каждого непрозрачного значения входит в ключ.
func agentSkipWaveKey(roots []string) string {
	var key strings.Builder
	for _, root := range roots {
		key.WriteString(strconv.Itoa(len(root)))
		key.WriteByte(':')
		key.WriteString(root)
	}
	return key.String()
}

func cloneAgentSkipReached(source map[agentSkipCause]bool) map[agentSkipCause]bool {
	result := make(map[agentSkipCause]bool, len(source))
	for cause := range source {
		result[cause] = true
	}
	return result
}

func knownAgentSkippedState(state State) bool {
	return state == Skipped || knownAgentState(state)
}

func agentSkippedVisitTerminal(state State) bool {
	return state == Failed || state == Skipped || state == Succeeded
}
