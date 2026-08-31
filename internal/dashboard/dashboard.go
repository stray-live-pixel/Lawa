// Package dashboard предоставляет локальное read-only представление сохранённых
// workflow. Сервер не разделяет память с координаторами: каждое обновление страницы
// перечитывает атомарные snapshot, поэтому видит run других процессов и переживает
// собственный перезапуск без отдельной базы данных.
package dashboard

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
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
	Groups                       []runGroup
	Problems                     []problem
}

// problem — безопасная короткая диагностика одного каталога, не скрывающая дерево.
type problem struct{ Name, Message string }

// runGroup — сворачиваемая секция корневых workflow с одинаковым итоговым
// состоянием. Дочерние workflow остаются рядом с родителем: перенос каждого
// узла в собственную секцию разрушил бы сохранённое дерево связанных запусков.
type runGroup struct {
	ID, Title, Tone string
	Open            bool
	Roots           []*runNode
}

// runNode — готовая к HTML структура одного workflow и его потомков. Все URL
// строит сервер из проверенных ID, поэтому template.URL не содержит сырого ввода.
type runNode struct {
	ID, ParentID, Name, State, Tone, Updated string
	TicketID, TicketTitle                    string
	EventsURL, VSCodeURL, UMLURL             template.URL
	TicketURL                                template.URL
	HasUML, Open                             bool
	Steps                                    []stepNode
	Children                                 []*runNode
	updatedAt, activityAt                    time.Time
	searchText, treeState                    string
}

// stepNode описывает лист дерева и доступность его сохранённой памяти.
type stepNode struct {
	ID, State, Tone, Runtime, Message, Action string
	EventsURL, MemoryURL                      template.URL
	HasMemory                                 bool
}

// Handler возвращает полностью автономный HTTP-интерфейс для абсолютного root.
// Маршруты памяти и PNG сначала загружают runstore snapshot и используют только
// проверенные ID из него. Пользовательский URL поэтому не становится путём файла.
func Handler(root string) http.Handler {
	h := handler{root: root}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.live)
	mux.HandleFunc("GET /preview", h.preview)
	mux.HandleFunc("GET /memory/{run}/{thread}", h.memory)
	mux.HandleFunc("GET /events/{run}", h.events)
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

// handler хранит единственную область чтения Lawa. Dashboard не обходит
// внутренние каталоги Codex и не раскрывает сырые rollout-файлы.
type handler struct{ root string }

