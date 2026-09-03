//go:build darwin || linux

package runstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// AgentAdvance — результат одной атомарной материализации pure-плана. Snapshot
// уже содержит новые visits и Applied-флаги; CreatedVisits связывает ordered
// activations планировщика с созданными visitId для следующего шага coordinator.
// Changed=false означает, что durable snapshot уже полностью применён.
type AgentAdvance struct {
	Snapshot      Snapshot
	Plan          scheduler.AgentPlan
	CreatedVisits []Visit
	Changed       bool
}

// AdvanceAgentGraph применяет только локальные причинные переходы v4. Под общей
// блокировкой run функция перечитывает durable snapshot, получает чистый план,
// проверяет будущую metadata, сохраняет пустую память каждого нового visit и
// лишь затем одним Rename публикует Applied+Visits либо terminal outcome.
//
// Порядок файлов намеренный: авария до meta оставляет недостижимую orphan memory,
// которую Load игнорирует; обратный порядок мог бы опубликовать visit без памяти.
// После неоднозначного результата записи meta владелец poisoned и обязан закрыться.
func (r *LockedRun) AdvanceAgentGraph() (AgentAdvance, error) {
	return r.advanceAgentGraph((*os.File).Sync)
}

// advanceAgentGraph принимает Sync-hook только для проверки crash boundaries.
func (r *LockedRun) advanceAgentGraph(syncFile func(*os.File) error) (AgentAdvance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return AgentAdvance{}, err
	}
	snapshot, err := load(r.dir, r.runID)
	if err != nil {
		return AgentAdvance{}, err
	}
	if snapshot.Meta.Version != 4 {
		return AgentAdvance{}, fmt.Errorf("AdvanceAgentGraph требует meta.json v4")
	}
	if snapshot.Meta.RunState != RunRunning {
		return AgentAdvance{}, fmt.Errorf("завершённый agent-graph run нельзя продвигать")
	}
	plan, err := scheduler.PlanAgentGraph(snapshot.Workflow, agentVisitViews(snapshot.Meta.Visits))
	if err != nil {
		return AgentAdvance{}, err
	}
	if plan.Terminal != nil && (len(plan.DecisionActivations) != 0 || len(plan.AfterActivations) != 0) {
		return AgentAdvance{}, fmt.Errorf("планировщик смешал terminal outcome и новые activations")
	}
	changed := len(plan.ApplyDecisionVisitIDs) != 0 || len(plan.DecisionActivations) != 0 || len(plan.AfterActivations) != 0 || plan.Terminal != nil
	if !changed {
		return AgentAdvance{Snapshot: snapshot, Plan: plan}, nil
	}

	if err = applyAgentDecisionFlags(&snapshot, plan.ApplyDecisionVisitIDs); err != nil {
		return AgentAdvance{}, err
	}
	created := materializeAgentVisits(snapshot.Meta.Visits, plan.DecisionActivations, plan.AfterActivations)
	snapshot.Meta.Visits = append(snapshot.Meta.Visits, created...)
	if plan.Terminal != nil {
		switch plan.Terminal.Outcome {
		case workflow.OutcomeSucceeded:
			snapshot.Meta.RunState = RunSucceeded
		case workflow.OutcomeFailed:
			snapshot.Meta.RunState = RunFailed
		default:
			return AgentAdvance{}, fmt.Errorf("планировщик вернул неизвестный terminal outcome %q", plan.Terminal.Outcome)
		}
		snapshot.Meta.StopReason = compactAgentStopReason(plan.Terminal.Reason)
		snapshot.Meta.StopVisitID = plan.Terminal.CauseVisitID
		snapshot.Meta.StopLimitStepID = plan.Terminal.LimitStepID
		if plan.Terminal.LimitStepID != "" {
			trigger := VisitTrigger{
				Kind:           TriggerKind(plan.Terminal.LimitTrigger.Kind),
				SourceVisitIDs: slices.Clone(plan.Terminal.LimitTrigger.SourceVisitIDs),
				DecisionKey:    plan.Terminal.LimitTrigger.DecisionKey,
			}
			snapshot.Meta.StopLimitTrigger = &trigger
			snapshot.Meta.StopLimitIteration = plan.Terminal.LimitIteration
		}
		// Защитный fallback сохраняет проверяемую причину даже при регрессии
		// planner: штатный explicit finish уже возвращает свой CauseVisitID.
		if snapshot.Meta.StopVisitID == "" && len(plan.ApplyDecisionVisitIDs) == 1 {
			snapshot.Meta.StopVisitID = plan.ApplyDecisionVisitIDs[0]
		}
	}
	// Валидация до создания файлов отлавливает ошибку planner/bridge без orphan.
	// Повторяем её и внутри будущих Load, поэтому сохранённый снимок не опирается
	// на доверие к одной версии чистого планировщика.
	if err = snapshot.validate(r.runID); err != nil {
		return AgentAdvance{}, fmt.Errorf("применить agent-graph plan: %w", err)
	}
	if err = createAgentVisitMemories(r.dir, created, syncFile); err != nil {
		return AgentAdvance{}, fmt.Errorf("создать память новых посещений: %w", err)
	}
	if err = saveMetadata(r.dir, snapshot.Meta, syncFile); err != nil {
		return AgentAdvance{}, r.recordVisitSaveFailure(err)
	}
	return AgentAdvance{Snapshot: snapshot, Plan: plan, CreatedVisits: slices.Clone(created), Changed: true}, nil
}

