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

// Конфиг загружается до первого turn, сохраняется на заказ и не приглашает
// сотрудников заранее. Ошибки полей/ID не должны создавать частичный заказ.
func TestTeamConfigAPI(t *testing.T) {
	root := t.TempDir()
	h := Handler(root)
	create := func(characters any) *httptest.ResponseRecorder {
		input, _ := json.Marshal(map[string]any{"goal": "Создать дизайн", "cwd": t.TempDir(), "characters": characters})
		r := httptest.NewRequest("POST", "http://localhost/api/teams", strings.NewReader(string(input)))
		r.Header.Set("Origin", "http://localhost")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	character := map[string]string{"name": "Дизайнер", "history": "Опытный UX-исследователь", "instructions": "Проверяй удобство", "avatar": "pixel-designer"}
	w := create(map[string]any{"designer": character})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var created struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	chat, err := runstore.ReadTeam(root, created.RunID)
	if err != nil || len(chat.Room.Actors) != 1 || len(chat.Room.Catalog) != 2 || chat.Room.Catalog["designer"].Avatar != "pixel-designer" {
		t.Fatal(chat, err)
	}
	s, err := runstore.Load(root, created.RunID)
	if err != nil || s.Workflow.Characters["designer"].History != character["history"] {
		t.Fatal(s.Workflow, err)
	}
	for _, id := range []string{"human", "system", "../designer", "Designer", ""} {
		if w = create(map[string]any{id: character}); w.Code != 400 {
			t.Fatal(id, w.Code, w.Body.String())
		}
	}
	for _, field := range []string{"name", "history", "instructions"} {
		invalid := map[string]string{"name": "Дизайнер", "history": "Опыт", "instructions": "Работай"}
		delete(invalid, field)
		if w = create(map[string]any{"designer": invalid}); w.Code != 400 {
			t.Fatal(field, w.Code)
		}
	}
	if w = create(map[string]any{"designer": map[string]string{"name": "Дизайнер", "history": "Опыт", "instructions": "Работай", "role": "admin"}}); w.Code != 400 {
		t.Fatal("неизвестное поле", w.Code)
	}
	large := map[string]any{}
	for i := 0; i < 20; i++ {
		large["employee-"+strings.Repeat("a", i+1)] = character
	}
	if w = create(large); w.Code != 400 {
		t.Fatal("превышен лимит с добавлением Босса", w.Code)
	}
	// Явный {} означает одного Босса, а не неявное добавление Разработчика.
	if w = create(map[string]any{}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	chat, err = runstore.ReadTeam(root, created.RunID)
	if err != nil || len(chat.Room.Catalog) != 1 {
		t.Fatal(chat, err)
	}
}