// live перечитывает хранилище на каждый polling-запрос.
func (h handler) live(w http.ResponseWriter, r *http.Request) {
	view := page{Title: "Lawa workflows", Refresh: "3"}
	roots, problems := loadTree(h.root)
	visible, filter, pagination := applyDashboardView(roots, parseViewParams(r.URL.Query()), time.Now())
	view.Groups, view.Problems, view.Filter, view.Pagination = groupRuns(visible), problems, filter, pagination
	if len(roots) == 0 {
		view.EmptyMessage = "Запусков пока нет. Создайте workflow командой lawa run."
	} else {
		view.EmptyMessage = "Ничего не найдено. Измените период или строку поиска."
	}
	render(w, view)
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
	if err := pageTemplate.ExecuteTemplate(w, "page", view); err != nil {
		http.Error(w, "не удалось построить страницу", http.StatusInternalServerError)
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
		node, eventErr := makeRunNode(root, snapshot)
		nodes[entry.Name()] = node
		if eventErr != nil {
			problems = append(problems, problem{Name: entry.Name(), Message: "журнал событий: " + diagnostic(eventErr)})
		}
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
	sortNodes(roots)
	for _, root := range roots {
		finalizeTree(root)
	}
	return roots, problems
}

// makeRunNode добавляет только существующие память и PNG; отсутствие артефакта
// превращается в неактивное действие, а не в ссылку на несуществующий файл.
func makeRunNode(root string, snapshot runstore.Snapshot) (*runNode, error) {
	runID := snapshot.Meta.RunID
	ticket := ticketFromTask(snapshot.Task)
	events, eventErr := runstore.ReadEvents(root, runID)
	summaries := runstore.SummarizeEvents(events)
	node := &runNode{
		ID: runID, ParentID: snapshot.Meta.ParentRunID, Name: snapshot.Workflow.ID,
		State: workflowState(snapshot), Open: true, TicketID: ticket.ID, TicketTitle: ticket.Title, TicketURL: ticket.URL,
		EventsURL: template.URL("/events/" + runID), VSCodeURL: vscodeFileURL(filepath.Join(root, runID)),
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
	if len(events) != 0 && events[len(events)-1].Time.After(node.updatedAt) {
		node.updatedAt = events[len(events)-1].Time
		node.Updated = node.updatedAt.Local().Format("2006-01-02 15:04:05")
	}
	if _, err := runstore.ReadStatusImage(root, runID); err == nil {
		node.HasUML, node.UMLURL = true, template.URL("/uml/"+runID)
	}
	for _, step := range snapshot.Meta.Steps {
		memory, err := runstore.ReadMemory(root, runID, step.ThreadID)
		summary := summaries[step.ID]
		runtime := ""
		if summary.PID != 0 {
			runtime = fmt.Sprintf("pid %d", summary.PID)
		} else if summary.ExitCode != nil {
			runtime = fmt.Sprintf("exit %d", *summary.ExitCode)
		} else if summary.Signal != "" {
			runtime = "signal " + summary.Signal
		}
		if step.TurnID != "" {
			if runtime != "" {
				runtime += " · "
			}
			runtime += "turn " + step.TurnID
		}
		if !summary.LastActivity.IsZero() {
			if runtime != "" {
				runtime += " · "
			}
			runtime += summary.LastActivity.Local().Format("15:04:05")
		}
		action := strings.Join(summary.ActiveItemTypes, ", ")
		search = append(search, step.ID, step.ThreadID, step.CodexThreadID, step.TurnID, string(step.State), string(memory), summary.Message, action)
		node.Steps = append(node.Steps, stepNode{
			ID: step.ID, State: string(step.State), Tone: tone(string(step.State)), Runtime: runtime, Message: summary.Message, Action: action,
			EventsURL: template.URL("/events/" + runID + "?step=" + url.QueryEscape(step.ID)),
			MemoryURL: template.URL("/memory/" + runID + "/" + step.ThreadID), HasMemory: err == nil && len(memory) > 0,
		})
	}
	node.searchText = strings.Join(search, "\n")
	return node, eventErr
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

// sortNodes даёт стабильный порядок независимо от обхода Go map и polling.
func sortNodes(nodes []*runNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Name == nodes[j].Name {
			return nodes[i].ID < nodes[j].ID
		}
		return nodes[i].Name < nodes[j].Name
	})
	for _, node := range nodes {
		sortNodes(node.Children)
	}
}

// groupRuns сохраняет порядок корней после фильтрации и пагинации.
// Pending относится к работе: workflow ещё не завершён, даже если ни один шаг
// пока не стартовал. treeState учитывает дочерние workflow, поэтому завершившийся
// диспетчер с работающим ребёнком остаётся в секции «В работе».
func groupRuns(roots []*runNode) []runGroup {
	groups := []runGroup{
		{ID: "active", Title: "В работе", Tone: "running", Open: true},
		{ID: "failed", Title: "Сломавшиеся", Tone: "failed"},
		{ID: "succeeded", Title: "Успешные", Tone: "succeeded"},
	}
	for _, root := range roots {
		group := 0
		state := root.treeState
		if state == "" {
			state = normalizeTreeState(root.State)
		}
		switch state {
		case "failed":
			group = 1
		case "succeeded":
			group = 2
		}
		groups[group].Roots = append(groups[group].Roots, root)
	}
	visible := make([]runGroup, 0, len(groups))
	for _, group := range groups {
		if len(group.Roots) > 0 {
			visible = append(visible, group)
		}
	}
	return visible
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
		return stepNode{ID: id, State: state, Tone: tone(state), Runtime: "pid 4201 · turn turn-preview · 18:42:10", Action: currentAction, EventsURL: action, MemoryURL: action, HasMemory: memory}
	}
	run := func(id, name, state string, age time.Duration, steps ...stepNode) *runNode {
		search := []string{id, name, state}
		for _, item := range steps {
			search = append(search, item.ID, item.State, item.Action)
		}
		return &runNode{
			ID: id, Name: name, State: state, Tone: tone(state), Updated: "2026-08-31 18:42:10",
			EventsURL: action, VSCodeURL: action, UMLURL: action, HasUML: true, Open: true, Steps: steps,
			updatedAt: now.Add(-age), searchText: strings.Join(search, " "),
		}
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
		Title: "Lawa workflows — preview", Preview: true, Groups: groupRuns(visible), Filter: filter, Pagination: pagination,
		EmptyMessage: "Ничего не найдено. Измените период или строку поиска.",
	}
}
