// Package dashboard предоставляет локальное read-only представление сохранённых
// workflow. Сервер не разделяет память с координаторами: каждое обновление страницы
// перечитывает атомарные snapshot, поэтому видит run других процессов и переживает
// собственный перезапуск без отдельной базы данных.
package dashboard

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/assets"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/series"
)

const DefaultAddress = "127.0.0.1:60800"

//go:embed dashboard.html
var pageHTML string

var pageTemplate = template.Must(template.New("dashboard").Parse(pageHTML))

// page — единая модель live и preview; Refresh пуст только у статичного макета.
type page struct {
	Title, Refresh, EmptyMessage string
	Preview                      bool
	Filter                       filterView
	Pagination                   paginationView
	Roots                        []*runNode
	Scheduled                    []scheduledRun
	Problems                     []problem
}

// problem — безопасная короткая диагностика одного каталога, не скрывающая дерево.
type problem struct{ Name, Message string }

// scheduledRun — одна реально сохранённая точка следующего запуска серии.
// nextRunAt остаётся внутренним ключом сортировки, видимые строки строятся на
// сервере и не требуют повторять календарную семантику series в JavaScript.
type scheduledRun struct {
	SeriesID, WorkflowID, Next, Remaining, Schedule, Progress string
	Overdue                                                   bool
	nextRunAt                                                 time.Time
}

// runNode — готовая к HTML структура одного workflow и его потомков. Все URL
// строит сервер из проверенных ID, поэтому template.URL не содержит сырого ввода.
type runNode struct {
	ID, ParentID, Name, State, Tone, Updated           string
	TicketID, TicketTitle                              string
	EventsURL, VSCodeURL, UMLURL                       template.URL
	TicketURL                                          template.URL
	HasUML, Open, HasUnfinished, HasWorking, HasFailed bool
	CompletedSteps, TotalSteps                         int
	Steps, ActiveSteps                                 []stepNode
	Children                                           []*runNode
	updatedAt, activityAt                              time.Time
	baseSearch, searchText, treeState                  string
}

// stepNode описывает лист дерева и доступность его сохранённой памяти.
type stepNode struct {
	ID, State, Tone, Runtime, Message, Action, Updated string
	EventsURL, MemoryURL, TraceURL                     template.URL
	HasMemory, Active                                  bool
	updatedAt                                          time.Time
	threadID, turnID                                   string
}

// Handler возвращает полностью автономный HTTP-интерфейс для абсолютного root.
// Маршруты памяти и PNG сначала загружают runstore snapshot и используют только
// проверенные ID из него. Пользовательский URL поэтому не становится путём файла.
func Handler(root string) http.Handler {
	h := handler{root: root}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.live)
	mux.HandleFunc("GET /preview", h.preview)
	mux.HandleFunc("GET /assets/lawa-logo.png", h.logo)
	mux.HandleFunc("GET /memory/{run}/{thread}", h.memory)
	mux.HandleFunc("GET /events/{run}", h.events)
	mux.HandleFunc("GET /api/trace/{run}", h.trace)
	mux.HandleFunc("GET /uml/{run}", h.uml)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeMux канонизирует пути с `..` редиректом. Для read-only локального
		// файлового интерфейса безопаснее отклонить такой запрос до маршрутизации.
		if strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// logo отдаёт встроенный фирменный PNG. Короткий cache позволяет браузеру не
// перечитывать 864-КБ ресурс при polling, но не удерживает старый логотип надолго
// после обновления установленной версии Lawa.
func (h handler) logo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(assets.LawaLogoPNG)
}

// handler хранит единственную область чтения Lawa. Dashboard не обходит
// внутренние каталоги Codex и не раскрывает сырые rollout-файлы.
type handler struct{ root string }

