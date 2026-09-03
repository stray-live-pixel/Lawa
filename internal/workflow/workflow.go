// Package workflow читает и проверяет схему Lawa без файловых и сетевых побочных
// эффектов. Общий валидатор используют validate, run и чтение сохранённого resume.
package workflow

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Speed — выбранный пользователем режим обслуживания запроса Codex. Отдельный
// тип не даёт coordinator перепутать продуктовые значения normal/fast с сырым
// serviceTier протокола, который может расширяться независимо от схемы Lawa.
type Speed string

const (
	SpeedNormal Speed = "normal"
	SpeedFast   Speed = "fast"
)

const (
	// VersionLegacy — исходный контракт строгого DAG. Отсутствующее поле version
	// означает именно эту версию, поэтому старые workflow сохраняют прежний JSON и
	// поведение без неявной миграции.
	VersionLegacy = 1
	// VersionAgentGraph описывает граф с явными стартами, агентными решениями и
	// ограниченными циклами. Новые запуски этой версии получают visit-aware
	// metadata v4; сохранённые версии выбирают подходящий runtime при resume.
	VersionAgentGraph = 2
)

// TerminalOutcome — явно выбранный итог workflow. Других значений, в том числе
// внутренних состояний отдельных turn, в пользовательском контракте нет.
type TerminalOutcome string

const (
	OutcomeSucceeded TerminalOutcome = "succeeded"
	OutcomeFailed    TerminalOutcome = "failed"
)

// Route — один статически разрешённый результат агентного решения. To запускает
// перечисленные цели параллельно, а Finish завершает весь workflow. Ровно одна
// форма обязательна; это проверяет Workflow.Validate после чтения всех ID.
type Route struct {
	To     []string         `json:"to,omitzero"`
	Finish *TerminalOutcome `json:"finish,omitempty"`
}

// Workflow описывает неизменяемый вход запуска. Model задаёт общую модель для
// шагов без собственного override; nil оставляет выбор конфигурации Codex. Порядок
// Steps не задаёт зависимости, но служит стабильным порядком планирования.
type Workflow struct {
	Version *int     `json:"version,omitempty"`
	ID      string   `json:"id"`
	Model   *string  `json:"model,omitempty"`
	Start   []string `json:"start,omitzero"`
	Steps   []Step   `json:"steps"`
}

// EffectiveVersion возвращает семантическую версию, не меняя представление
// отсутствующего поля. Это различие нужно, чтобы повторное JSON-кодирование старого
// workflow не добавляло version=1 и сохраняло прежний внешний контракт.
func (w Workflow) EffectiveVersion() int {
	if w.Version == nil {
		return VersionLegacy
	}
	return *w.Version
}

// Step — задача агента. ID является ключом графа, а не путём или ID чата Codex.
// В v1 пустой DependsOn разрешает старт без ожидания, в v2 входящие технические
// рёбра задаёт After, а присутствие Decisions превращает запуск в кубик решения.
// Model, Effort и Speed — указатели, потому что отсутствие поля означает
// наследование: Model сначала берётся из Workflow, остальные настройки — из Codex.
// Явное значение попадает в неизменяемый снимок run и используется при продолжении.
type Step struct {
	ID        string           `json:"id"`
	Type      string           `json:"type"`
	Prompt    string           `json:"prompt"`
	DependsOn []string         `json:"dependsOn,omitzero"`
	After     []string         `json:"after,omitzero"`
	Decisions map[string]Route `json:"decisions,omitzero"`
	MaxVisits *int             `json:"maxVisits,omitempty"`
	OnLimit   *TerminalOutcome `json:"onLimit,omitempty"`
	Model     *string          `json:"model,omitempty"`
	Effort    *string          `json:"effort,omitempty"`
	Speed     *Speed           `json:"speed,omitempty"`
}

// optionalInteger хранит присутствие целого JSON-поля. Указатель в публичной
// модели нужен для version и maxVisits, но явный null не должен совпасть с
// отсутствием поля и незаметно включить legacy/default-семантику.
type optionalInteger struct {
	value   int
	present bool
}

func (value *optionalInteger) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	if decoder.PeekKind() == 'n' {
		if _, err := decoder.ReadValue(); err != nil {
			return err
		}
		return fmt.Errorf("явный null недопустим")
	}
	if err := json.UnmarshalDecode(decoder, &value.value); err != nil {
		return err
	}
	value.present = true
	return nil
}

func (value optionalInteger) pointer() *int {
	if !value.present {
		return nil
	}
	result := value.value
	return &result
}

