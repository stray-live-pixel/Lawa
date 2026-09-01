package dashboard

import (
	"strings"
	"testing"
	"time"
)

func testFilterNode(id, state, search string, updated time.Time, children ...*runNode) *runNode {
	return &runNode{ID: id, Name: id, State: state, searchText: search, updatedAt: updated, Children: children}
}

func finalizeTestRoots(roots ...*runNode) []*runNode {
	for _, root := range roots {
		finalizeTree(root)
	}
	return roots
}

// TestTimePaginationUsesWindows фиксирует продуктовый смысл пагинации: один
// номер — один соседний временной интервал, а не лимит числа карточек. Граница
// принадлежит более новому окну, поэтому run не дублируется на двух страницах.
func TestTimePaginationUsesWindows(t *testing.T) {
	now := time.Date(2026, time.August, 31, 20, 0, 0, 0, time.Local)
	roots := finalizeTestRoots(
		testFilterNode("recent", "succeeded", "recent", now.Add(-30*time.Minute)),
		testFilterNode("boundary", "succeeded", "boundary", now.Add(-time.Hour)),
		testFilterNode("previous", "succeeded", "previous", now.Add(-90*time.Minute)),
		testFilterNode("old", "succeeded", "old", now.Add(-150*time.Minute)),
		testFilterNode("active", "running", "active", now.Add(-10*time.Hour)),
	)

	first, filter, pagination := applyDashboardView(roots, viewParams{Period: "1h", Scope: "all", Page: 1}, now)
	if got := nodeIDs(first); got != "recent,boundary,active" {
		t.Fatalf("неверное текущее окно: %s", got)
	}
	if filter.Matched != 3 || pagination.Total != 3 || pagination.PreviousURL != "" || !strings.Contains(string(pagination.NextURL), "page=2") {
		t.Fatalf("неверная пагинация первого окна: filter=%+v pagination=%+v", filter, pagination)
	}

	second, _, pagination := applyDashboardView(roots, viewParams{Period: "1h", Scope: "all", Page: 2}, now)
	if got := nodeIDs(second); got != "previous" || pagination.PreviousURL == "" || pagination.NextURL == "" {
		t.Fatalf("неверное предыдущее окно: roots=%s pagination=%+v", got, pagination)
	}
	third, _, _ := applyDashboardView(roots, viewParams{Period: "1h", Scope: "all", Page: 3}, now)
	if got := nodeIDs(third); got != "old" {
		t.Fatalf("неверное самое старое окно: %s", got)
	}
	all, _, pagination := applyDashboardView(roots, viewParams{Period: "all", Scope: "all", Page: 99}, now)
	if len(all) != len(roots) || pagination.Visible {
		t.Fatalf("режим «всё время» неожиданно режет элементы: roots=%d pagination=%+v", len(all), pagination)
	}
}

// TestTimeWindowDoesNotLimitItems защищает уточнение пользователя: даже большой
// набор одного часа целиком остаётся на одной странице.
func TestTimeWindowDoesNotLimitItems(t *testing.T) {
	now := time.Date(2026, time.August, 31, 20, 0, 0, 0, time.Local)
	roots := make([]*runNode, 0, 45)
	for index := 0; index < 45; index++ {
		roots = append(roots, testFilterNode(string(rune('a'+index)), "succeeded", "bulk", now.Add(-30*time.Minute)))
	}
	finalizeTestRoots(roots...)
	visible, filter, pagination := applyDashboardView(roots, viewParams{Period: "1h", Scope: "all", Page: 1}, now)
	if len(visible) != 45 || filter.Matched != 45 || pagination.Visible {
		t.Fatalf("элементы ошибочно ограничены количеством: visible=%d filter=%+v pagination=%+v", len(visible), filter, pagination)
	}
}