// live перечитывает хранилище на каждый polling-запрос.
func (h handler) live(w http.ResponseWriter, r *http.Request) {
	view := page{Title: "Lawa", Refresh: "3"}
	now := time.Now()
	params := parseViewParams(r.URL.Query())
	roots, problems := loadTree(h.root)
	// Обычный polling сначала выбирает нужное временное окно по компактным
	// meta.json. Содержимое журналов и memory требуется только карточкам,
	// которые действительно попадут в HTML. Явный поиск остаётся полнотекстовым,
	// поэтому для него подробности нужны до фильтрации.
	if params.Query != "" {
		problems = append(problems, hydrateRunNodes(h.root, roots, true)...)
		for _, root := range roots {
			finalizeTree(root)
		}
	}
	scheduled, seriesProblems := loadScheduledRuns(h.root, now)
	visible, filter, pagination := applyDashboardView(roots, params, now)
	if params.Query == "" {
		problems = append(problems, hydrateRunNodes(h.root, visible, false)...)
	}
	view.Roots, view.Scheduled, view.Problems = visible, scheduled, append(problems, seriesProblems...)
	view.Filter, view.Pagination = filter, pagination
	if len(roots) == 0 {
		view.EmptyMessage = "Запусков пока нет. Создайте workflow командой lawa run."
	} else if filter.ActiveOnly && !filter.HasActiveQuery && len(visible) == 0 {
		view.EmptyMessage = "Активных workflow нет. Переключитесь на «Все», чтобы посмотреть завершённые."
	} else {
		view.EmptyMessage = "Ничего не найдено. Измените период или строку поиска."
	}
	render(w, view)
}

// loadScheduledRuns читает только состояние waiting с опубликованным nextRunAt.
// Running не имеет будущей точки, а stop-маркер запрещает новый run даже до того,
// как владелец серии успел переписать состояние в stopped. Просроченная точка не
// скрывается: она помогает заметить остановившийся или задержанный координатор.
func loadScheduledRuns(root string, now time.Time) ([]scheduledRun, []problem) {
	entries, err := os.ReadDir(filepath.Join(root, "series"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []problem{{Name: "Серии", Message: diagnostic(err)}}
	}
	var scheduled []scheduledRun
	var problems []problem
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		snapshot, loadErr := series.Load(root, entry.Name())
		if loadErr != nil {
			problems = append(problems, problem{Name: "Серия " + entry.Name(), Message: diagnostic(loadErr)})
			continue
		}
		if snapshot.HistoricalAppNative || snapshot.StopRequested || snapshot.State != series.Waiting || snapshot.NextRunAt == nil {
			continue
		}
		next := *snapshot.NextRunAt
		if next.IsZero() {
			problems = append(problems, problem{Name: "Серия " + entry.Name(), Message: "nextRunAt содержит нулевое время"})
			continue
		}
		workflowID := snapshot.WorkflowID
		if workflowID == "" {
			workflowID = "Серия " + snapshot.SeriesID
		}
		progress := fmt.Sprintf("запущено: %d", snapshot.RunsStarted)
		if snapshot.Config.MaxRuns > 0 {
			progress += fmt.Sprintf(" из %d", snapshot.Config.MaxRuns)
		}
		// Next — только готовая подпись для панели. Формат с днём в начале привычен
		// русскоязычному интерфейсу; календарные вычисления продолжают использовать
		// исходный time.Time в nextRunAt.
		scheduled = append(scheduled, scheduledRun{
			SeriesID: snapshot.SeriesID, WorkflowID: workflowID,
			Next: next.Local().Format("02.01.2006 15:04:05"), Remaining: remainingLabel(next, now),
			Schedule: scheduleLabel(snapshot.Config), Progress: progress,
			Overdue: !next.After(now), nextRunAt: next,
		})
	}
	sort.Slice(scheduled, func(i, j int) bool {
		if scheduled[i].nextRunAt.Equal(scheduled[j].nextRunAt) {
			return scheduled[i].SeriesID < scheduled[j].SeriesID
		}
		return scheduled[i].nextRunAt.Before(scheduled[j].nextRunAt)
	})
	return scheduled, problems
}

// remainingLabel даёт интерфейсу короткий обратный отсчёт без JavaScript-таймера.
// Контекст пилюли и панели уже объясняет, что это время до запуска, поэтому строка
// не повторяет «через». Округление вверх не показывает «0 мин», а polling
// обновляет подпись при переходе к следующей минуте.
func remainingLabel(next, now time.Time) string {
	if !next.After(now) {
		return "ожидает запуска"
	}
	remaining := next.Sub(now)
	if remaining < time.Minute {
		return "меньше минуты"
	}
	minutes := int((remaining + time.Minute - time.Nanosecond) / time.Minute)
	if minutes < 60 {
		return fmt.Sprintf("%d мин", minutes)
	}
	hours, restMinutes := minutes/60, minutes%60
	if hours < 24 {
		if restMinutes == 0 {
			return fmt.Sprintf("%d ч", hours)
		}
		return fmt.Sprintf("%d ч %d мин", hours, restMinutes)
	}
	days, restHours := hours/24, hours%24
	if restHours == 0 {
		return fmt.Sprintf("%d д", days)
	}
	return fmt.Sprintf("%d д %d ч", days, restHours)
}

