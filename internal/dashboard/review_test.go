package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// TestReviewHTTP проверяет реальный контракт создания и чтения без платного агента.
func TestReviewHTTP(t *testing.T) {
	store := reviewstore.New(t.TempDir())
	var started string
	a := reviewAPI{store: store, start: func(id string, _ bool) error { started = id; return nil }}
	data, _ := json.Marshal(reviewstore.CreateOptions{CWD: t.TempDir(), Prompt: "Проверь локальные изменения", Config: reviewstore.DefaultConfig()})
	r := httptest.NewRequest("POST", "http://localhost/api/reviews", bytes.NewReader(data))
	r.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	a.create(w, r)
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var review reviewstore.Review
	if err := json.Unmarshal(w.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if review.ID == "" || started != review.ID {
		t.Fatal("worker не получил сохранённый ID")
	}
	if err := store.SaveArtifact(review.ID, "artifacts/example.svg", []byte(`<svg onload="alert(1)"/>`)); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "http://localhost/api/reviews/"+review.ID+"/artifact?path=artifacts/example.svg", nil)
	r.SetPathValue("review", review.ID)
	w = httptest.NewRecorder()
	a.artifact(w, r)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("активный asset не изолирован: %v", w.Result().Header)
	}
	// Прямой путь за пределы review не раскрывает файлы проекта.
	r = httptest.NewRequest("GET", "http://localhost/artifact?path=../../secret", nil)
	r.SetPathValue("review", review.ID)
	w = httptest.NewRecorder()
	a.artifact(w, r)
	if w.Code == 200 {
		t.Fatal("разрешён traversal")
	}
	// Новый тип каталога не попадает в ошибки старого workflow dashboard.
	_, problems := loadTree(store.Root)
	if len(problems) != 0 {
		t.Fatalf("review принят за повреждённый workflow: %v", problems)
	}
}

// TestReviewWritesRequireOrigin защищает локальный запуск от чужой веб-страницы.
func TestReviewWritesRequireOrigin(t *testing.T) {
	a := reviewAPI{store: reviewstore.New(t.TempDir()), start: func(string, bool) error { t.Fatal("запуск без origin"); return nil }}
	for _, origin := range []string{"", "https://evil.example"} {
		r := httptest.NewRequest("POST", "http://localhost/api/reviews", strings.NewReader(`{}`))
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		a.create(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("origin=%q status=%d", origin, w.Code)
		}
	}
}

// TestReviewRetryRejectsLiveOwner не запускает второго координатора кнопкой retry.
func TestReviewRetryRejectsLiveOwner(t *testing.T) {
	s := reviewstore.New(t.TempDir())
	v, err := s.Create(reviewstore.CreateOptions{CWD: t.TempDir(), Prompt: "review", Config: reviewstore.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := s.AcquireExecution(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	a := reviewAPI{store: s, start: func(string, bool) error {
		t.Fatal("повтор при активном владельце")
		return nil
	}}
	r := httptest.NewRequest("POST", "http://localhost/retry", strings.NewReader(`{}`))
	r.Header.Set("Origin", "http://localhost")
	r.SetPathValue("review", v.ID)
	w := httptest.NewRecorder()
	a.retry(w, r)
	if w.Code != 409 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