// TestSearchCoversTree проверяет регистронезависимое вхождение в ребёнка и
// сохранение фильтров в ссылках временной навигации.
func TestSearchCoversTree(t *testing.T) {
	now := time.Date(2026, time.August, 31, 20, 0, 0, 0, time.Local)
	child := testFilterNode("child", "succeeded", "Кубик THINKTWICE-592 codex-thread-42", now.Add(-90*time.Minute))
	parent := testFilterNode("parent", "succeeded", "dispatcher", now.Add(-2*time.Hour), child)
	other := testFilterNode("other", "succeeded", "unrelated", now.Add(-30*time.Minute))
	roots := finalizeTestRoots(parent, other)
	visible, filter, pagination := applyDashboardView(roots, viewParams{Query: "thinktwice-592", Period: "1h", Scope: "all", Page: 2}, now)
	if len(visible) != 1 || visible[0] != parent || filter.SearchMatched != 1 {
		t.Fatalf("поиск по ребёнку не сохранил дерево: visible=%v filter=%+v", visible, filter)
	}
	links := buildPagination(viewParams{Query: "a&b", Period: "1h", Page: 2}, 2, 3)
	if !strings.Contains(string(links.PreviousURL), "q=a%26b") || !strings.Contains(string(links.NextURL), "q=a%26b") || !pagination.Visible {
		t.Fatalf("фильтры потеряны при навигации: links=%+v searchPagination=%+v", links, pagination)
	}
}

// TestActiveScopeKeepsMixedTree защищает основной режим инспектора: завершённые
// корни скрыты, но ошибка в одной ветке не прячет выполняющегося соседа. Ссылки
// переключателя сохраняют поиск и период и не переносят историческую страницу.
func TestActiveScopeKeepsMixedTree(t *testing.T) {
	now := time.Now()
	mixed := testFilterNode("mixed", "failed", "mixed", now.Add(-time.Hour),
		testFilterNode("working-child", "running", "needle", now))
	finished := testFilterNode("finished", "succeeded", "finished", now)
	roots := finalizeTestRoots(mixed, finished)

	visible, filter, pagination := applyDashboardView(roots, viewParams{Query: "needle", Period: "24h", Page: 3}, now)
	if got := nodeIDs(visible); got != "mixed" || !mixed.HasUnfinished || pagination.Current != 1 {
		t.Fatalf("активное смешанное дерево скрыто: visible=%s mixed=%+v pagination=%+v", got, mixed, pagination)
	}
	if !filter.ActiveOnly || strings.Contains(string(filter.ActiveURL), "view=all") ||
		!strings.Contains(string(filter.AllURL), "q=needle") || !strings.Contains(string(filter.AllURL), "view=all") ||
		strings.Contains(string(filter.AllURL), "page=") {
		t.Fatalf("неверный статусный переключатель: %+v", filter)
	}

	all, allFilter, _ := applyDashboardView(roots, viewParams{Period: "all", Scope: "all", Page: 1}, now)
	if got := nodeIDs(all); got != "finished,mixed" || allFilter.ActiveOnly {
		t.Fatalf("режим «Все» потерял завершённые workflow: roots=%s filter=%+v", got, allFilter)
	}
}

// TestWorkingStateProjectsTree проверяет, что фильтр оставляет только реально
// выполняющиеся кубики и ветки к ним. Исходное дерево при этом не меняется: оно
// понадобится следующему запросу без фильтра состояний.
func TestWorkingStateProjectsTree(t *testing.T) {
	now := time.Now()
	workingChild := testFilterNode("working-child", "running", "working-child", now)
	finishedChild := testFilterNode("finished-child", "succeeded", "finished-child", now)
	root := testFilterNode("root", "failed", "root", now, workingChild, finishedChild)
	root.Steps = []stepNode{{ID: "working-cube", State: "running", Active: true}, {ID: "finished-cube", State: "succeeded"}}
	root.ActiveSteps = []stepNode{root.Steps[0]}
	finalizeTree(root)

	visible, filter, _ := applyDashboardView([]*runNode{root}, viewParams{Period: "all", Scope: "all", States: "working"}, now)
	if len(visible) != 1 || visible[0] == root || len(visible[0].Steps) != 1 || visible[0].Steps[0].ID != "working-cube" ||
		len(visible[0].Children) != 1 || visible[0].Children[0].ID != "working-child" || !filter.WorkingOnly {
		t.Fatalf("неверная проекция работающего дерева: visible=%+v filter=%+v", visible, filter)
	}
	if len(root.Steps) != 2 || len(root.Children) != 2 {
		t.Fatalf("фильтр изменил исходное дерево: steps=%d children=%d", len(root.Steps), len(root.Children))
	}
}

