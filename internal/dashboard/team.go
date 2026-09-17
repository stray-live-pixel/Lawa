package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/teamruntime"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
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
		CWD      string   `json:"cwd"`
		Problems []string `json:"problems"`
	}{Teams: []entry{}, Problems: []string{}}
	result.CWD, _ = os.Getwd()
	entries, err := os.ReadDir(h.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, diagnostic(err), 500)
		return
	}
	for _, item := range entries {
		if !item.IsDir() || item.Name() == "series" {
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

// createTeam сохраняет цель и первое обращение к Боссу. Фоновый Engine в Serve
// автоматически подхватывает заказ; HTTP не удерживает соединение на время turn.
// Цель не ограничивается длиной сообщения, CWD выбирается человеком.
func (h handler) createTeam(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Goal string `json:"goal"`
		CWD  string `json:"cwd"`
	}
	if !teamInput(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Goal) == "" || len(input.Goal) > 65536 || strings.TrimSpace(input.CWD) == "" {
		http.Error(w, "нужны цель (до 64 КБ) и рабочая папка", http.StatusBadRequest)
		return
	}
	definition := workflow.Workflow{ID: "office-boss", Characters: map[string]workflow.Character{"boss": workflow.Boss()}, Steps: []workflow.Step{{ID: "boss", Type: "agent", Character: "boss", Prompt: workflow.BossAssignment, DependsOn: []string{}}}}
	data, err := json.Marshal(definition)
	if err != nil {
		http.Error(w, diagnostic(err), 500)
		return
	}
	s, err := runstore.Create(h.root, runstore.Input{Order: true, Team: true, WorkflowJSON: data, Task: input.Goal, CWD: input.CWD})
	if err != nil {
		http.Error(w, diagnostic(err), http.StatusBadRequest)
		return
	}
	teamJSON(w, struct {
		RunID string `json:"runId"`
	}{s.Meta.RunID})
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
	}
	if !teamInput(w, r, &input) {
		return
	}
	message, err := runstore.PostTeam(r.Context(), h.root, r.PathValue("run"), "", input.ID, input.Text)
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