// optionalTerminalOutcome отделяет отсутствие finish/onLimit от явного null.
// Значение проверяется позже вместе с остальной семантикой route или лимита.
type optionalTerminalOutcome struct {
	value   TerminalOutcome
	present bool
}

// UnmarshalJSONFrom принимает только строку и не позволяет null превратиться в
// отсутствие terminal outcome.
func (outcome *optionalTerminalOutcome) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	if decoder.PeekKind() == 'n' {
		if _, err := decoder.ReadValue(); err != nil {
			return err
		}
		return fmt.Errorf("явный null недопустим; нужен terminal outcome")
	}
	if err := json.UnmarshalDecode(decoder, &outcome.value); err != nil {
		return err
	}
	outcome.present = true
	return nil
}

// pointer возвращает отдельную копию только для присутствовавшего поля.
func (outcome optionalTerminalOutcome) pointer() *TerminalOutcome {
	if !outcome.present {
		return nil
	}
	value := outcome.value
	return &value
}

// stringList отличает отсутствующее поле от явного пустого массива и отклоняет
// null на границе декодирования. Это позволяет v1 требовать dependsOn, а v2 —
// after, не принимая неоднозначное смешение двух контрактов.
type stringList []string

func (list *stringList) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	if decoder.PeekKind() == 'n' {
		if _, err := decoder.ReadValue(); err != nil {
			return err
		}
		return fmt.Errorf("явный null недопустим; нужен JSON-массив")
	}
	var values []string
	if err := json.UnmarshalDecode(decoder, &values); err != nil {
		return err
	}
	*list = values
	return nil
}

// decisionMap отклоняет null, но сохраняет явный пустой объект: Validate выдаёт
// для него предметную ошибку вместо молчаливого превращения в обычный кубик.
type decisionMap map[string]Route

func (decisions *decisionMap) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	if decoder.PeekKind() == 'n' {
		if _, err := decoder.ReadValue(); err != nil {
			return err
		}
		return fmt.Errorf("явный null недопустим; нужен объект решений")
	}
	var values map[string]Route
	if err := json.UnmarshalDecode(decoder, &values, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*decisions = values
	return nil
}

// optionalRuntimeSetting используется при чтении Workflow и Step и сохраняет факт
// присутствия JSON-поля отдельно от его значения. Обычный указатель Go не подходит:
// encoding/json одинаково превращает отсутствующее поле и явный null в nil, хотя
// в контракте workflow только отсутствие разрешает наследование следующего уровня.
type optionalRuntimeSetting[T ~string] struct {
	value   T
	present bool
}

// UnmarshalJSONFrom отклоняет явный null до преобразования в Go-указатель. Значения
// остальных JSON-типов читает общий декодер: он сохраняет стандартную диагностику
// с путём до поля и проверку ожидаемого строкового типа.
func (setting *optionalRuntimeSetting[T]) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	if decoder.PeekKind() == 'n' {
		if _, err := decoder.ReadValue(); err != nil {
			return err
		}
		return fmt.Errorf("явный null недопустим; удалите поле, чтобы наследовать настройку")
	}
	if err := json.UnmarshalDecode(decoder, &setting.value); err != nil {
		return err
	}
	setting.present = true
	return nil
}

// pointer возвращает отдельный указатель только для присутствовавшего поля.
// Копия значения не позволяет последующему переиспользованию служебной структуры
// декодирования изменить уже собранный публичный Workflow или Step.
func (setting optionalRuntimeSetting[T]) pointer() *T {
	if !setting.present {
		return nil
	}
	value := setting.value
	return &value
}

// workflowJSON отделяет корневой model от публичной структуры по той же причине,
// что и настройки Step: отсутствие наследует Codex, а явный null является ошибкой.
type workflowJSON struct {
	Version optionalInteger                `json:"version"`
	ID      string                         `json:"id"`
	Model   optionalRuntimeSetting[string] `json:"model"`
	Start   stringList                     `json:"start"`
	Steps   []Step                         `json:"steps"`
}

// UnmarshalJSONFrom собирает публичный Workflow после проверки присутствия model.
// Строгий режим сохраняет запрет неизвестных корневых полей независимо от того,
// с какими опциями вызывающий код использует этот тип.
func (workflow *Workflow) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	var raw workflowJSON
	if err := json.UnmarshalDecode(decoder, &raw, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*workflow = Workflow{
		Version: raw.Version.pointer(), ID: raw.ID, Model: raw.Model.pointer(),
		Start: []string(raw.Start), Steps: raw.Steps,
	}
	return nil
}

