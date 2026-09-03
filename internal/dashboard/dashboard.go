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
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/assets"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/series"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

const DefaultAddress = "127.0.0.1:60800"

const (
	processStopGrace = 2 * time.Second
	runStopTimeout   = 10 * time.Second
	runStopPoll      = 50 * time.Millisecond
)

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
	StopReason, StopVisit, StopLimit                   string
	EventsURL, VSCodeURL, UMLURL, DeleteURL            template.URL
	TicketURL                                          template.URL
	HasUML, Open, HasUnfinished, HasWorking, HasFailed bool
	AgentGraph                                         bool
	CompletedSteps, TotalSteps                         int
	Steps, ActiveSteps                                 []stepNode
	Children                                           []*runNode
	createdAt, updatedAt, activityAt                   time.Time
	baseSearch, searchText, treeState                  string
}

// stepNode описывает лист дерева и доступность его сохранённой памяти.
type stepNode struct {
	Key, ID, StepID, VisitID                                   string
	State, Tone, Runtime, Message, Action, Updated             string
	Trigger, Decision, Explanation, Transition, Skipped, Limit string
	TechnicalError, DecisionError                              string
	Visit, Iteration, Attempt                                  int
	EventsURL, MemoryURL, TraceURL                             template.URL
	HasMemory, Active                                          bool
	memoryID, turnID                                           string
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
	mux.HandleFunc("POST /api/runs/{run}/stop-and-delete", h.stopAndDelete)
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

// stopAndDelete принимает только same-origin запрос интерфейса. Проверка Origin
// не заменяет авторизацию для публичного сервера, но не позволяет посторонней
// веб-странице незаметно отправить destructive POST на loopback dashboard.
func (h handler) stopAndDelete(w http.ResponseWriter, r *http.Request) {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host != r.Host || origin.Scheme != "http" && origin.Scheme != "https" {
		http.Error(w, "запрос разрешён только из dashboard", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runStopTimeout)
	defer cancel()
	if err = stopAndRemoveRun(ctx, h.root, r.PathValue("run")); err != nil {
		status := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		} else if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, diagnostic(err), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"deleted":true}`))
}

// stopAndRemoveRun останавливает только process group, PID которой Lawa сама
// записала как активный App Server. Отрицательный PID адресует созданную Codex
// отдельную группу и не затрагивает координатор. ESRCH означает, что процесс уже
// исчез, поэтому повтор и удаление остаются идемпотентными по отношению к stop.
//
// Каждая итерация сначала пытается захватить lock и удалить run. Если координатор
// уже исчез, это не даёт устаревшему PID из журнала остановить постороннюю группу,
// которой ОС успела повторно выдать тот же номер. Только занятый lock подтверждает,
// что живой координатор ещё владеет run и записанный процесс можно останавливать.
// После сигнала список перечитывается: координатор мог запустить следующий кубик
// между попытками, а удаление допустимо только после освобождения lock.
func stopAndRemoveRun(ctx context.Context, root, runID string) error {
	terminated := make(map[int]time.Time)
	for {
		if err := runstore.Remove(root, runID); err == nil {
			return nil
		} else if !errors.Is(err, runstore.ErrRunLocked) {
			return fmt.Errorf("удалить run %q: %w", runID, err)
		}
		events, err := runstore.ReadEvents(root, runID)
		if err != nil {
			return fmt.Errorf("прочитать процессы run %q: %w", runID, err)
		}
		now := time.Now()
		for _, summary := range runstore.SummarizeEvents(events) {
			if summary.PID <= 1 {
				continue
			}
			if _, known := terminated[summary.PID]; !known {
				if err = signalProcessGroup(summary.PID, syscall.SIGTERM); err != nil {
					return err
				}
				terminated[summary.PID] = now
			}
		}
		for pid, started := range terminated {
			exists, checkErr := processGroupExists(pid)
			if checkErr != nil {
				return checkErr
			}
			if !exists {
				delete(terminated, pid)
				continue
			}
			if now.Sub(started) >= processStopGrace {
				if err = signalProcessGroup(pid, syscall.SIGKILL); err != nil {
					return err
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("остановить и удалить run %q: %w", runID, ctx.Err())
		case <-time.After(runStopPoll):
		}
	}
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("послать %s process group %d: %w", signal, pid, err)
	}
	return nil
}

func processGroupExists(pid int) (bool, error) {
	err := syscall.Kill(-pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, fmt.Errorf("проверить process group %d: %w", pid, err)
}

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

// trace отдаёт инкрементальный приватный поток только для известного шага или
// точного посещения. Сгенерированные ссылки v4 всегда используют visitId, чтобы
// повторные проходы одного шага не смешивали item lifecycle и live-вывод.
// Byte cursor относится ко всему events.jsonl: даже отфильтрованные lifecycle-
// события двигают позицию и больше не перечитываются следующим polling. JSON
// экранирует данные, а браузер вставляет Content только через textContent.
func (h handler) trace(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run")
	scope, err := parseEventScope(r.URL.Query(), true)
	if err != nil {
		http.Error(w, diagnostic(err), http.StatusBadRequest)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		offset, err = 0, nil
	}
	if err != nil || offset < 0 {
		http.Error(w, "неверная позиция журнала", http.StatusBadRequest)
		return
	}
	snapshot, err := runstore.LoadForDashboard(h.root, runID)
	if err != nil || !snapshotHasEventScope(snapshot, scope) {
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
		if !eventInScope(event, scope) || event.Content == "" && event.Kind != "error" {
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

// events отдаёт только нормализованный журнал Lawa. Опциональный step/visit
// фильтрует уже разобранные события и никогда не становится именем файла.
func (h handler) events(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run")
	scope, err := parseEventScope(r.URL.Query(), false)
	if err != nil {
		http.Error(w, diagnostic(err), http.StatusBadRequest)
		return
	}
	snapshot, err := runstore.LoadForDashboard(h.root, runID)
	if err != nil || !snapshotHasEventScope(snapshot, scope) {
		http.NotFound(w, r)
		return
	}
	events, err := runstore.ReadEvents(h.root, runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for _, event := range events {
		if !eventInScope(event, scope) {
			continue
		}
		_, _ = fmt.Fprintln(w, runstore.FormatEvent(event))
	}
}

// eventScope — уже разобранная логическая или точная область HTTP-журнала.
// field ограничен внутренними значениями step/visit и не используется как путь.
type eventScope struct{ field, value string }

// parseEventScope отличает отсутствующий query от явно пустого. Иначе опечатка
// `?visit=` неожиданно раскрыла бы журнал всего run вместо ожидаемого фильтра.
func parseEventScope(values url.Values, required bool) (eventScope, error) {
	step, hasStep := values["step"]
	visit, hasVisit := values["visit"]
	if hasStep && hasVisit {
		return eventScope{}, errors.New("step и visit взаимоисключающие")
	}
	if !hasStep && !hasVisit {
		if required {
			return eventScope{}, errors.New("нужен step или visit")
		}
		return eventScope{}, nil
	}
	field, candidates := "step", step
	if hasVisit {
		field, candidates = "visit", visit
	}
	if len(candidates) != 1 || strings.TrimSpace(candidates[0]) == "" {
		return eventScope{}, fmt.Errorf("%s должен быть одним непустым значением", field)
	}
	return eventScope{field: field, value: strings.TrimSpace(candidates[0])}, nil
}

// snapshotHasEventScope связывает query с проверенным snapshot до чтения данных.
// Step v2 ищется в неизменяемом workflow, поэтому допустим до первого посещения.
func snapshotHasEventScope(snapshot runstore.Snapshot, scope eventScope) bool {
	if scope.field == "" {
		return true
	}
	if scope.field == "visit" {
		if snapshot.Meta.Version != 4 {
			return false
		}
		for _, visit := range snapshot.Meta.Visits {
			if visit.VisitID == scope.value {
				return true
			}
		}
		return false
	}
	if snapshot.Meta.Version == 4 {
		for _, step := range snapshot.Workflow.Steps {
			if step.ID == scope.value {
				return true
			}
		}
		return false
	}
	for _, step := range snapshot.Meta.Steps {
		if step.ID == scope.value {
			return true
		}
	}
	return false
}

// eventInScope применяется только к разобранному RuntimeEvent и сравнивает ID
// как строки; query никогда не превращается в шаблон, регулярку или имя файла.
func eventInScope(event runstore.RuntimeEvent, scope eventScope) bool {
	switch scope.field {
	case "step":
		return event.StepID == scope.value
	case "visit":
		return event.VisitID == scope.value
	default:
		return true
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
	w.Header().Set("Cache-Control", "no-store")
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
		DeleteURL:  template.URL("/api/runs/" + runID + "/stop-and-delete"),
		AgentGraph: snapshot.Meta.Version == 4, StopReason: snapshot.Meta.StopReason,
		StopVisit:  snapshot.Meta.StopVisitID,
		TotalSteps: len(snapshot.Meta.Steps),
	}
	if node.AgentGraph {
		node.TotalSteps = len(snapshot.Meta.Visits)
		node.StopLimit = formatStopLimit(snapshot.Meta)
	}
	node.Tone = tone(node.State)
	search := []string{
		snapshot.Workflow.ID, runID, snapshot.Meta.ParentRunID, snapshot.Meta.CWD,
		snapshot.Task, node.State, node.StopReason, node.StopVisit, node.StopLimit,
		ticket.ID, ticket.Title, string(ticket.URL),
	}
	if snapshot.Workflow.Model != nil {
		search = append(search, *snapshot.Workflow.Model)
	}
	for _, definition := range snapshot.Workflow.Steps {
		search = append(search, definition.ID, definition.Type, definition.Prompt,
			strings.Join(definition.DependsOn, " "), strings.Join(definition.After, " "), formatVisitLimit(definition))
		for _, key := range sortedRouteKeys(definition.Decisions) {
			search = append(search, key, formatRouteDestination(definition.Decisions[key]))
		}
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
	// workflow.json создаётся один раз вместе с run и затем не перезаписывается.
	// Его ModTime даёт стабильный ключ порядка без чтения events.jsonl и без
	// миграции старых meta.json, в которых отдельной даты создания ещё нет.
	if info, err := os.Stat(filepath.Join(root, runID, "workflow.json")); err == nil {
		node.createdAt = info.ModTime()
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
	// Наличие артефактов определяется по метаданным файлов: обычный polling
	// сохраняет рабочие ссылки, но не читает содержимое UML и memory.
	if info, err := os.Lstat(filepath.Join(root, runID, runstore.StatusImageFilename)); err == nil && info.Mode().IsRegular() {
		// Версия из ModTime меняет HTML при обновлении PNG. Polling заменяет
		// карточку и браузер запрашивает новое изображение, не показывая старый кэш.
		node.HasUML = true
		node.UMLURL = template.URL("/uml/" + runID + "?v=" + strconv.FormatInt(info.ModTime().UnixNano(), 10))
	}
	for _, step := range snapshot.Meta.Steps {
		active := activeStepState(step.State)
		if step.State == scheduler.Succeeded {
			node.CompletedSteps++
		}
		search = append(search, step.ID, step.ThreadID, step.CodexThreadID, step.TurnID, string(step.State))
		memoryPath := filepath.Join(root, runID, "memory", step.ThreadID+".md")
		node.Steps = append(node.Steps, stepNode{
			Key: step.ID, ID: step.ID, StepID: step.ID, State: string(step.State), Tone: tone(string(step.State)),
			EventsURL: template.URL("/events/" + runID + "?step=" + url.QueryEscape(step.ID)),
			MemoryURL: template.URL("/memory/" + runID + "/" + step.ThreadID), HasMemory: nonEmptyRegularFile(memoryPath),
			TraceURL: template.URL("/api/trace/" + runID + "?step=" + url.QueryEscape(step.ID)),
			Active:   active, memoryID: step.ThreadID, turnID: step.TurnID,
		})
	}
	if node.AgentGraph {
		definitions := make(map[string]workflow.Step, len(snapshot.Workflow.Steps))
		for _, definition := range snapshot.Workflow.Steps {
			definitions[definition.ID] = definition
		}
		for _, visit := range snapshot.Meta.Visits {
			definition := definitions[visit.StepID]
			item := makeAgentStepNode(root, runID, visit, definition)
			node.Steps = append(node.Steps, item)
			if visit.State == scheduler.Succeeded || visit.State == scheduler.Failed {
				node.CompletedSteps++
			}
			search = append(search, item.Key, item.ID, item.StepID, item.VisitID, item.State,
				item.Trigger, item.Decision, item.Explanation, item.Transition, item.Skipped, item.Limit,
				item.TechnicalError, item.DecisionError, visit.CodexThreadID, visit.TurnID)
		}
	}
	for _, step := range node.Steps {
		if step.Active {
			node.ActiveSteps = append(node.ActiveSteps, step)
		}
	}
	node.baseSearch = strings.Join(search, "\n")
	return node
}

// makeAgentStepNode превращает один append-only visit в самостоятельный лист.
// Key равен VisitID и поэтому остаётся уникальным даже у повторов одного StepID;
// ID — короткая человекочитаемая подпись, не используемая как адрес хранилища.
func makeAgentStepNode(root, runID string, visit runstore.Visit, definition workflow.Step) stepNode {
	query := url.QueryEscape(visit.VisitID)
	memoryPath := filepath.Join(root, runID, "memory", visit.VisitID+".md")
	item := stepNode{
		Key: visit.VisitID, ID: fmt.Sprintf("%s#%d", visit.StepID, visit.Visit),
		StepID: visit.StepID, VisitID: visit.VisitID, Visit: visit.Visit,
		Iteration: visit.Iteration, Attempt: visit.Attempt,
		State: string(visit.State), Tone: tone(string(visit.State)), Active: activeStepState(visit.State),
		Trigger: formatVisitTrigger(visit.Trigger), Limit: formatVisitLimit(definition),
		TechnicalError: visit.TechnicalError,
		EventsURL:      template.URL("/events/" + runID + "?visit=" + query),
		TraceURL:       template.URL("/api/trace/" + runID + "?visit=" + query),
		MemoryURL:      template.URL("/memory/" + runID + "/" + visit.VisitID),
		HasMemory:      nonEmptyRegularFile(memoryPath), memoryID: visit.VisitID, turnID: visit.TurnID,
	}
	if visit.Decision != nil {
		item.Decision = fmt.Sprintf("%s · applied=%t", visit.Decision.Key, visit.Decision.Applied)
		item.Explanation = visit.Decision.Explanation
		item.Transition = formatDecisionDestination(*visit.Decision)
		item.Skipped = strings.Join(visit.Decision.Skipped, ", ")
		item.DecisionError = visit.Decision.Error
	}
	return item
}

// formatVisitTrigger сохраняет causal порядок sourceVisitIds из metadata.
func formatVisitTrigger(trigger runstore.VisitTrigger) string {
	result := string(trigger.Kind)
	if trigger.DecisionKey != "" {
		result += ":" + trigger.DecisionKey
	}
	if len(trigger.SourceVisitIDs) != 0 {
		result += " ← " + strings.Join(trigger.SourceVisitIDs, ", ")
	}
	return result
}

// formatDecisionDestination читает материализованный маршрут из metadata, а не
// из текущего workflow: это сохраняет точный исторический выбор после resume.
func formatDecisionDestination(decision runstore.DecisionRecord) string {
	if decision.Finish != nil {
		return "finish:" + string(*decision.Finish)
	}
	return strings.Join(decision.To, ", ")
}

// formatRouteDestination нужен только полнотекстовому индексу неизменяемого
// workflow и одинаково представляет ветвление к шагам и terminal outcome.
func formatRouteDestination(route workflow.Route) string {
	if route.Finish != nil {
		return "finish:" + string(*route.Finish)
	}
	return strings.Join(route.To, ", ")
}

// sortedRouteKeys устраняет случайный порядок Go map в поисковом индексе.
func sortedRouteKeys(routes map[string]workflow.Route) []string {
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// formatVisitLimit показывает эффективный исход: отсутствие onLimit означает
// failed по контракту v2, а не неизвестное значение.
func formatVisitLimit(step workflow.Step) string {
	if step.MaxVisits == nil {
		return ""
	}
	outcome := workflow.OutcomeFailed
	if step.OnLimit != nil {
		outcome = *step.OnLimit
	}
	return fmt.Sprintf("maxVisits=%d · onLimit=%s", *step.MaxVisits, outcome)
}

// formatStopLimit описывает не созданную активацию N+1, которая остановила run.
// Сохранённый trigger однозначно связывает её с последним причинным visit.
func formatStopLimit(meta runstore.Metadata) string {
	if meta.StopLimitStepID == "" || meta.StopLimitTrigger == nil {
		return ""
	}
	return fmt.Sprintf("%s · iteration=%d · %s", meta.StopLimitStepID,
		meta.StopLimitIteration, formatVisitTrigger(*meta.StopLimitTrigger))
}

// hydrateRunNodes загружает подробности всех деревьев только для явного
// полнотекстового поиска. Ошибка одного журнала не мешает остальным карточкам и
// возвращается отдельной диагностикой.
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
	for index := range node.Steps {
		step := &node.Steps[index]
		summary := summaries[step.Key]
		step.Message, step.Action = summary.Message, strings.Join(summary.ActiveItemTypes, ", ")
		step.Updated = ""
		if !summary.LastActivity.IsZero() {
			step.Updated = summary.LastActivity.Local().Format("15:04:05")
		}
		step.Runtime = formatStepRuntime(summary, step.turnID)
		node.baseSearch += "\n" + summary.Message + "\n" + step.Action
		if fullTextSearch && step.HasMemory {
			if memory, err := runstore.ReadMemory(root, node.ID, step.memoryID); err == nil {
				node.baseSearch += "\n" + string(memory)
			}
		}
	}
	node.ActiveSteps = node.ActiveSteps[:0]
	for _, step := range node.Steps {
		if step.Active {
			node.ActiveSteps = append(node.ActiveSteps, step)
		}
	}
	return eventErr
}

// nonEmptyRegularFile проверяет memory без чтения содержимого. Lstat не принимает
// симлинк за доступный файл: защищённый HTTP-маршрут также его отвергнет.
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

// workflowState для v4 использует только авторитетный RunState: отдельный Failed
// visit может быть штатным входом after-проверки и не означает провал workflow.
// Legacy сохраняет прежнюю свёртку состояний кубиков.
func workflowState(snapshot runstore.Snapshot) string {
	if snapshot.Meta.Version == 4 {
		return string(snapshot.Meta.RunState)
	}
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

// sortNodes ставит новые workflow сверху по времени создания неизменяемого
// workflow.json. Последующая активность не меняет порядок папок при polling.
// Равное или неизвестное время стабилизируется по ID.
func sortNodes(nodes []*runNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].createdAt.Equal(nodes[j].createdAt) {
			return nodes[i].ID < nodes[j].ID
		}
		return nodes[i].createdAt.After(nodes[j].createdAt)
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
		return stepNode{Key: id, ID: id, StepID: id, State: state, Tone: tone(state), Runtime: "pid 4201 · turn turn-preview · 18:42:10", Action: currentAction, EventsURL: action, MemoryURL: action, TraceURL: action, HasMemory: memory, Active: activeStepState(scheduler.State(state)), Updated: "18:42:10"}
	}
	run := func(id, name, state string, age time.Duration, steps ...stepNode) *runNode {
		search := []string{id, name, state}
		for _, item := range steps {
			search = append(search, item.ID, item.State, item.Action)
		}
		node := &runNode{
			ID: id, Name: name, State: state, Tone: tone(state), Updated: "2026-08-31 18:42:10",
			EventsURL: action, VSCodeURL: action, UMLURL: action, DeleteURL: action, HasUML: true, Steps: steps, TotalSteps: len(steps),
			createdAt: now.Add(-age), updatedAt: now.Add(-age), searchText: strings.Join(search, " "),
		}
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
