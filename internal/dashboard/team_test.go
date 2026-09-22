package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// HTTP наблюдает уже созданный CLI заказ, но не создаёт новый. Чат сохраняет
// прежние правила авторства и same-origin; POST создания недоступен даже своему UI.
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
	// Заказ создаётся тем же хранилищем, которое использует CLI, без HTTP.
	definition := workflow.Workflow{ID: "office-boss", Characters: workflow.DefaultTeamCharacters(), Steps: []workflow.Step{{ID: "boss", Type: "agent", Character: "boss", Prompt: "Работай", DependsOn: []string{}}}}
	data, _ := json.Marshal(definition)
	initial, err := runstore.Create(root, runstore.Input{Order: true, Team: true, WorkflowJSON: data, CWD: t.TempDir(), Task: "Сделать платформер"})
	if err != nil {
		t.Fatal(err)
	}
	created := struct{ RunID string }{initial.Meta.RunID}
	for _, origin := range []string{"https://foreign.test", "http://localhost"} {
		if w := request("POST", "/api/teams", `{"goal":"Новая цель"}`, origin); w.Code != 405 {
			t.Fatal("HTTP создал заказ", w.Code, w.Body.String())
		}
	}
	var w *httptest.ResponseRecorder
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

// Публичное чтение повторяет агентский контракт без запуска App Server.
// Некорректная позиция возвращает восстановление снимка, архив требует явного режима.
func TestTeamContextHTTP(t *testing.T) {
	root := t.TempDir()
	s, err := runstore.Create(root, runstore.Input{WorkflowJSON: []byte(`{"id":"context","steps":[{"id":"work","type":"agent","prompt":"Тест","dependsOn":[]}]}`), CWD: t.TempDir(), Task: "Цель"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three"} {
		if _, err := runstore.PostTeam(t.Context(), root, s.Meta.RunID, "", id, "Исходный текст"); err != nil {
			t.Fatal(err)
		}
	}
	h := Handler(root)
	get := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/teams/"+s.Meta.RunID+"/context"+query, nil))
		return w
	}
	first := get("?limit=1")
	var page runstore.TeamContext
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || first.Code != 200 || !page.HasMore || len(page.Messages) != 1 {
		t.Fatal(first.Code, first.Body.String(), err)
	}
	delta := get("?cursor=" + page.Cursor)
	if delta.Code != 200 || strings.Contains(delta.Body.String(), `"id":"one"`) {
		t.Fatal(delta.Body.String())
	}
	invalid := get("?cursor=broken")
	if !strings.Contains(invalid.Body.String(), `"resetRequired":true`) {
		t.Fatal(invalid.Body.String())
	}
	archive := get("?archive=true&id=one")
	if archive.Code != 200 || !strings.Contains(archive.Body.String(), "Исходный текст") {
		t.Fatal(archive.Body.String())
	}
	for _, query := range []string{"?from=1", "?archive=invalid", "?limit=101", "?through=no", "?archive=true&id=missing"} {
		if w := get(query); w.Code != 400 {
			t.Fatal(query, w.Code, w.Body.String())
		}
	}
}