// compactAgentStopReason превращает диагностический текст pure planner в
// безопасное поле metadata. Пользовательские step ID и decision key могут быть
// длинными и содержать экранированные JSON control-символы; такой корректный
// workflow всё равно обязан завершиться, а не навсегда остаться Running из-за
// локального ограничения хранения. Управляющие руны записываются видимым кодом,
// затем строка обрезается по UTF-8-границе вместе с маркером сокращения.
func compactAgentStopReason(value string) string {
	var escaped strings.Builder
	escaped.Grow(min(len(value), maxStoredTextBytes))
	for _, character := range value {
		if unicode.IsControl(character) {
			fmt.Fprintf(&escaped, `\u%04X`, character)
			continue
		}
		escaped.WriteRune(character)
	}
	reason := strings.TrimSpace(escaped.String())
	if reason == "" {
		reason = "workflow завершён без указанной причины"
	}
	if len(reason) <= maxStoredTextBytes {
		return reason
	}
	const suffix = "…"
	limit := maxStoredTextBytes - len(suffix)
	for limit > 0 && !utf8.RuneStart(reason[limit]) {
		limit--
	}
	return reason[:limit] + suffix
}

// applyAgentDecisionFlags меняет только перечисленные planner решения. Повторный
// ID и уже Applied означают внутреннюю ошибку плана: обычный crash-retry сначала
// перечитывает metadata и pure planner возвращает zero plan.
func applyAgentDecisionFlags(snapshot *Snapshot, visitIDs []string) error {
	indices := visitIndices(snapshot.Meta.Visits)
	seen := make(map[string]bool, len(visitIDs))
	for _, visitID := range visitIDs {
		index, exists := indices[visitID]
		if !exists || seen[visitID] {
			return fmt.Errorf("планировщик указал неизвестное или повторное решение visit %q", visitID)
		}
		seen[visitID] = true
		visit := &snapshot.Meta.Visits[index]
		if visit.State != scheduler.Succeeded || visit.Decision == nil || visit.Decision.Applied || visit.Decision.Error != "" {
			return fmt.Errorf("решение посещения %q нельзя применить", visitID)
		}
		visit.Decision.Applied = true
	}
	return nil
}

// materializeAgentVisits сохраняет порядок planner: сначала выбранные decision
// targets в порядке Visits/route.to, затем after-barriers в порядке Steps. Номер
// Visit считается по уже durable истории и возрастает отдельно для StepID.
func materializeAgentVisits(history []Visit, decision, after []scheduler.AgentActivation) []Visit {
	numbers := make(map[string]int)
	for _, visit := range history {
		numbers[visit.StepID] = visit.Visit
	}
	activations := make([]scheduler.AgentActivation, 0, len(decision)+len(after))
	activations = append(activations, decision...)
	activations = append(activations, after...)
	created := make([]Visit, 0, len(activations))
	for _, activation := range activations {
		numbers[activation.StepID]++
		created = append(created, Visit{
			VisitID: newID(), StepID: activation.StepID, Visit: numbers[activation.StepID],
			Iteration: activation.Iteration, State: scheduler.Pending,
			Trigger: VisitTrigger{
				Kind: TriggerKind(activation.Trigger.Kind), SourceVisitIDs: slices.Clone(activation.Trigger.SourceVisitIDs),
				DecisionKey: activation.Trigger.DecisionKey,
			},
		})
	}
	return created
}

// createAgentVisitMemories синхронизирует каждый inode и затем имя в memory/.
// При любой ошибке metadata не публикуется, а уже созданные файлы намеренно не
// удаляются: их имена случайны, старый snapshot на них не ссылается, а удаление
// после неясного Sync создало бы ещё одну недоказуемую границу durability.
func createAgentVisitMemories(dir *os.Root, visits []Visit, syncFile func(*os.File) error) error {
	if len(visits) == 0 {
		return nil
	}
	for _, visit := range visits {
		name := filepath.Join("memory", visit.VisitID+".md")
		file, err := dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if err = syncFile(file); err == nil {
			err = file.Close()
		} else {
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			return err
		}
	}
	memory, err := dir.Open("memory")
	if err != nil {
		return err
	}
	return errors.Join(syncFile(memory), memory.Close())
}
