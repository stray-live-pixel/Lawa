package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/teamruntime"
)

// teamInput принимает ограниченный JSON только от своего UI. Сервер остаётся
// локальным: Origin защищает от сторонней страницы, но не заменяет авторизацию.
func teamInput(w http.ResponseWriter, r *http.Request, value any) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host != r.Host || (origin.Scheme != "http" && origin.Scheme != "https") {
		http.Error(w, "запрос разрешён только из интерфейса Lawa", http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		http.Error(w, "неверный JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "ожидался один JSON-объект", http.StatusBadRequest)
		return false
	}
	return true
}

// teamJSON запрещает кэширование личной командной истории.
func teamJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(value)
}

// teams возвращает только корни: дочерние workflow принадлежат тому же чату.
// Повреждённый заказ не скрывает остальные, но его ошибка остаётся видимой.
func (h handler) teams(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		ID   string `json:"id"`
		Goal string `json:"goal"`
	}
	result := struct {
		Teams    []entry  `json:"teams"`
		Problems []string `json:"problems"`
	}{Teams: []entry{}, Problems: []string{}}
	entries, err := os.ReadDir(h.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, diagnostic(err), 500)
		return
	}
	for _, item := range entries {
		if !item.IsDir() || item.Name() == "series" || reviewstore.IsReview(h.root, item.Name()) {
			continue
		}
		s, err := runstore.LoadForDashboard(h.root, item.Name())
		if err != nil {
			result.Problems = append(result.Problems, item.Name()+": "+diagnostic(err))
			continue
		}
		if s.Meta.ParentRunID != "" {
			continue
		}
		chat, err := runstore.ReadTeam(h.root, s.Meta.RunID)
		if err != nil {
			result.Problems = append(result.Problems, item.Name()+": "+diagnostic(err))
			continue
		}
		result.Teams = append(result.Teams, entry{ID: chat.RunID, Goal: chat.Goal})
	}
	teamJSON(w, result)
}

// retryTeam снимает только доказанную локальную ошибку до отправки в Codex.
func (h handler) retryTeam(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if !teamInput(w, r, &input) {
		return
	}
	if err := teamruntime.RetryUnsent(r.Context(), h.root, r.PathValue("run"), r.PathValue("actor")); err != nil {
		http.Error(w, diagnostic(err), http.StatusConflict)
		return
	}
	teamJSON(w, map[string]bool{"retried": true})
}

// team читает в том числе по ID ребёнка: UI всегда получает ID общего корня.
func (h handler) team(w http.ResponseWriter, r *http.Request) {
	chat, err := runstore.ReadTeam(h.root, r.PathValue("run"))
	if err != nil {
		http.Error(w, diagnostic(err), http.StatusNotFound)
		return
	}
	teamJSON(w, chat)
}

// postTeam отправляет реплику только от Чела. Выбирать другого автора из UI
// нельзя: агент публикует от своего имени через привязанный dynamic tool.
func (h handler) postTeam(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID   string `json:"id"`
		Text string `json:"text"`
		runstore.TeamMessageLinks
	}
	if !teamInput(w, r, &input) {
		return
	}
	var message runstore.TeamMessage
	var err error
	if input.TaskID != "" || input.ReplyTo != "" {
		message, err = runstore.PostLinkedActor(r.Context(), h.root, r.PathValue("run"), "human", input.ID, input.Text, input.TeamMessageLinks)
	} else {
		message, err = runstore.PostTeam(r.Context(), h.root, r.PathValue("run"), "", input.ID, input.Text)
	}
	if err != nil {
		http.Error(w, diagnostic(err), http.StatusBadRequest)
		return
	}
	teamJSON(w, message)
}

// recoverTeamHistory восстанавливает только наблюдаемую историю через read-only
// App Server. Это явное действие пользователя, а не запуск модели при GET.
func (h handler) recoverTeamHistory(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if !teamInput(w, r, &input) {
		return
	}
	s, err := runstore.TeamRoot(h.root, r.PathValue("run"))
	if err != nil {
		http.Error(w, diagnostic(err), 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	observer, err := codex.OpenObserver(ctx, codex.Connection{CWD: s.Meta.CWD})
	if err != nil {
		http.Error(w, diagnostic(err), 502)
		return
	}
	defer observer.Close()
	if err = teamruntime.RecoverHistory(ctx, h.root, s.Meta.RunID, observer.ReadTurns); err != nil {
		http.Error(w, diagnostic(err), 409)
		return
	}
	chat, err := runstore.ReadTeam(h.root, s.Meta.RunID)
	if err != nil {
		http.Error(w, diagnostic(err), 500)
		return
	}
	teamJSON(w, chat)
}

// teamContext предоставляет тот же ограниченный контракт оператору и QA,
// не запускает модель и не меняет адресную доставку. Полный UI-журнал — h.team.
func (h handler) teamContext(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	options := runstore.TeamReadOptions{Cursor: q.Get("cursor"), IDs: q["id"]}
	if value := q.Get("archive"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			http.Error(w, "неверный archive", 400)
			return
		}
		options.Archive = parsed
	}
	for key, target := range map[string]*int{"from": &options.From, "through": &options.Through, "limit": &options.Limit} {
		if value := q.Get(key); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				http.Error(w, "неверный "+key, 400)
				return
			}
			*target = parsed
		}
	}
	result, err := runstore.ReadTeamContext(h.root, r.PathValue("run"), options)
	if err != nil {
		http.Error(w, diagnostic(err), 400)
		return
	}
	teamJSON(w, result)
}

// tasks предоставляет read-only карточки и архивные обсуждения без запуска моделей.
func (h handler) tasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if q.Get("limit") == "" {
		limit = 20
		err = nil
	}
	if err != nil {
		http.Error(w, "неверный limit", 400)
		return
	}
	offset, err := strconv.Atoi(q.Get("offset"))
	if q.Get("offset") == "" {
		offset = 0
		err = nil
	}
	if err != nil {
		http.Error(w, "неверный offset", 400)
		return
	}
	out, err := runstore.ReadTasks(h.root, r.PathValue("run"), runstore.TaskReadOptions{TaskID: q.Get("taskId"), Section: q.Get("section"), After: q.Get("after"), Offset: offset, Limit: limit})
	if err != nil {
		http.Error(w, diagnostic(err), 400)
		return
	}
	teamJSON(w, out)
}

// changeTask — необязательное явное управление от человека. Автор не принимается
// из тела; произвольный статус и обход приёмки этим endpoint не поддерживаются.
func (h handler) changeTask(w http.ResponseWriter, r *http.Request) {
	var in runstore.TaskCommand
	if !teamInput(w, r, &in) {
		return
	}
	out, err := runstore.ApplyTaskCommand(r.Context(), h.root, r.PathValue("run"), "human", in)
	if err != nil {
		http.Error(w, diagnostic(err), http.StatusConflict)
		return
	}
	teamJSON(w, out)
}