// scheduleLabel переводит сохранённую конфигурацию в короткое описание. Это не
// вычисление расписания: источником истины для ближайшей точки остаётся NextRunAt.
func scheduleLabel(config series.Config) string {
	switch config.Mode {
	case series.Immediate:
		return "сразу после предыдущего запуска"
	case series.After:
		return "через " + config.Delay + " после завершения"
	case series.Cron:
		return "cron " + config.Cron + " · " + config.TimeZone
	default:
		return string(config.Mode)
	}
}

// preview использует тот же шаблон, но никогда не обращается к runstore.
func (h handler) preview(w http.ResponseWriter, r *http.Request) {
	render(w, previewPage(parseViewParams(r.URL.Query()), time.Now()))
}

// render полагается на html/template для экранирования всех видимых данных.
func render(w http.ResponseWriter, view page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if err := pageTemplate.ExecuteTemplate(w, "page", view); err != nil {
		http.Error(w, "не удалось построить страницу", http.StatusInternalServerError)
	}
}

type traceResponse struct {
	Events []traceEvent `json:"events"`
	Next   int64        `json:"next"`
}

type traceEvent struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind,omitempty"`
	ItemID   string    `json:"itemId,omitempty"`
	ItemType string    `json:"itemType,omitempty"`
	Content  string    `json:"content,omitempty"`
	Message  string    `json:"message,omitempty"`
	TurnID   string    `json:"turnId,omitempty"`
}

