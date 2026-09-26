package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// reviewAPI хранит заменяемую границу запуска worker. Сам HTTP не владеет
// жизнью агента: после закрытия вкладки worker продолжает писать в общий каталог.
type reviewAPI struct {
	store *reviewstore.Store
	start func(string, bool) error
}

// registerReviews подключает самостоятельную feature к общему UI root.
func registerReviews(mux *http.ServeMux, root string) {
	a := reviewAPI{store: reviewstore.New(root), start: func(id string, retry bool) error { return startReviewWorker(root, id, retry) }}
	mux.HandleFunc("GET /code-review", serveUI)
	mux.HandleFunc("GET /api/reviews/defaults", a.defaults)
	mux.HandleFunc("GET /api/reviews/models", a.models)
	mux.HandleFunc("GET /api/reviews", a.list)
	mux.HandleFunc("POST /api/reviews", a.create)
	mux.HandleFunc("GET /api/reviews/{review}", a.detail)
	mux.HandleFunc("GET /api/reviews/{review}/events", a.events)
	mux.HandleFunc("GET /api/reviews/{review}/artifact", a.artifact)
	mux.HandleFunc("GET /api/reviews/{review}/view", a.view)
	mux.HandleFunc("POST /api/reviews/{review}/view", a.saveView)
	mux.HandleFunc("POST /api/reviews/{review}/stop", a.stop)
	mux.HandleFunc("POST /api/reviews/{review}/retry", a.retry)
}