// stepJSON отделяет присутствие необязательных полей от публичной модели Step.
// Обязательные поля остаются обычными Go-значениями и проходят прежнюю Validate.
type stepJSON struct {
	ID        string                         `json:"id"`
	Type      string                         `json:"type"`
	Prompt    string                         `json:"prompt"`
	DependsOn stringList                     `json:"dependsOn"`
	After     stringList                     `json:"after"`
	Decisions decisionMap                    `json:"decisions"`
	MaxVisits optionalInteger                `json:"maxVisits"`
	OnLimit   optionalTerminalOutcome        `json:"onLimit"`
	Model     optionalRuntimeSetting[string] `json:"model"`
	Effort    optionalRuntimeSetting[string] `json:"effort"`
	Speed     optionalRuntimeSetting[Speed]  `json:"speed"`
}

// UnmarshalJSONFrom сохраняет прежний удобный контракт Step с указателями, но
// создаёт их только для реально переданных строковых значений. Вложенный вызов
// явно сохраняет строгий запрет неизвестных полей, потому что пользовательская
// схема не должна становиться мягче из-за собственного декодера Step.
func (step *Step) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	var raw stepJSON
	if err := json.UnmarshalDecode(decoder, &raw, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*step = Step{
		ID: raw.ID, Type: raw.Type, Prompt: raw.Prompt,
		DependsOn: []string(raw.DependsOn), After: []string(raw.After), Decisions: map[string]Route(raw.Decisions),
		MaxVisits: raw.MaxVisits.pointer(), OnLimit: raw.OnLimit.pointer(),
		Model: raw.Model.pointer(), Effort: raw.Effort.pointer(), Speed: raw.Speed.pointer(),
	}
	return nil
}

// routeJSON нужен для того же строгого режима, что workflowJSON и stepJSON:
// неизвестное поле внутри route не должно теряться из-за пользовательского
// UnmarshalJSONFrom у окружающего Step.
type routeJSON struct {
	To     stringList              `json:"to"`
	Finish optionalTerminalOutcome `json:"finish"`
}

// UnmarshalJSONFrom отклоняет null и неизвестные поля до семантической проверки
// взаимоисключающих форм route.
func (route *Route) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	if decoder.PeekKind() == 'n' {
		if _, err := decoder.ReadValue(); err != nil {
			return err
		}
		return fmt.Errorf("route не может быть null")
	}
	var raw routeJSON
	if err := json.UnmarshalDecode(decoder, &raw, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*route = Route{To: []string(raw.To), Finish: raw.Finish.pointer()}
	return nil
}

// Decode читает ровно один JSON-объект и возвращает только целиком корректный граф.
// При ошибке результат пустой: вызывающий код не должен запускать часть схемы.
// Идентификаторы и промпты сохраняются без обрезки пробелов и других исправлений.
// json/v2 отклоняет повторные ключи и повреждённый Unicode, учитывает регистр
// имён. UnmarshalRead также требует EOF после объекта; неизвестные поля запрещаем
// явно. Проверка обязательных полей и связей остаётся в общем Validate.
func Decode(r io.Reader) (Workflow, error) {
	var w Workflow
	if err := json.UnmarshalRead(r, &w, json.RejectUnknownMembers(true)); err != nil {
		return Workflow{}, fmt.Errorf("workflow: %w", err)
	}
	if err := w.Validate(); err != nil {
		return Workflow{}, err
	}
	return w, nil
}