// trace отдаёт инкрементальный приватный поток только для известного шага.
// Byte cursor относится ко всему events.jsonl: даже отфильтрованные lifecycle-
// события двигают позицию и больше не перечитываются следующим polling. JSON
// экранирует данные, а браузер вставляет Content только через textContent.
func (h handler) trace(w http.ResponseWriter, r *http.Request) {
	runID, stepID := r.PathValue("run"), strings.TrimSpace(r.URL.Query().Get("step"))
	offset, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		offset, err = 0, nil
	}
	if err != nil || offset < 0 || stepID == "" {
		http.Error(w, "неверная позиция или шаг", http.StatusBadRequest)
		return
	}
	snapshot, err := runstore.LoadForDashboard(h.root, runID)
	if err != nil || !snapshotHasStep(snapshot, stepID) {
		http.NotFound(w, r)
		return
	}
	events, next, err := runstore.ReadEventsAfter(h.root, runID, offset)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	response := traceResponse{Next: next}
	for _, event := range events {
		if event.StepID != stepID || event.Content == "" && event.Kind != "error" {
			continue
		}
		response.Events = append(response.Events, traceEvent{
			Time: event.Time, Kind: event.Kind, ItemID: event.ItemID, ItemType: event.ItemType,
			Content: event.Content, Message: event.Message, TurnID: event.TurnID,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if err = json.NewEncoder(w).Encode(response); err != nil {
		return
	}
}

func snapshotHasStep(snapshot runstore.Snapshot, stepID string) bool {
	for _, step := range snapshot.Meta.Steps {
		if step.ID == stepID {
			return true
		}
	}
	return false
}

// memory отдаёт содержимое как text/plain. Даже память с тегами script остаётся
// текстом; перед чтением thread должен принадлежать указанному валидному snapshot.
func (h handler) memory(w http.ResponseWriter, r *http.Request) {
	runID, threadID := r.PathValue("run"), r.PathValue("thread")
	data, err := runstore.ReadMemory(h.root, runID, threadID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// events отдаёт только нормализованный журнал Lawa. Опциональный step query
// фильтрует уже разобранные события и никогда не становится именем файла.
func (h handler) events(w http.ResponseWriter, r *http.Request) {
	runID, stepID := r.PathValue("run"), strings.TrimSpace(r.URL.Query().Get("step"))
	events, err := runstore.ReadEvents(h.root, runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for _, event := range events {
		if stepID != "" && event.StepID != stepID {
			continue
		}
		_, _ = fmt.Fprintln(w, runstore.FormatEvent(event))
	}
}

func (h handler) uml(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run")
	data, err := runstore.ReadStatusImage(h.root, runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// loadTree изолирует повреждение одного каталога. Сначала все корректные run
// собираются в map, затем связи проверяются на потерянных родителей и циклы.
// Только после этого создаётся рекурсивная структура для безопасного шаблона.
func loadTree(root string) ([]*runNode, []problem) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []problem{{Name: "Хранилище", Message: diagnostic(err)}}
	}
	nodes := make(map[string]*runNode)
	var problems []problem
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "series" {
			continue
		}
		snapshot, loadErr := runstore.LoadForDashboard(root, entry.Name())
		if loadErr != nil {
			problems = append(problems, problem{Name: entry.Name(), Message: diagnostic(loadErr)})
			continue
		}
		node := makeRunNode(root, snapshot)
		nodes[entry.Name()] = node
	}
	for _, node := range nodes {
		if parentCycle(node, nodes) {
			problems = append(problems, problem{Name: node.ID, Message: "циклическая связь parentRunId; run показан корнем"})
			node.ParentID = ""
		}
	}
	var roots []*runNode
	for _, node := range nodes {
		if node.ParentID == "" {
			roots = append(roots, node)
			continue
		}
		parent := nodes[node.ParentID]
		if parent == nil {
			problems = append(problems, problem{Name: node.ID, Message: "родитель " + node.ParentID + " недоступен; run показан корнем"})
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	for _, root := range roots {
		finalizeTree(root)
	}
	sortNodes(roots)
	return roots, problems
}

// makeRunNode строит дешёвый индекс только из meta.json и метаданных файлов.
// Тяжёлые events.jsonl, memory и PNG читаются после фильтрации в hydrateRunNode.
func makeRunNode(root string, snapshot runstore.Snapshot) *runNode {
	runID := snapshot.Meta.RunID
	ticket := ticketFromTask(snapshot.Task)
	node := &runNode{
		ID: runID, ParentID: snapshot.Meta.ParentRunID, Name: snapshot.Workflow.ID,
		State: workflowState(snapshot), TicketID: ticket.ID, TicketTitle: ticket.Title, TicketURL: ticket.URL,
		EventsURL: template.URL("/events/" + runID), VSCodeURL: vscodeFileURL(filepath.Join(root, runID)),
		TotalSteps: len(snapshot.Meta.Steps),
	}
	node.Tone = tone(node.State)
	search := []string{
		snapshot.Workflow.ID, runID, snapshot.Meta.ParentRunID, snapshot.Meta.CWD,
		snapshot.Task, node.State,
		ticket.ID, ticket.Title, string(ticket.URL),
	}
	if snapshot.Workflow.Model != nil {
		search = append(search, *snapshot.Workflow.Model)
	}
	for _, definition := range snapshot.Workflow.Steps {
		search = append(search, definition.ID, definition.Type, definition.Prompt, strings.Join(definition.DependsOn, " "))
		if definition.Model != nil {
			search = append(search, *definition.Model)
		}
		if definition.Effort != nil {
			search = append(search, *definition.Effort)
		}
		if definition.Speed != nil {
			search = append(search, string(*definition.Speed))
		}
	}
	if info, err := os.Stat(filepath.Join(root, runID, "meta.json")); err == nil {
		node.updatedAt = info.ModTime()
		node.Updated = node.updatedAt.Format("2006-01-02 15:04:05")
	}
	// ModTime журнала достаточно для выбора временной страницы и не зависит от
	// его размера. Точное время последнего события уточняется при гидратации.
	if info, err := os.Stat(filepath.Join(root, runID, "events.jsonl")); err == nil && info.ModTime().After(node.updatedAt) {
		node.updatedAt = info.ModTime()
		node.Updated = node.updatedAt.Local().Format("2006-01-02 15:04:05")
	}
	for _, step := range snapshot.Meta.Steps {
		active := activeStepState(step.State)
		if step.State == scheduler.Succeeded {
			node.CompletedSteps++
		}
		search = append(search, step.ID, step.ThreadID, step.CodexThreadID, step.TurnID, string(step.State))
		node.Steps = append(node.Steps, stepNode{
			ID: step.ID, State: string(step.State), Tone: tone(string(step.State)),
			EventsURL: template.URL("/events/" + runID + "?step=" + url.QueryEscape(step.ID)),
			MemoryURL: template.URL("/memory/" + runID + "/" + step.ThreadID),
			TraceURL:  template.URL("/api/trace/" + runID + "?step=" + url.QueryEscape(step.ID)),
			Active:    active, threadID: step.ThreadID, turnID: step.TurnID,
		})
	}
	sortSteps(node.Steps)
	for _, step := range node.Steps {
		if step.Active {
			node.ActiveSteps = append(node.ActiveSteps, step)
		}
	}
	node.baseSearch = strings.Join(search, "\n")
	return node
}

// hydrateRunNodes загружает подробности только выбранных деревьев. Ошибка одного
// журнала не мешает остальным карточкам и возвращается отдельной диагностикой.
func hydrateRunNodes(root string, nodes []*runNode, fullTextSearch bool) []problem {
	var problems []problem
	for _, node := range nodes {
		if err := hydrateRunNode(root, node, fullTextSearch); err != nil {
			problems = append(problems, problem{Name: node.ID, Message: "журнал событий: " + diagnostic(err)})
		}
		problems = append(problems, hydrateRunNodes(root, node.Children, fullTextSearch)...)
	}
	return problems
}

func hydrateRunNode(root string, node *runNode, fullTextSearch bool) error {
	events, eventErr := runstore.ReadEvents(root, node.ID)
	summaries := runstore.SummarizeEvents(events)
	if len(events) != 0 && events[len(events)-1].Time.After(node.updatedAt) {
		node.updatedAt = events[len(events)-1].Time
		node.Updated = node.updatedAt.Local().Format("2006-01-02 15:04:05")
	}
	if regularFileExists(filepath.Join(root, node.ID, runstore.StatusImageFilename)) {
		node.HasUML, node.UMLURL = true, template.URL("/uml/"+node.ID)
	}
	for index := range node.Steps {
		step := &node.Steps[index]
		summary := summaries[step.ID]
		memoryPath := filepath.Join(root, node.ID, "memory", step.threadID+".md")
		step.HasMemory = nonEmptyRegularFile(memoryPath)
		step.Message, step.Action, step.updatedAt = summary.Message, strings.Join(summary.ActiveItemTypes, ", "), summary.LastActivity
		step.Updated = ""
		if !summary.LastActivity.IsZero() {
			step.Updated = summary.LastActivity.Local().Format("15:04:05")
		}
		step.Runtime = formatStepRuntime(summary, step.turnID)
		node.baseSearch += "\n" + summary.Message + "\n" + step.Action
		if fullTextSearch && step.HasMemory {
			if memory, err := runstore.ReadMemory(root, node.ID, step.threadID); err == nil {
				node.baseSearch += "\n" + string(memory)
			}
		}
	}
	sortSteps(node.Steps)
	node.ActiveSteps = node.ActiveSteps[:0]
	for _, step := range node.Steps {
		if step.Active {
			node.ActiveSteps = append(node.ActiveSteps, step)
		}
	}
	return eventErr
}

// regularFileExists проверяет артефакт без чтения его содержимого. Lstat не
// принимает симлинк за доступный файл: защищённый HTTP-маршрут также его отвергнет.
func regularFileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func nonEmptyRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// formatStepRuntime собирает компактную строку карточки из последнего состояния
// процесса и сохранённого turn. Она вызывается только для видимого кубика.
func formatStepRuntime(summary runstore.EventSummary, turnID string) string {
	runtime := ""
	if summary.PID != 0 {
		runtime = fmt.Sprintf("pid %d", summary.PID)
	} else if summary.ExitCode != nil {
		runtime = fmt.Sprintf("exit %d", *summary.ExitCode)
	} else if summary.Signal != "" {
		runtime = "signal " + summary.Signal
	}
	if turnID != "" {
		if runtime != "" {
			runtime += " · "
		}
		runtime += "turn " + turnID
	}
	if !summary.LastActivity.IsZero() {
		if runtime != "" {
			runtime += " · "
		}
		runtime += summary.LastActivity.Local().Format("15:04:05")
	}
	return runtime
}

func activeStepState(state scheduler.State) bool {
	return state == scheduler.Starting || state == scheduler.Running || state == scheduler.WaitingForApproval
}

// sortSteps ставит сверху шаг с самой свежей активностью. Если два события
// получили одну временную метку, выполняющийся шаг важнее завершённого: оператор
// сразу видит текущую работу, а окончательным стабильным ключом остаётся ID.
func sortSteps(steps []stepNode) {
	sort.SliceStable(steps, func(left, right int) bool {
		if !steps[left].updatedAt.Equal(steps[right].updatedAt) {
			return steps[left].updatedAt.After(steps[right].updatedAt)
		}
		if steps[left].Active != steps[right].Active {
			return steps[left].Active
		}
		return steps[left].ID < steps[right].ID
	})
}

// workflowState сворачивает состояния кубиков в три цветных итога и нейтральное
// ожидание. Ошибка приоритетнее работы, а успех требует успеха каждого кубика.
func workflowState(snapshot runstore.Snapshot) string {
	allSucceeded := len(snapshot.Meta.Steps) > 0
	active := false
	for _, step := range snapshot.Meta.Steps {
		switch step.State {
		case scheduler.Failed, scheduler.Cancelled, scheduler.Unknown:
			return "failed"
		case scheduler.Starting, scheduler.Running, scheduler.WaitingForApproval:
			allSucceeded, active = false, true
		default:
			allSucceeded = allSucceeded && step.State == scheduler.Succeeded
		}
	}
	if allSucceeded {
		return "succeeded"
	}
	if active {
		return "running"
	}
	return "pending"
}

// parentCycle идёт только по родителям и потому завершается за глубину дерева.
func parentCycle(start *runNode, nodes map[string]*runNode) bool {
	seen := map[string]bool{start.ID: true}
	for parentID := start.ParentID; parentID != ""; {
		if seen[parentID] {
			return true
		}
		seen[parentID] = true
		parent := nodes[parentID]
		if parent == nil {
			return false
		}
		parentID = parent.ParentID
	}
	return false
}

// sortNodes ставит самый недавно изменённый workflow сверху. activityAt уже
// включает потомков, поэтому новое дочернее действие поднимает всё связанное
// дерево. Равное или неизвестное время стабилизируется по ID между polling.
func sortNodes(nodes []*runNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].activityAt.Equal(nodes[j].activityAt) {
			return nodes[i].ID < nodes[j].ID
		}
		return nodes[i].activityAt.After(nodes[j].activityAt)
	})
	for _, node := range nodes {
		sortNodes(node.Children)
	}
}

// tone переводит техническое состояние в ограниченный CSS-словарь.
func tone(state string) string {
	switch state {
	case "running", "starting", "waiting_for_approval":
		return "running"
	case "failed", "cancelled", "unknown":
		return "failed"
	case "succeeded":
		return "succeeded"
	default:
		return "neutral"
	}
}

// vscodeFileURL строит ссылку только из локального пути, выбранного Lawa. url.URL
// экранирует пробелы и другие специальные символы; ручная конкатенация здесь
// могла бы получить иной host или оборвать путь на первом `#`.
func vscodeFileURL(path string) template.URL {
	return template.URL((&url.URL{Scheme: "vscode", Host: "file", Path: filepath.ToSlash(path)}).String())
}

// diagnostic убирает управляющие переносы и ограничивает размер ошибки одного run.
func diagnostic(err error) string {
	message := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || !utf8.ValidRune(r) {
			return ' '
		}
		return r
	}, err.Error())
	runes := []rune(message)
	if len(runes) > 300 {
		message = string(runes[:300]) + "…"
	}
	return message
}

// Serve запускает сервер на уже проверенном абсолютном root и завершает его по
// отмене контекста. Shutdown даёт активному короткому чтению закончиться, но не
// удерживает процесс дольше пяти секунд после Ctrl+C.
func Serve(ctx context.Context, root, address string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("слушать %s: %w", address, err)
	}
	return serve(ctx, listener, Handler(root))
}

