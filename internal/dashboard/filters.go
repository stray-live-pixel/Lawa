package dashboard

import (
	"html/template"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPeriod = "24h"
	defaultScope  = "active"
	defaultStates = "all"
	maxQueryRunes = 300
	maxRootRunes  = 200
)

// periodDefinition задаёт стабильное значение query-параметра и точную длительность.
// Месяц в dashboard — скользящие 30 дней, а не календарный месяц: так граница
// фильтра не зависит от числа дней и часового пояса запуска сервера.
type periodDefinition struct {
	Value, Label string
	Duration     time.Duration
}

var periodDefinitions = []periodDefinition{
	{Value: "1h", Label: "За последний час", Duration: time.Hour},
	{Value: "2h", Label: "За последние 2 часа", Duration: 2 * time.Hour},
	{Value: "4h", Label: "За последние 4 часа", Duration: 4 * time.Hour},
	{Value: "8h", Label: "За последние 8 часов", Duration: 8 * time.Hour},
	{Value: "12h", Label: "За последние 12 часов", Duration: 12 * time.Hour},
	{Value: "24h", Label: "За последние 24 часа", Duration: 24 * time.Hour},
	{Value: "2d", Label: "За последние 2 дня", Duration: 2 * 24 * time.Hour},
	{Value: "5d", Label: "За последние 5 дней", Duration: 5 * 24 * time.Hour},
	{Value: "7d", Label: "За последнюю неделю", Duration: 7 * 24 * time.Hour},
	{Value: "14d", Label: "За последние 2 недели", Duration: 14 * 24 * time.Hour},
	{Value: "30d", Label: "За последний месяц", Duration: 30 * 24 * time.Hour},
	{Value: "all", Label: "За всё время"},
}

type viewParams struct {
	Query, Period, Scope, States, RootID string
	Page                                 int
}

type periodOption struct {
	Value, Label string
	Selected     bool
}

type filterView struct {
	Query, WindowLabel, Scope, Period, States, RootID   string
	ActiveURL, AllURL                                   template.URL
	AllStatesURL, WorkingURL, FailedURL                 template.URL
	Periods                                             []periodOption
	FocusPath                                           []focusPart
	FocusParentID                                       string
	Total, SearchMatched, Matched                       int
	HasActiveQuery, ActiveOnly, WorkingOnly, FailedOnly bool
	Focused                                             bool
}

// focusPart — один доступный переход в пути закреплённого workflow. Состояние
// нужно только для цветной папки; URL строит JavaScript из уже выбранных фильтров.
type focusPart struct{ ID, Name, State, Tone string }

type paginationItem struct {
	Label   string
	URL     template.URL
	Current bool
}

type paginationView struct {
	Current, Total       int
	PreviousURL, NextURL template.URL
	Items                []paginationItem
	Visible              bool
}

