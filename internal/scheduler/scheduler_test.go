package scheduler

import (
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/stray-live-pixel/flows-2/internal/workflow"
)

// reviewWorkflow берёт продуктовый пример: сборщик записан раньше родителей,
// а два ревью должны выполняться независимо после общих метрик.
func reviewWorkflow(t *testing.T) workflow.Workflow {
	t.Helper()
	f, err := os.Open("../../examples/review.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := workflow.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// pendingStates имитирует явную инициализацию нового запуска координатором.
// Восстановление сохранённого run не должно заполнять потерянные записи так же.
func pendingStates(w workflow.Workflow) map[string]State {
	states := make(map[string]State, len(w.Steps))
	for _, step := range w.Steps {
		states[step.ID] = Pending
	}
	return states
}

// TestWorkflowLifecycle воспроизводит ошибку, ручной повтор в том же чате и
// успешный сборщик. Starting имитирует уже сохранённое намерение координатора:
// повторные вычисления после него не должны снова предлагать этот шаг.
// История строится вне t.Run: выбор одного подтеста через -run не должен пропускать
// предшествующие события. Каждый подтест получает отдельный снимок состояния.
func TestWorkflowLifecycle(t *testing.T) {
	w := reviewWorkflow(t)
	states := pendingStates(w)
	for _, tc := range []struct {
		name     string
		changes  map[string]State
		ready    []string
		waiting  []string
		complete bool
	}{
		{"начало", nil, []string{"metrics"}, []string{"summary", "architecture", "security"}, false},
		{"намерение сохранено", map[string]State{"metrics": Starting}, nil, []string{"summary", "architecture", "security"}, false},
		{"метрики готовы", map[string]State{"metrics": Succeeded}, []string{"architecture", "security"}, []string{"summary"}, false},
		{"ревью начаты", map[string]State{"architecture": Starting, "security": Starting}, nil, []string{"summary"}, false},
		{"одна ветка упала", map[string]State{"architecture": Failed, "security": Running}, nil, []string{"summary"}, false},
		{"ручной повтор", map[string]State{"architecture": Running, "security": Succeeded}, nil, []string{"summary"}, false},
		{"оба ревью готовы", map[string]State{"architecture": Succeeded}, []string{"summary"}, nil, false},
		{"сборщик начат", map[string]State{"summary": Starting}, nil, nil, false},
		{"повтор события", nil, nil, nil, false},
		{"родитель продолжен после запуска сборщика", map[string]State{"architecture": Running}, nil, nil, false},
		{"повтор родителя завершён", map[string]State{"architecture": Succeeded, "summary": Running}, nil, nil, false},
		{"всё завершено", map[string]State{"summary": Succeeded}, nil, nil, true},
		{"успешный run снова активен", map[string]State{"architecture": Running}, nil, nil, false},
		{"повтор снова успешен", map[string]State{"architecture": Succeeded}, nil, nil, true},
	} {
		maps.Copy(states, tc.changes)
		snapshot := maps.Clone(states)
		t.Run(tc.name, func(t *testing.T) {
			want := Plan{Ready: tc.ready, Waiting: tc.waiting, Complete: tc.complete}
			got, err := Evaluate(w, snapshot)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("план: %+v, %v; ожидался: %+v", got, err, want)
			}
		})
	}
}

// TestStatesAndIndependentBranches проверяет все состояния родителя. Ошибка,
// отмена, неизвестность и ожидание подтверждения блокируют только его потомка,
// но не другую ветку. Ни одно уже начатое состояние не разрешает повторный старт.
func TestStatesAndIndependentBranches(t *testing.T) {
	w := workflow.Workflow{ID: "independent", Steps: []workflow.Step{
		{ID: "after-a", Type: "agent", Prompt: "задача", DependsOn: []string{"a"}},
		{ID: "after-b", Type: "agent", Prompt: "задача", DependsOn: []string{"b"}},
		{ID: "a", Type: "agent", Prompt: "задача", DependsOn: []string{}},
		{ID: "b", Type: "agent", Prompt: "задача", DependsOn: []string{}},
	}}
	for _, state := range []State{Pending, Starting, Unknown, Running, WaitingForApproval, Failed, Cancelled, Succeeded} {
		t.Run(string(state), func(t *testing.T) {
			states := pendingStates(w)
			states["a"], states["b"] = state, Succeeded
			want := Plan{Ready: []string{"after-b"}, Waiting: []string{"after-a"}}
			if state == Pending {
				want.Ready = append(want.Ready, "a")
			} else if state == Succeeded {
				want = Plan{Ready: []string{"after-a", "after-b"}}
			}
			got, err := Evaluate(w, states)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("план: %+v, %v; ожидался: %+v", got, err, want)
			}
		})
	}
}