// startReviewWorker запускает только текущий бинарник, без shell и команд из JSON.
// Setsid отделяет worker от сервера, Wait в goroutine не оставляет zombie.
func startReviewWorker(root, id string, retry bool) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"review-execute", id, "--root", root}
	if retry {
		args = append(args, "--retry")
	}
	command := exec.Command(executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	log, err := os.OpenFile(filepath.Join(root, id, "worker.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	command.Stdout, command.Stderr = log, log
	if err = command.Start(); err != nil {
		_ = log.Close()
		return err
	}
	go func() { _ = command.Wait(); _ = log.Close() }()
	return nil
}

// defaults отдаёт базовые модели; UI не должен угадывать актуальную конфигурацию.
func (a reviewAPI) defaults(w http.ResponseWriter, r *http.Request) {
	cwd, err := os.Getwd()
	if err != nil {
		reviewError(w, err)
		return
	}
	teamJSON(w, struct {
		Config reviewstore.Config
		CWD    string
	}{reviewstore.DefaultConfig(), cwd})
}

// models даёт конфигу запуска реальные допустимые model/effort текущего аккаунта.
func (a reviewAPI) models(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	cwd, err := os.Getwd()
	if err != nil {
		reviewError(w, err)
		return
	}
	models, err := codex.ListModels(ctx, codex.Connection{CWD: cwd})
	if err != nil {
		reviewError(w, err)
		return
	}
	teamJSON(w, models)
}

// list включает состояния просмотра отдельно, не меняя updatedAt сущностей.
func (a reviewAPI) list(w http.ResponseWriter, r *http.Request) {
	reviews, err := a.store.List()
	if err != nil {
		reviewError(w, err)
		return
	}
	views := map[string]reviewstore.ViewState{}
	for i, review := range reviews {
		refreshed, refreshErr := a.store.Refresh(review.ID)
		if refreshErr != nil {
			reviewError(w, refreshErr)
			return
		}
		reviews[i] = refreshed
		view, err := a.store.LoadView(review.ID)
		if err == nil {
			views[review.ID] = view
		}
	}
	teamJSON(w, struct {
		Reviews []reviewstore.Review
		Views   map[string]reviewstore.ViewState
	}{reviews, views})
}

// create валидирует источник и фиксирует review до запуска фонового worker.
func (a reviewAPI) create(w http.ResponseWriter, r *http.Request) {
	input := reviewstore.CreateOptions{Config: reviewstore.DefaultConfig()}
	if !teamInput(w, r, &input) {
		return
	}
	if input.CWD == "" {
		input.CWD, _ = os.Getwd()
	}
	review, err := a.store.Create(input)
	if err != nil {
		http.Error(w, diagnostic(err), 400)
		return
	}
	if err = a.start(review.ID, false); err != nil {
		_, _ = a.store.Update(review.ID, func(v *reviewstore.Review) error {
			v.State = reviewstore.Failed
			v.Error = "Не удалось запустить процесс: " + err.Error()
			return nil
		})
		reviewError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(review)
}

// detail возвращает только сохранённые факты; отсутствие данных не имитируется.
func (a reviewAPI) detail(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.Refresh(r.PathValue("review"))
	if err != nil {
		reviewError(w, err)
		return
	}
	teamJSON(w, v)
}

// events возвращает журнал, включая неизвестные расширения событий Codex.
func (a reviewAPI) events(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.Events(r.PathValue("review"))
	if err != nil {
		reviewError(w, err)
		return
	}
	teamJSON(w, v)
}

// artifact отдаёт только разрешённый store относительный путь. HTML/SVG никогда
// не исполняются как документы того же origin: активное содержимое отдаётся текстом.
func (a reviewAPI) artifact(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("path")
	data, err := a.store.ReadArtifact(r.PathValue("review"), name)
	if err != nil {
		reviewError(w, err)
		return
	}
	contentType := "text/plain; charset=utf-8"
	switch detected := http.DetectContentType(data); detected {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		contentType = detected
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// view читает пользовательский курсор независимо от состояния работы агента.
func (a reviewAPI) view(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.LoadView(r.PathValue("review"))
	if err != nil {
		reviewError(w, err)
		return
	}
	teamJSON(w, v)
}

// saveView принимает просмотренную версию, а не автоматически текущую серверную.
func (a reviewAPI) saveView(w http.ResponseWriter, r *http.Request) {
	var v reviewstore.ViewPatch
	if !teamInput(w, r, &v) {
		return
	}
	if err := a.store.PatchView(r.PathValue("review"), v); err != nil {
		reviewError(w, err)
		return
	}
	view, err := a.store.LoadView(r.PathValue("review"))
	if err != nil {
		reviewError(w, err)
		return
	}
	teamJSON(w, view)
}

// stop ставит устойчивый запрос отмены; worker прерывает принадлежащий ему turn.
func (a reviewAPI) stop(w http.ResponseWriter, r *http.Request) {
	var v struct{}
	if !teamInput(w, r, &v) {
		return
	}
	review, err := a.store.RequestStop(r.PathValue("review"))
	if err != nil {
		reviewError(w, err)
		return
	}
	teamJSON(w, review)
}

// retry не редактирует config: новый worker продолжит первый незавершённый этап.
func (a reviewAPI) retry(w http.ResponseWriter, r *http.Request) {
	var v struct{}
	if !teamInput(w, r, &v) {
		return
	}
	id := r.PathValue("review")
	review, err := a.store.Load(id)
	if err != nil {
		reviewError(w, err)
		return
	}
	if review.State == reviewstore.Succeeded {
		http.Error(w, "ревью уже завершено", 409)
		return
	}
	lock, err := a.store.AcquireExecution(id)
	if err != nil {
		http.Error(w, "ревью уже выполняется", 409)
		return
	}
	_, err = a.store.RecoverInterrupted(id)
	closeErr := lock.Close()
	if err != nil || closeErr != nil {
		reviewError(w, errors.Join(err, closeErr))
		return
	}
	if err = a.start(id, true); err != nil {
		reviewError(w, err)
		return
	}
	teamJSON(w, review)
}

// reviewError не превращает повреждение хранилища в пустое успешное состояние.
func reviewError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, os.ErrNotExist) {
		status = 404
	}
	http.Error(w, fmt.Sprintf("Code Review: %s", diagnostic(err)), status)
}