// Validate выбирает один из двух взаимоисключающих контрактов. Legacy сохраняет
// прежний строгий DAG, а v2 использует start/after/decisions и допускает циклы
// только через ограниченное агентное решение. Проверка ничего не нормализует и не
// меняет: порядок Steps и массивов является частью детерминированного исполнения.
func (w Workflow) Validate() error {
	if strings.TrimSpace(w.ID) == "" || len(w.Steps) == 0 {
		return fmt.Errorf("нужны непустой id workflow и непустой массив steps")
	}
	version := w.EffectiveVersion()
	if version != VersionLegacy && version != VersionAgentGraph {
		return fmt.Errorf("неподдерживаемая версия workflow %d", version)
	}
	if err := validateOptionalSetting("workflow", "model", w.Model); err != nil {
		return err
	}
	indices := make(map[string]int, len(w.Steps))
	for i, s := range w.Steps {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("steps[%d]: нужен непустой id", i)
		}
		if _, exists := indices[s.ID]; exists {
			return fmt.Errorf("повторный id шага %q", s.ID)
		}
		indices[s.ID] = i
		if version == VersionLegacy && (s.Type != "agent" || strings.TrimSpace(s.Prompt) == "" || s.DependsOn == nil) {
			return fmt.Errorf("шаг %q: нужны type=agent, непустой prompt и массив dependsOn", s.ID)
		}
		if version == VersionAgentGraph && (s.Type != "agent" || strings.TrimSpace(s.Prompt) == "") {
			return fmt.Errorf("шаг %q: нужны type=agent и непустой prompt", s.ID)
		}
		subject := fmt.Sprintf("шаг %q", s.ID)
		if err := validateOptionalSetting(subject, "model", s.Model); err != nil {
			return err
		}
		if err := validateOptionalSetting(subject, "effort", s.Effort); err != nil {
			return err
		}
		if s.Speed != nil && *s.Speed != SpeedNormal && *s.Speed != SpeedFast {
			return fmt.Errorf("шаг %q: speed должен быть %q или %q", s.ID, SpeedNormal, SpeedFast)
		}
	}
	if version == VersionLegacy {
		return validateLegacy(w, indices)
	}
	return validateAgentGraph(w, indices)
}

// validateLegacy намеренно повторяет прежние правила и диагностику dependsOn.
// Любое присутствие v2-поля отклоняется, даже если массив или объект пуст.
func validateLegacy(w Workflow, indices map[string]int) error {
	if w.Start != nil {
		return fmt.Errorf("workflow v1: поле start относится только к version=2")
	}
	for _, step := range w.Steps {
		if step.DependsOn == nil {
			return fmt.Errorf("шаг %q: нужны type=agent, непустой prompt и массив dependsOn", step.ID)
		}
		if step.After != nil || step.Decisions != nil || step.MaxVisits != nil || step.OnLimit != nil {
			return fmt.Errorf("шаг %q: after, decisions, maxVisits и onLimit относятся только к version=2", step.ID)
		}
	}
	remaining, followers, err := dependencyGraph(w.Steps, indices, func(step Step) []string { return step.DependsOn }, "зависимость")
	if err != nil {
		return err
	}
	if !isDAG(remaining, followers) {
		return fmt.Errorf("обнаружен цикл зависимостей")
	}
	return nil
}

// validateAgentGraph проверяет v2 в несколько независимых проходов: сначала форму
// и ссылки, затем статический after-DAG, потенциальную достижимость, возможность
// завершения и, последней, защиту циклических компонент. Такой порядок даёт одну
// стабильную и наиболее локальную ошибку для одного и того же JSON.
func validateAgentGraph(w Workflow, indices map[string]int) error {
	if len(w.Start) == 0 {
		return fmt.Errorf("workflow v2: нужен непустой массив start")
	}
	if err := validateTargets("workflow start", w.Start, indices); err != nil {
		return err
	}
	for _, step := range w.Steps {
		if step.DependsOn != nil {
			return fmt.Errorf("шаг %q: dependsOn нельзя сочетать с workflow version=2; используйте after", step.ID)
		}
		if step.After == nil {
			return fmt.Errorf("шаг %q: workflow v2 требует явный массив after", step.ID)
		}
		if err := validateTargets(fmt.Sprintf("шаг %q, after", step.ID), step.After, indices); err != nil {
			return err
		}
		if step.Decisions != nil && len(step.Decisions) == 0 {
			return fmt.Errorf("шаг %q: decisions должен быть непустым объектом", step.ID)
		}
		if step.MaxVisits != nil && *step.MaxVisits <= 0 {
			return fmt.Errorf("шаг %q: maxVisits должен быть положительным", step.ID)
		}
		if step.OnLimit != nil {
			if step.MaxVisits == nil {
				return fmt.Errorf("шаг %q: onLimit требует maxVisits", step.ID)
			}
			if !validOutcome(*step.OnLimit) {
				return fmt.Errorf("шаг %q: onLimit должен быть %q или %q", step.ID, OutcomeSucceeded, OutcomeFailed)
			}
		}
		keys := sortedDecisionKeys(step.Decisions)
		for _, key := range keys {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("шаг %q: нужен непустой ключ решения", step.ID)
			}
			route := step.Decisions[key]
			hasTargets, hasFinish := route.To != nil, route.Finish != nil
			if hasTargets == hasFinish || hasTargets && len(route.To) == 0 {
				return fmt.Errorf("шаг %q, решение %q: нужен ровно один из непустого to или finish", step.ID, key)
			}
			if hasFinish && !validOutcome(*route.Finish) {
				return fmt.Errorf("шаг %q, решение %q: finish должен быть %q или %q", step.ID, key, OutcomeSucceeded, OutcomeFailed)
			}
			if hasTargets {
				if err := validateTargets(fmt.Sprintf("шаг %q, решение %q, to", step.ID, key), route.To, indices); err != nil {
					return err
				}
			}
		}
	}

	remaining, afterFollowers, err := dependencyGraph(w.Steps, indices, func(step Step) []string { return step.After }, "after")
	if err != nil {
		return err
	}
	if !isDAG(remaining, afterFollowers) {
		return fmt.Errorf("обнаружен цикл after; цикл допустим только через route агентного решения")
	}
	graph := fullGraph(w, indices, afterFollowers)
	reachable := reachableFrom(w.Start, indices, graph)
	for index, step := range w.Steps {
		if !reachable[index] {
			return fmt.Errorf("шаг %q недостижим из start", step.ID)
		}
	}
	canFinish := nodesWithFinishPath(w, graph)
	for index, step := range w.Steps {
		if reachable[index] && !canFinish[index] {
			return fmt.Errorf("шаг %q не имеет пути к конечному кубику или finish", step.ID)
		}
	}
	cyclic := cyclicNodes(graph)
	for index, step := range w.Steps {
		if cyclic[index] && len(step.Decisions) != 0 && step.MaxVisits == nil {
			return fmt.Errorf("шаг решения %q входит в цикл и требует положительный maxVisits", step.ID)
		}
	}
	return nil
}