// TestLatestStatusBlocksPendingChildren защищает от залипания прежнего успеха.
// Даже если сборщик уже был готов, новый статус родителя до фиксации Starting
// обязан убрать его из Ready. План не является разрешением на будущие запуски.
func TestLatestStatusBlocksPendingChildren(t *testing.T) {
	w := reviewWorkflow(t)
	states := pendingStates(w)
	for _, id := range []string{"metrics", "architecture", "security"} {
		states[id] = Succeeded
	}
	for _, state := range []State{Running, WaitingForApproval, Failed, Cancelled, Unknown} {
		states["architecture"] = Succeeded
		before, err := Evaluate(w, states)
		if err != nil || !slices.Equal(before.Ready, []string{"summary"}) {
			t.Fatalf("сборщик не готов после успеха: %+v, %v", before, err)
		}
		states["architecture"] = state
		after, err := Evaluate(w, states)
		want := Plan{Waiting: []string{"summary"}}
		if err != nil || !reflect.DeepEqual(after, want) {
			t.Fatalf("старый успех принят вместо %q: %+v, %v", state, after, err)
		}
	}
}

// TestCompletionRequiresEverySuccess проверяет завершение без Pending-шагов:
// активный, упавший или потерявший связь чат тоже запрещает общий успех.
func TestCompletionRequiresEverySuccess(t *testing.T) {
	w := reviewWorkflow(t)
	for _, step := range w.Steps {
		for _, state := range []State{Pending, Starting, Unknown, Running, WaitingForApproval, Failed, Cancelled, Succeeded} {
			t.Run(step.ID+"/"+string(state), func(t *testing.T) {
				states := pendingStates(w)
				for id := range states {
					states[id] = Succeeded
				}
				states[step.ID] = state
				got, err := Evaluate(w, states)
				if err != nil || got.Complete != (state == Succeeded) {
					t.Fatalf("завершение при %q: %+v, %v", state, got, err)
				}
			})
		}
	}
}