// TestFailedStateProjectsTree проверяет аварийную выборку: красные failed и
// cancelled остаются, успешные соседи скрываются, а папки-предки сохраняют связь.
func TestFailedStateProjectsTree(t *testing.T) {
	now := time.Now()
	brokenChild := testFilterNode("broken-child", "failed", "broken-child", now)
	workingChild := testFilterNode("working-child", "running", "working-child", now)
	root := testFilterNode("root", "running", "root", now, brokenChild, workingChild)
	root.Steps = []stepNode{{ID: "cancelled-cube", State: "cancelled"}, {ID: "finished-cube", State: "succeeded"}}
	finalizeTree(root)

	visible, filter, _ := applyDashboardView([]*runNode{root}, viewParams{Period: "all", Scope: "all", States: "failed", Page: 1}, now)
	if len(visible) != 1 || len(visible[0].Steps) != 1 || visible[0].Steps[0].ID != "cancelled-cube" ||
		len(visible[0].Children) != 1 || visible[0].Children[0].ID != "broken-child" || !filter.FailedOnly {
		t.Fatalf("неверная проекция сломавшегося дерева: visible=%+v filter=%+v", visible, filter)
	}
	if !strings.Contains(string(filter.FailedURL), "states=failed") {
		t.Fatalf("ссылка фильтра не сохраняет failed: %s", filter.FailedURL)
	}
}

// TestFocusedRootBuildsPath фиксирует файловую навигацию: закреплённый workflow
// становится единственным корнем, а интерфейс получает полный путь и родителя для
// перехода `../`. Все ссылки фильтров сохраняют выбранную папку.
func TestFocusedRootBuildsPath(t *testing.T) {
	now := time.Now()
	grandchild := testFilterNode("grandchild", "running", "grandchild", now)
	child := testFilterNode("child", "running", "child", now, grandchild)
	root := testFilterNode("root", "running", "root", now, child)
	finalizeTree(root)

	visible, filter, pagination := applyDashboardView([]*runNode{root}, viewParams{Period: "24h", Scope: "all", RootID: "grandchild", Page: 1}, now)
	if len(visible) != 1 || visible[0] != grandchild || !filter.Focused || filter.FocusParentID != "child" || len(filter.FocusPath) != 3 {
		t.Fatalf("закреплённая папка потеряла дерево или путь: visible=%+v filter=%+v", visible, filter)
	}
	if !strings.Contains(string(filter.WorkingURL), "root=grandchild") || !strings.Contains(string(filter.WorkingURL), "states=working") || pagination.Visible {
		t.Fatalf("фильтры не сохранили закреплённый root: filter=%+v pagination=%+v", filter, pagination)
	}
}

// TestTreeStateAndNewestChildOrder проверяет две агрегированные гарантии дерева:
// живой ребёнок делает всё дерево активным, а самый свежий ребёнок идёт первым.
func TestTreeStateAndNewestChildOrder(t *testing.T) {
	now := time.Now()
	old := testFilterNode("old", "succeeded", "old", now.Add(-time.Hour))
	recent := testFilterNode("recent", "running", "recent", now)
	root := testFilterNode("root", "succeeded", "root", now.Add(-2*time.Hour), old, recent)
	finalizeTree(root)
	sortNodes([]*runNode{root})
	if root.treeState != "running" || root.Children[0].ID != "recent" {
		t.Fatalf("агрегат или сортировка дерева неверны: state=%s children=%s", root.treeState, nodeIDs(root.Children))
	}
}

// TestSortStepsNewestFirst фиксирует порядок строк внутри раскрытого workflow:
// сначала самая свежая работа, а при одинаковом времени — выполняющийся шаг.
func TestSortStepsNewestFirst(t *testing.T) {
	now := time.Now()
	steps := []stepNode{
		{ID: "old-active", Active: true, updatedAt: now.Add(-time.Minute)},
		{ID: "finished", updatedAt: now},
		{ID: "current", Active: true, updatedAt: now},
	}
	sortSteps(steps)
	if got := steps[0].ID + "," + steps[1].ID + "," + steps[2].ID; got != "current,finished,old-active" {
		t.Fatalf("шаги отсортированы не по текущей активности: %s", got)
	}
}

func nodeIDs(nodes []*runNode) string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	return strings.Join(ids, ",")
}