// parseViewParams принимает только известный период, положительную страницу и
// ограниченную строку поиска. Неверная ссылка мягко возвращается к первой странице
// и 24 часам, не превращая ошибку query string в падение всего dashboard.
func parseViewParams(values url.Values) viewParams {
	query := strings.TrimSpace(values.Get("q"))
	runes := []rune(query)
	if len(runes) > maxQueryRunes {
		query = string(runes[:maxQueryRunes])
	}
	period := values.Get("period")
	if _, ok := findPeriod(period); !ok {
		period = defaultPeriod
	}
	page, err := strconv.Atoi(values.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	scope := values.Get("view")
	if scope != "all" {
		scope = defaultScope
	}
	states := values.Get("states")
	if states != "working" && states != "failed" {
		states = defaultStates
	}
	rootID := strings.TrimSpace(values.Get("root"))
	rootRunes := []rune(rootID)
	if len(rootRunes) > maxRootRunes {
		rootID = string(rootRunes[:maxRootRunes])
	}
	return viewParams{Query: query, Period: period, Scope: scope, States: states, RootID: rootID, Page: page}
}

// applyDashboardView сначала применяет статусный фильтр к целым деревьям, затем
// ищет и выбирает временное окно. В режиме active дерево остаётся видимым, если
// незавершён хотя бы один вложенный workflow: терминальная ошибка соседней ветки
// не должна скрыть продолжающуюся работу. Страница означает соседний временной
// интервал, а не лимит элементов: все совпавшие корни окна показываются целиком.
func applyDashboardView(roots []*runNode, params viewParams, now time.Time) ([]*runNode, filterView, paginationView) {
	if params.Scope != "all" {
		params.Scope = defaultScope
	}
	if params.States != "working" && params.States != "failed" {
		params.States = defaultStates
	}
	viewRoots, focusPath := focusDashboardRoots(roots, params.RootID)
	if params.RootID != "" && len(focusPath) == 0 {
		params.RootID = ""
	}
	period, _ := findPeriod(params.Period)
	needle := strings.ToLower(params.Query)
	searchMatched := make([]*runNode, 0, len(viewRoots))
	for _, original := range viewRoots {
		root := original
		if params.Scope == defaultScope && !root.HasUnfinished {
			continue
		}
		if params.States == "working" {
			root = workingTree(root)
			if root == nil {
				continue
			}
		} else if params.States == "failed" {
			root = failedTree(root)
			if root == nil {
				continue
			}
		}
		if needle != "" && !strings.Contains(root.searchText, needle) {
			continue
		}
		searchMatched = append(searchMatched, root)
	}

	totalPages := timeWindowCount(searchMatched, period.Duration, now)
	current := params.Page
	if current > totalPages {
		current = totalPages
	}
	visible := make([]*runNode, 0, len(searchMatched))
	windowEnd := now
	windowStart := time.Time{}
	if period.Duration > 0 {
		windowEnd = now.Add(-time.Duration(current-1) * period.Duration)
		windowStart = windowEnd.Add(-period.Duration)
	}
	for _, root := range searchMatched {
		if period.Duration == 0 {
			visible = append(visible, root)
			continue
		}
		// Живое дерево показывается в текущем окне даже при старом meta.json, но
		// не повторяется на каждой исторической странице.
		if root.HasUnfinished {
			if current == 1 {
				visible = append(visible, root)
			}
			continue
		}
		if !root.activityAt.Before(windowStart) && (current == 1 || root.activityAt.Before(windowEnd)) {
			visible = append(visible, root)
		}
	}
	sort.SliceStable(visible, func(i, j int) bool {
		if visible[i].activityAt.Equal(visible[j].activityAt) {
			return visible[i].ID < visible[j].ID
		}
		return visible[i].activityAt.After(visible[j].activityAt)
	})

	filter := filterView{
		Query: params.Query, Scope: params.Scope, Period: params.Period, States: params.States, RootID: params.RootID,
		Total: len(viewRoots), SearchMatched: len(searchMatched), Matched: len(visible),
		HasActiveQuery: params.Query != "", ActiveOnly: params.Scope == defaultScope,
		WorkingOnly: params.States == "working", FailedOnly: params.States == "failed",
		WindowLabel: formatWindowLabel(windowStart, windowEnd, period.Duration, current),
	}
	filter.ActiveURL = scopeURL(params, defaultScope)
	filter.AllURL = scopeURL(params, "all")
	filter.AllStatesURL = statesURL(params, defaultStates)
	filter.WorkingURL = statesURL(params, "working")
	filter.FailedURL = statesURL(params, "failed")
	if len(focusPath) != 0 {
		filter.Focused = true
		for _, node := range focusPath {
			filter.FocusPath = append(filter.FocusPath, focusPart{ID: node.ID, Name: node.Name, State: node.State, Tone: node.Tone})
		}
		if len(focusPath) > 1 {
			filter.FocusParentID = focusPath[len(focusPath)-2].ID
		}
	}
	for _, definition := range periodDefinitions {
		filter.Periods = append(filter.Periods, periodOption{Value: definition.Value, Label: definition.Label, Selected: definition.Value == params.Period})
	}
	pagination := buildPagination(params, current, totalPages)
	return visible, filter, pagination
}

// timeWindowCount ограничивает переход «Раньше» окном, содержащим самый старый
// результат поиска. Пустые промежуточные окна остаются доступны: это сохраняет
// честную временную шкалу и позволяет сравнивать одинаковые интервалы.
func timeWindowCount(roots []*runNode, duration time.Duration, now time.Time) int {
	if duration == 0 || len(roots) == 0 {
		return 1
	}
	total := 1
	for _, root := range roots {
		if root.HasUnfinished {
			continue
		}
		age := now.Sub(root.activityAt)
		if age <= 0 {
			continue
		}
		pages := int(age / duration)
		if age%duration != 0 {
			pages++
		}
		if pages > total {
			total = pages
		}
	}
	return total
}

func formatWindowLabel(start, end time.Time, duration time.Duration, current int) string {
	if duration == 0 {
		return "Всё время"
	}
	const layout = "02.01.2006 15:04"
	if current == 1 {
		return "С " + start.Format(layout) + " до текущего момента"
	}
	return start.Format(layout) + " — " + end.Format(layout)
}

func findPeriod(value string) (periodDefinition, bool) {
	for _, definition := range periodDefinitions {
		if definition.Value == value {
			return definition, true
		}
	}
	return periodDefinition{}, false
}

// finalizeTree строит агрегаты после связывания детей с родителями. Поиск по
// ребёнку или его кубику возвращает целое дерево, activityAt выбирает самое свежее
// изменение, а HasUnfinished независимо от итогового цвета запоминает работу в
// любой ветке. Разделение необходимо для дерева с одновременно упавшим и всё ещё
// выполняющимся потомком: цвет сообщает об ошибке, фильтр не скрывает работу.
func finalizeTree(node *runNode) {
	node.activityAt = node.updatedAt
	node.treeState = normalizeTreeState(node.State)
	node.HasUnfinished = node.State != "failed" && node.State != "succeeded"
	node.HasWorking = isWorkingState(node.State) || len(node.ActiveSteps) != 0
	node.HasFailed = isFailedState(node.State)
	for _, step := range node.Steps {
		node.HasFailed = node.HasFailed || isFailedState(step.State)
	}
	parts := []string{node.searchText}
	for _, child := range node.Children {
		finalizeTree(child)
		parts = append(parts, child.searchText)
		if child.activityAt.After(node.activityAt) {
			node.activityAt = child.activityAt
		}
		node.treeState = mergeTreeState(node.treeState, child.treeState)
		node.HasUnfinished = node.HasUnfinished || child.HasUnfinished
		node.HasWorking = node.HasWorking || child.HasWorking
		node.HasFailed = node.HasFailed || child.HasFailed
	}
	node.searchText = strings.ToLower(strings.Join(parts, "\n"))
}

func isWorkingState(state string) bool {
	return state == "running" || state == "starting" || state == "waiting_for_approval"
}

func isFailedState(state string) bool {
	return state == "failed" || state == "cancelled" || state == "unknown"
}

// workingTree создаёт проекцию только для HTML и не меняет исходные snapshot.
// Папка-предок остаётся в дереве, если работа идёт глубже, а её завершённые кубики
// и соседние терминальные workflow скрываются. Копия нужна, чтобы другой запрос с
// режимом «Все состояния» продолжал видеть полное дерево.
func workingTree(node *runNode) *runNode {
	if !node.HasWorking {
		return nil
	}
	children := make([]*runNode, 0, len(node.Children))
	for _, child := range node.Children {
		if projected := workingTree(child); projected != nil {
			children = append(children, projected)
		}
	}
	steps := make([]stepNode, 0, len(node.ActiveSteps))
	steps = append(steps, node.ActiveSteps...)
	if len(steps) == 0 && len(children) == 0 && !isWorkingState(node.State) {
		return nil
	}
	clone := *node
	clone.Steps, clone.ActiveSteps, clone.Children = steps, steps, children
	clone.Open = true
	return &clone
}

// failedTree оставляет красные состояния и папки-предки к ним. Cancelled и
// unknown входят сюда вместе с failed: tone показывает их тем же аварийным цветом,
// поэтому фильтр соответствует визуальному языку дерева.
func failedTree(node *runNode) *runNode {
	if !node.HasFailed {
		return nil
	}
	children := make([]*runNode, 0, len(node.Children))
	for _, child := range node.Children {
		if projected := failedTree(child); projected != nil {
			children = append(children, projected)
		}
	}
	steps := make([]stepNode, 0, len(node.Steps))
	for _, step := range node.Steps {
		if isFailedState(step.State) {
			steps = append(steps, step)
		}
	}
	clone := *node
	clone.Steps, clone.ActiveSteps, clone.Children = steps, nil, children
	clone.Open = true
	return &clone
}

// focusDashboardRoots находит единственный закреплённый workflow и полный путь
// от исходного корня. Неизвестный root мягко возвращает обычное дерево: устаревшая
// закладка не должна превращать dashboard в пустой экран.
func focusDashboardRoots(roots []*runNode, rootID string) ([]*runNode, []*runNode) {
	if rootID == "" {
		return roots, nil
	}
	if path := findNodePath(roots, rootID, nil); len(path) != 0 {
		return []*runNode{path[len(path)-1]}, path
	}
	return roots, nil
}

func findNodePath(nodes []*runNode, id string, parents []*runNode) []*runNode {
	for _, node := range nodes {
		path := append(append([]*runNode(nil), parents...), node)
		if node.ID == id {
			return path
		}
		if found := findNodePath(node.Children, id, path); len(found) != 0 {
			return found
		}
	}
	return nil
}

func normalizeTreeState(state string) string {
	switch state {
	case "failed":
		return "failed"
	case "succeeded":
		return "succeeded"
	default:
		return "running"
	}
}

func mergeTreeState(left, right string) string {
	if left == "failed" || right == "failed" {
		return "failed"
	}
	if left == "running" || right == "running" {
		return "running"
	}
	return "succeeded"
}

func buildPagination(params viewParams, current, total int) paginationView {
	view := paginationView{Current: current, Total: total, Visible: total > 1}
	if current > 1 {
		view.PreviousURL = viewURL(params, current-1)
	}
	if current < total {
		view.NextURL = viewURL(params, current+1)
	}
	if total < 1 {
		return view
	}
	numbers := pageNumbers(current, total)
	previous := 0
	for _, number := range numbers {
		if previous != 0 && number-previous > 1 {
			view.Items = append(view.Items, paginationItem{Label: "…"})
		}
		view.Items = append(view.Items, paginationItem{Label: strconv.Itoa(number), URL: viewURL(params, number), Current: number == current})
		previous = number
	}
	return view
}

func pageNumbers(current, total int) []int {
	if total <= 7 {
		numbers := make([]int, total)
		for index := range numbers {
			numbers[index] = index + 1
		}
		return numbers
	}
	set := map[int]bool{1: true, total: true, current: true}
	for _, number := range []int{current - 1, current + 1} {
		if number > 1 && number < total {
			set[number] = true
		}
	}
	numbers := make([]int, 0, len(set))
	for number := range set {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	return numbers
}

// viewURL сохраняет выбранные фильтры при переходе между страницами. Значения
// кодирует net/url; template.URL получает только построенный здесь относительный URL.
func viewURL(params viewParams, page int) template.URL {
	values := make(url.Values)
	values.Set("period", params.Period)
	if params.Scope == "all" {
		values.Set("view", "all")
	}
	if params.States != defaultStates {
		values.Set("states", params.States)
	}
	if params.RootID != "" {
		values.Set("root", params.RootID)
	}
	if params.Query != "" {
		values.Set("q", params.Query)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	return template.URL("?" + values.Encode())
}

// scopeURL меняет только статусную выборку и возвращает её на первое временное
// окно. Поиск и период сохраняются, чтобы две кнопки работали как один фильтр, а
// не как полный сброс пользовательского контекста.
func scopeURL(params viewParams, scope string) template.URL {
	params.Scope, params.Page = scope, 1
	return viewURL(params, 1)
}

// statesURL переключает состав строк внутри дерева, сохраняя остальные фильтры.
func statesURL(params viewParams, states string) template.URL {
	params.States, params.Page = states, 1
	return viewURL(params, 1)
}
