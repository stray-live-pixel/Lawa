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
	maxQueryRunes = 300
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
	Query, Period string
	Page          int
}

type periodOption struct {
	Value, Label string
	Selected     bool
}

type filterView struct {
	Query                         string
	WindowLabel                   string
	Periods                       []periodOption
	Total, SearchMatched, Matched int
	HasActiveQuery                bool
}

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
	return viewParams{Query: query, Period: period, Page: page}
}

// applyDashboardView сначала ищет по целым деревьям, а затем выбирает временное
// окно. Страница означает не количество элементов, а соседний интервал выбранной
// длины: page=1 — последние N часов/дней, page=2 — предыдущие N и так далее.
// Все совпавшие корневые run окна показываются без количественного лимита.
func applyDashboardView(roots []*runNode, params viewParams, now time.Time) ([]*runNode, filterView, paginationView) {
	period, _ := findPeriod(params.Period)
	needle := strings.ToLower(params.Query)
	searchMatched := make([]*runNode, 0, len(roots))
	for _, root := range roots {
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
		if root.treeState == "running" {
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
		Query: params.Query, Total: len(roots), SearchMatched: len(searchMatched), Matched: len(visible),
		HasActiveQuery: params.Query != "", WindowLabel: formatWindowLabel(windowStart, windowEnd, period.Duration, current),
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
		if root.treeState == "running" {
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
// изменение в нём, а treeState не прячет активного ребёнка под успешным родителем.
func finalizeTree(node *runNode) {
	node.activityAt = node.updatedAt
	node.treeState = normalizeTreeState(node.State)
	parts := []string{node.searchText}
	for _, child := range node.Children {
		finalizeTree(child)
		parts = append(parts, child.searchText)
		if child.activityAt.After(node.activityAt) {
			node.activityAt = child.activityAt
		}
		node.treeState = mergeTreeState(node.treeState, child.treeState)
	}
	node.searchText = strings.ToLower(strings.Join(parts, "\n"))
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
	if params.Query != "" {
		values.Set("q", params.Query)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	return template.URL("?" + values.Encode())
}