func validOutcome(outcome TerminalOutcome) bool {
	return outcome == OutcomeSucceeded || outcome == OutcomeFailed
}

// validateTargets сохраняет порядок пользовательского массива в диагностике и
// отклоняет повтор как неоднозначную запись одного и того же перехода.
func validateTargets(subject string, targets []string, indices map[string]int) error {
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if _, exists := indices[target]; !exists {
			return fmt.Errorf("%s: неизвестная цель %q", subject, target)
		}
		if seen[target] {
			return fmt.Errorf("%s: повторная цель %q", subject, target)
		}
		seen[target] = true
	}
	return nil
}

func sortedDecisionKeys(decisions map[string]Route) []string {
	keys := make([]string, 0, len(decisions))
	for key := range decisions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// dependencyGraph строит входные счётчики и исходящие рёбра в порядке Steps.
// Вызывающий код заранее собрал indices, поэтому неизвестные ссылки здесь всё же
// проверяются как защита общей helper-функции и для прежней legacy-диагностики.
func dependencyGraph(steps []Step, indices map[string]int, dependencies func(Step) []string, label string) ([]int, [][]int, error) {
	remaining := make([]int, len(steps))
	followers := make([][]int, len(steps))
	for i, s := range steps {
		seen := make(map[string]bool)
		for _, dependency := range dependencies(s) {
			parent, exists := indices[dependency]
			if !exists {
				if label == "зависимость" {
					return nil, nil, fmt.Errorf("шаг %q: неизвестная зависимость %q", s.ID, dependency)
				}
				return nil, nil, fmt.Errorf("шаг %q: неизвестный %s %q", s.ID, label, dependency)
			}
			if seen[dependency] {
				if label == "зависимость" {
					return nil, nil, fmt.Errorf("шаг %q: повторная зависимость %q", s.ID, dependency)
				}
				return nil, nil, fmt.Errorf("шаг %q: повторный %s %q", s.ID, label, dependency)
			}
			seen[dependency] = true
			remaining[i]++
			followers[parent] = append(followers[parent], i)
		}
	}
	return remaining, followers, nil
}

// isDAG использует алгоритм Кана и не меняет переданный снимок счётчиков.
func isDAG(input []int, followers [][]int) bool {
	remaining := append([]int(nil), input...)
	// Алгоритм Кана обрабатывает каждую вершину и ребро один раз: O(V + E).
	// Очередь содержит только вершины без оставшихся входящих рёбер. Если после
	// обхода остались вершины, есть цикл. Рекурсии нет даже у очень длинной цепочки.
	var queue []int
	for i, count := range remaining {
		if count == 0 {
			queue = append(queue, i)
		}
	}
	for head := 0; head < len(queue); head++ {
		for _, next := range followers[queue[head]] {
			remaining[next]--
			if remaining[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	return len(queue) == len(remaining)
}

// fullGraph объединяет безусловные after-рёбра и все потенциальные decision-route.
// Повтор одного ребра в разных решениях не дублируется: за один visit выбирается
// ровно один ключ, а для достижимости и SCC важен только факт возможного перехода.
func fullGraph(w Workflow, indices map[string]int, afterFollowers [][]int) [][]int {
	graph := make([][]int, len(w.Steps))
	for source := range graph {
		seen := make(map[int]bool)
		for _, target := range afterFollowers[source] {
			if !seen[target] {
				graph[source] = append(graph[source], target)
				seen[target] = true
			}
		}
		for _, key := range sortedDecisionKeys(w.Steps[source].Decisions) {
			for _, targetID := range w.Steps[source].Decisions[key].To {
				target := indices[targetID]
				if !seen[target] {
					graph[source] = append(graph[source], target)
					seen[target] = true
				}
			}
		}
	}
	return graph
}

func reachableFrom(start []string, indices map[string]int, graph [][]int) []bool {
	reachable := make([]bool, len(graph))
	queue := make([]int, 0, len(start))
	for _, id := range start {
		index := indices[id]
		if !reachable[index] {
			reachable[index] = true
			queue = append(queue, index)
		}
	}
	for head := 0; head < len(queue); head++ {
		for _, next := range graph[queue[head]] {
			if !reachable[next] {
				reachable[next] = true
				queue = append(queue, next)
			}
		}
	}
	return reachable
}

// nodesWithFinishPath идёт по обратному графу от естественных листьев и явных
// finish/onLimit. Один maxVisits без выхода остаётся только аварийным предохранителем
// и не заменяет статически достижимое безопасное завершение workflow.
func nodesWithFinishPath(w Workflow, graph [][]int) []bool {
	reverse := make([][]int, len(graph))
	canFinish := make([]bool, len(graph))
	queue := make([]int, 0, len(graph))
	for source, targets := range graph {
		for _, target := range targets {
			reverse[target] = append(reverse[target], source)
		}
		hasFinish := w.Steps[source].OnLimit != nil
		for _, route := range w.Steps[source].Decisions {
			hasFinish = hasFinish || route.Finish != nil
		}
		if len(targets) == 0 || hasFinish {
			canFinish[source] = true
			queue = append(queue, source)
		}
	}
	for head := 0; head < len(queue); head++ {
		for _, previous := range reverse[queue[head]] {
			if !canFinish[previous] {
				canFinish[previous] = true
				queue = append(queue, previous)
			}
		}
	}
	return canFinish
}

// cyclicNodes находит strongly connected components алгоритмом Тарьяна. Результат
// нужен только как множество, а итоговая ошибка выбирается позднее в порядке Steps,
// поэтому обход не делает диагностику зависимой от map или порядка decisions.
func cyclicNodes(graph [][]int) []bool {
	indices := make([]int, len(graph))
	low := make([]int, len(graph))
	onStack := make([]bool, len(graph))
	for index := range indices {
		indices[index] = -1
	}
	stack := make([]int, 0, len(graph))
	cyclic := make([]bool, len(graph))
	nextIndex := 0
	var visit func(int)
	visit = func(node int) {
		indices[node], low[node] = nextIndex, nextIndex
		nextIndex++
		stack = append(stack, node)
		onStack[node] = true
		for _, target := range graph[node] {
			if indices[target] == -1 {
				visit(target)
				low[node] = min(low[node], low[target])
			} else if onStack[target] {
				low[node] = min(low[node], indices[target])
			}
		}
		if low[node] != indices[node] {
			return
		}
		component := make([]int, 0, 1)
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, last)
			if last == node {
				break
			}
		}
		if len(component) > 1 {
			for _, member := range component {
				cyclic[member] = true
			}
			return
		}
		for _, target := range graph[node] {
			if target == node {
				cyclic[node] = true
				break
			}
		}
	}
	for node := range graph {
		if indices[node] == -1 {
			visit(node)
		}
	}
	return cyclic
}

// validateOptionalSetting проверяет только устойчивый контракт Lawa: значение
// присутствует, корректно закодировано и является одним токеном. Список моделей
// и допустимых для них effort меняется на стороне Codex, поэтому совместимость
// пары проверяет сам app-server без молчаливой подстановки другого значения.
func validateOptionalSetting(subject, name string, value *string) error {
	if value == nil {
		return nil
	}
	if !utf8.ValidString(*value) || *value == "" || strings.IndexFunc(*value, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%s: %s должен быть непустым значением UTF-8 без пробельных символов", subject, name)
	}
	return nil
}
