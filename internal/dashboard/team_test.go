package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// HTTP создаёт командный заказ с первым тегом Босса; Handler без Serve не запускает
// модель. Pin остаётся точным, автор назначается
// сервером. Same-origin и неизвестные поля проверяются до записи.
func TestTeamAPI(t *testing.T) {
	root := t.TempDir()
	h := Handler(root)
	request := func(method, path, body, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	input, _ := json.Marshal(map[string]string{"goal": "Сделать платформер", "cwd": t.TempDir()})
	if w := request("POST", "/api/teams", string(input), "https://foreign.test"); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := request("POST", "/api/teams", string(input), "http://localhost")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var created struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	s, err := runstore.Load(root, created.RunID)
	if err != nil || s.Meta.Order == nil || !s.Meta.Order.Team || s.Meta.Steps[0].State != scheduler.Pending || s.Meta.Steps[0].CodexThreadID != "" {
		t.Fatalf("заказ запущен или не создан: %+v %v", s.Meta, err)
	}
	path := "/api/teams/" + created.RunID
	// Чужой сайт не должен запускать даже read-only App Server.
	if w = request("POST", path+"/history/recover", `{}`, "https://foreign.test"); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request("POST", path+"/history/recover", `{"threadId":"foreign"}`, "http://localhost"); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request("POST", path+"/messages", `{"id":"one","text":"Готово","authorId":"boss"}`, "http://localhost"); w.Code != 400 {
		t.Fatal("подмена автора", w.Code)
	}
	for i := 0; i < 2; i++ {
		if w = request("POST", path+"/messages", `{"id":"one","text":"Начинаем с управления героем"}`, "http://localhost"); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	w = request("GET", path, "", "")
	var chat runstore.TeamChat
	if err = json.Unmarshal(w.Body.Bytes(), &chat); err != nil || chat.Goal != "Сделать платформер" || len(chat.Messages) != 2 || chat.Messages[0].AuthorID != "human" || chat.Messages[0].Date.IsZero() {
		t.Fatalf("история: %+v %v", chat, err)
	}
	if w = request("GET", "/api/teams", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), created.RunID) {
		t.Fatal(w.Body.String())
	}
}