// serve отделён от открытия TCP listener для детерминированного теста Shutdown.
func serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = server.Shutdown(shutdownCtx)
			cancel()
		case <-done:
		}
	}()
	err := server.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// IsLoopbackAddress проверяет только host части listen-адреса. Пустой host
// означает все интерфейсы и требует заметного предупреждения CLI.
func IsLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	return host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

// previewPage намеренно содержит все основные статусы, длинное имя, два корня и
// три уровня вложенности. Фейковые ссылки визуальны и не покидают страницу.
func previewPage(params viewParams, now time.Time) page {
	action := template.URL("#preview")
	step := func(id, state string, memory bool) stepNode {
		currentAction := ""
		if state == "running" {
			currentAction = "commandExecution"
		}
		return stepNode{ID: id, State: state, Tone: tone(state), Runtime: "pid 4201 · turn turn-preview · 18:42:10", Action: currentAction, EventsURL: action, MemoryURL: action, TraceURL: action, HasMemory: memory, Active: activeStepState(scheduler.State(state)), Updated: "18:42:10"}
	}
	run := func(id, name, state string, age time.Duration, steps ...stepNode) *runNode {
		search := []string{id, name, state}
		for _, item := range steps {
			search = append(search, item.ID, item.State, item.Action)
		}
		node := &runNode{
			ID: id, Name: name, State: state, Tone: tone(state), Updated: "2026-08-31 18:42:10",
			EventsURL: action, VSCodeURL: action, UMLURL: action, HasUML: true, Steps: steps, TotalSteps: len(steps),
			updatedAt: now.Add(-age), searchText: strings.Join(search, " "),
		}
		// Preview проходит тот же порядок, что и live-данные. Иначе макет мог бы
		// скрыть регрессию, при которой текущая работа уступила завершённой строке.
		sortSteps(node.Steps)
		for _, item := range node.Steps {
			if item.State == string(scheduler.Succeeded) {
				node.CompletedSteps++
			}
			if item.Active {
				node.ActiveSteps = append(node.ActiveSteps, item)
			}
		}
		return node
	}
	release := run("preview-release", "release-v0.3.0", "running", 15*time.Minute,
		step("prepare-release-notes-with-a-deliberately-long-name", "succeeded", true), step("build-all-platforms", "running", true))
	release.TicketID, release.TicketTitle, release.TicketURL = "THINKTWICE-592", "[СП] Проблемы с модалкой на уровнях", action
	release.searchText += " THINKTWICE-592 [СП] Проблемы с модалкой на уровнях prepare-release-notes-with-a-deliberately-long-name build-all-platforms"
	verification := run("preview-verify", "verify-related-artifacts", "failed", 2*time.Hour,
		step("linux-amd64", "succeeded", true), step("macos-arm64", "failed", true), step("windows-amd64", "pending", false), step("not-selected-branch", "skipped", false))
	verification.Children = []*runNode{run("preview-repair", "repair-failed-macos-build", "running", 30*time.Minute,
		step("diagnose", "succeeded", true), step("fix-and-rebuild", "waiting_for_approval", true), step("publish", "pending", false))}
	release.Children = []*runNode{verification, run("preview-docs", "publish-documentation", "succeeded", 3*time.Hour, step("deploy", "succeeded", true))}
	maintenance := run("preview-maintenance", "nightly-maintenance", "pending", 26*time.Hour,
		step("collect", "pending", false), step("cleanup-skipped-artifacts", "cancelled", false))
	maintenance.Open = false
	failed := run("preview-failed", "failed-nightly-cleanup", "failed", 4*time.Hour, step("cleanup", "failed", true))
	succeeded := run("preview-succeeded", "previous-release", "succeeded", 48*time.Hour, step("publish", "succeeded", true))
	roots := []*runNode{release, maintenance, failed, succeeded}
	for _, root := range roots {
		finalizeTree(root)
	}
	visible, filter, pagination := applyDashboardView(roots, params, now)
	return page{
		Title: "Lawa", Preview: true, Roots: visible, Filter: filter, Pagination: pagination,
		Scheduled: []scheduledRun{
			{
				SeriesID: "preview-series-nightly", WorkflowID: "nightly-review",
				Next: now.Add(35 * time.Minute).Format("02.01.2006 15:04:05"), Remaining: "35 мин",
				Schedule: "cron 0 3 * * * · Europe/Moscow", Progress: "запущено: 4 из 30",
			},
			{
				SeriesID: "preview-series-sync", WorkflowID: "sync-project-status",
				Next: now.Add(2*time.Hour + 10*time.Minute).Format("02.01.2006 15:04:05"), Remaining: "2 ч 10 мин",
				Schedule: "через 2h после завершения", Progress: "запущено: 8",
			},
			{
				SeriesID: "preview-series-report", WorkflowID: "weekly-report",
				Next: now.Add(29 * time.Hour).Format("02.01.2006 15:04:05"), Remaining: "1 д 5 ч",
				Schedule: "cron 0 19 * * 5 · Europe/Moscow", Progress: "запущено: 12 из 52",
			},
		},
		EmptyMessage: "Ничего не найдено. Измените период или строку поиска.",
	}
}
