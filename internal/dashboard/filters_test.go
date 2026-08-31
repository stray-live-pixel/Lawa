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

	first, filter, pagination := applyDashboardView(roots, viewParams{Period: "1h", Page: 1}, now)
	if got := nodeIDs(first); got != "recent,boundary,active" {
		t.Fatalf("неверное текущее окно: %s", got)
	}
	if filter.Matched != 3 || pagination.Total != 3 || pagination.PreviousURL != "" || !strings.Contains(string(pagination.NextURL), "page=2") {
		t.Fatalf("неверная пагинация первого окна: filter=%+v pagination=%+v", filter, pagination)
	}

	second, _, pagination := applyDashboardView(roots, viewParams{Period: "1h", Page: 2}, now)
	if got := nodeIDs(second); got != "previous" || pagination.PreviousURL == "" || pagination.NextURL == "" {
		t.Fatalf("неверное предыдущее окно: roots=%s pagination=%+v", got, pagination)
	}
	third, _, _ := applyDashboardView(roots, viewParams{Period: "1h", Page: 3}, now)
	if got := nodeIDs(third); got != "old" {
		t.Fatalf("неверное самое старое окно: %s", got)
	}
	all, _, pagination := applyDashboardView(roots, viewParams{Period: "all", Page: 99}, now)
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
	visible, filter, pagination := applyDashboardView(roots, viewParams{Period: "1h", Page: 1}, now)
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
	visible, filter, pagination := applyDashboardView(roots, viewParams{Query: "thinktwice-592", Period: "1h", Page: 2}, now)
	if len(visible) != 1 || visible[0] != parent || filter.SearchMatched != 1 {
		t.Fatalf("поиск по ребёнку не сохранил дерево: visible=%v filter=%+v", visible, filter)
	}
	links := buildPagination(viewParams{Query: "a&b", Period: "1h", Page: 2}, 2, 3)
	if !strings.Contains(string(links.PreviousURL), "q=a%26b") || !strings.Contains(string(links.NextURL), "q=a%26b") || !pagination.Visible {
		t.Fatalf("фильтры потеряны при навигации: links=%+v searchPagination=%+v", links, pagination)
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