// TestOnlyDirectDependenciesGateStart фиксирует отсутствие автоматического
// пересчёта: повтор метрик не обесценивает уже завершённые ревью. Сборщик ждёт
// именно их, но весь workflow ещё не успешен, пока метрики снова выполняются.
func TestOnlyDirectDependenciesGateStart(t *testing.T) {
	w := reviewWorkflow(t)
	states := map[string]State{
		"summary": Pending, "architecture": Succeeded, "security": Succeeded, "metrics": Running,
	}
	got, err := Evaluate(w, states)
	want := Plan{Ready: []string{"summary"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("повтор предка изменил готовность сборщика: %+v, %v", got, err)
	}
}

// TestRejectsInvalidSnapshot запрещает частичный план при повреждённом состоянии.
// В частности, потерянный шаг нельзя молча превратить в Pending и создать повторно.
func TestRejectsInvalidSnapshot(t *testing.T) {
	w := reviewWorkflow(t)
	for _, tc := range []struct {
		name   string
		mutate func(map[string]State)
	}{
		{"пустой снимок", func(s map[string]State) { clear(s) }},
		{"пропущен шаг", func(s map[string]State) { delete(s, "security") }},
		{"лишний шаг", func(s map[string]State) { s["other"] = Pending }},
		{"подмена ключа", func(s map[string]State) { delete(s, "security"); s["other"] = Pending }},
		{"нулевое состояние", func(s map[string]State) { s["security"] = "" }},
		{"неизвестное состояние", func(s map[string]State) { s["security"] = "idle" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := pendingStates(w)
			tc.mutate(states)
			got, err := Evaluate(w, states)
			if err == nil || !reflect.DeepEqual(got, Plan{}) {
				t.Fatalf("повреждённый снимок дал план: %+v, %v", got, err)
			}
		})
	}
	if got, err := Evaluate(w, nil); err == nil || !reflect.DeepEqual(got, Plan{}) {
		t.Fatalf("nil-снимок дал план: %+v, %v", got, err)
	}
}

// TestRejectsInvalidWorkflow проверяет общий валидатор на границе планировщика:
// нельзя использовать некорректный граф даже при полном, успешном снимке.
func TestRejectsInvalidWorkflow(t *testing.T) {
	for _, mutate := range []func(*workflow.Workflow){
		func(w *workflow.Workflow) { w.ID = "" },
		func(w *workflow.Workflow) { w.Steps = nil },
		func(w *workflow.Workflow) { w.Steps[0].DependsOn = []string{"missing"} },
		func(w *workflow.Workflow) { w.Steps[3].DependsOn = []string{"summary"} },
	} {
		w := reviewWorkflow(t)
		states := pendingStates(w)
		for id := range states {
			states[id] = Succeeded
		}
		mutate(&w)
		if got, err := Evaluate(w, states); err == nil || !reflect.DeepEqual(got, Plan{}) {
			t.Fatalf("неверный граф дал план: %+v, %v", got, err)
		}
	}
}

// TestSnapshotsAreNotMutated проверяет отсутствие скрытого резервирования шагов
// и ссылок из результата на исходный граф. Однократность обеспечивается только
// после фиксации Starting вызывающим кодом, а не повторным чтением того же снимка.
func TestSnapshotsAreNotMutated(t *testing.T) {
	w := reviewWorkflow(t)
	states := pendingStates(w)
	before := maps.Clone(states)
	first, err := Evaluate(w, states)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Evaluate(w, states)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("повтор изменил план: %+v, %+v, %v", first, second, err)
	}
	first.Ready[0], first.Waiting[0] = "changed", "changed"
	if !maps.Equal(states, before) || !reflect.DeepEqual(w, reviewWorkflow(t)) || second.Ready[0] != "metrics" || second.Waiting[0] != "summary" {
		t.Fatal("вычисление или изменение результата затронуло входы либо другой план")
	}
}

// TestWideAndDeepGraphs защищает отсутствие лимита параллельности и рекурсии.
// Порядок Steps обратный; планировщик не должен навязывать порядок из JSON.
func TestWideAndDeepGraphs(t *testing.T) {
	for _, chain := range []bool{false, true} {
		w := workflow.Workflow{ID: "large"}
		for i := 9999; i >= 0; i-- {
			deps := []string{}
			if chain && i > 0 {
				deps = append(deps, fmt.Sprint(i-1))
			}
			w.Steps = append(w.Steps, workflow.Step{ID: fmt.Sprint(i), Type: "agent", Prompt: "задача", DependsOn: deps})
		}
		got, err := Evaluate(w, pendingStates(w))
		wantReady, wantWaiting := 10000, 0
		if chain {
			wantReady, wantWaiting = 1, 9999
		}
		if err != nil || got.Complete || len(got.Ready) != wantReady || len(got.Waiting) != wantWaiting {
			t.Fatalf("цепочка=%v: готово %d, ждёт %d, завершено=%v, ошибка=%v", chain, len(got.Ready), len(got.Waiting), got.Complete, err)
		}
		if chain && got.Ready[0] != "0" {
			t.Fatal("цепочка должна начинаться с корня, а не первого элемента steps")
		}
	}
}
