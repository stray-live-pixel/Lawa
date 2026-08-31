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
	Title, Refresh string
	Preview        bool
	Roots          []*runNode
	Problems       []problem
}

// problem — безопасная короткая диагностика одного каталога, не скрывающая дерево.
type problem struct{ Name, Message string }

// runNode — готовая к HTML структура одного workflow и его потомков. Все URL
// строит сервер из проверенных ID, поэтому template.URL не содержит сырого ввода.
type runNode struct {
	ID, ParentID, Name, State, Tone, Updated string
	CodexURL, VSCodeURL, UMLURL              template.URL
	HasUML, Open                             bool
	Steps                                    []stepNode
	Children                                 []*runNode
}

// stepNode описывает лист дерева и доступность его сохранённой памяти.
type stepNode struct {
	ID, State, Tone     string
	CodexURL, MemoryURL template.URL
	HasMemory           bool
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

// handler хранит единственную область чтения для всех HTTP-маршрутов.
type handler struct{ root string }

// live перечитывает хранилище на каждый polling-запрос.
func (h handler) live(w http.ResponseWriter, _ *http.Request) {
	view := page{Title: "Lawa workflows", Refresh: "3"}
	view.Roots, view.Problems = loadTree(h.root)
	render(w, view)
}

// preview использует тот же шаблон, но никогда не обращается к runstore.
func (h handler) preview(w http.ResponseWriter, _ *http.Request) {
	render(w, previewPage())
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
		snapshot, loadErr := runstore.Load(root, entry.Name())
		if loadErr != nil {
			problems = append(problems, problem{Name: entry.Name(), Message: diagnostic(loadErr)})
			continue
		}
		nodes[entry.Name()] = makeRunNode(root, snapshot)
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
	return roots, problems
}

// makeRunNode добавляет только существующие память и PNG; отсутствие артефакта
// превращается в неактивное действие, а не в ссылку на несуществующий файл.
func makeRunNode(root string, snapshot runstore.Snapshot) *runNode {
	runID := snapshot.Meta.RunID
	node := &runNode{
		ID: runID, ParentID: snapshot.Meta.ParentRunID, Name: snapshot.Workflow.ID,
		State: workflowState(snapshot), Open: true,
		CodexURL:  codexURL(snapshot.Meta.InitiatorThreadID),
		VSCodeURL: template.URL((&url.URL{Scheme: "vscode", Host: "file", Path: filepath.ToSlash(filepath.Join(root, runID))}).String()),
	}
	node.Tone = tone(node.State)
	if info, err := os.Stat(filepath.Join(root, runID, "meta.json")); err == nil {
		node.Updated = info.ModTime().Format("2006-01-02 15:04:05")
	}
	if _, err := runstore.ReadStatusImage(root, runID); err == nil {
		node.HasUML, node.UMLURL = true, template.URL("/uml/"+runID)
	}
	for _, step := range snapshot.Meta.Steps {
		memory, err := runstore.ReadMemory(root, runID, step.ThreadID)
		node.Steps = append(node.Steps, stepNode{
			ID: step.ID, State: string(step.State), Tone: tone(string(step.State)),
			CodexURL: codexURL(step.CodexThreadID), MemoryURL: template.URL("/memory/" + runID + "/" + step.ThreadID),
			HasMemory: err == nil && len(memory) > 0,
		})
	}
	return node
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

// codexURL кодирует thread ID как один сегмент deeplink Codex.
func codexURL(threadID string) template.URL {
	if strings.TrimSpace(threadID) == "" {
		return ""
	}
	return template.URL((&url.URL{Scheme: "codex", Host: "threads", Path: "/" + threadID}).String())
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
func previewPage() page {
	action := template.URL("#preview")
	step := func(id, state string, memory bool) stepNode {
		return stepNode{ID: id, State: state, Tone: tone(state), CodexURL: action, MemoryURL: action, HasMemory: memory}
	}
	run := func(id, name, state string, steps ...stepNode) *runNode {
		return &runNode{ID: id, Name: name, State: state, Tone: tone(state), Updated: "2026-08-31 18:42:10", CodexURL: action, VSCodeURL: action, UMLURL: action, HasUML: true, Open: true, Steps: steps}
	}
	release := run("preview-release", "release-v0.3.0", "running",
		step("prepare-release-notes-with-a-deliberately-long-name", "succeeded", true), step("build-all-platforms", "running", true))
	verification := run("preview-verify", "verify-related-artifacts", "failed",
		step("linux-amd64", "succeeded", true), step("macos-arm64", "failed", true), step("windows-amd64", "pending", false), step("not-selected-branch", "skipped", false))
	verification.Children = []*runNode{run("preview-repair", "repair-failed-macos-build", "running",
		step("diagnose", "succeeded", true), step("fix-and-rebuild", "waiting_for_approval", true), step("publish", "pending", false))}
	release.Children = []*runNode{verification, run("preview-docs", "publish-documentation", "succeeded", step("deploy", "succeeded", true))}
	maintenance := run("preview-maintenance", "nightly-maintenance", "pending",
		step("collect", "pending", false), step("cleanup-skipped-artifacts", "cancelled", false))
	maintenance.Open = false
	return page{Title: "Lawa workflows — preview", Preview: true, Roots: []*runNode{release, maintenance}}
}
