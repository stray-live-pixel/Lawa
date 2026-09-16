package dashboard

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestStaticUI проверяет настоящий встроенный build, а не пустой placeholder:
// все ресурсы index доступны, неизвестные API/файлы не маскируются SPA HTML.
func TestStaticUI(t *testing.T) {
	handler := Handler(t.TempDir())
	for _, path := range []string{"/", "/preview", "/office", "/graph/test"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		if recorder.Code != 200 || !strings.Contains(body, `id="root"`) || strings.Contains(body, "iframe") {
			t.Fatalf("не отдана React-оболочка: %s", body)
		}
		assets := regexp.MustCompile(`(?:src|href)="(/ui/[^\"]+)"`).FindAllStringSubmatch(body, -1)
		if len(assets) < 2 {
			t.Fatal("build не содержит JS/CSS")
		}
		for _, asset := range assets {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset[1], nil))
			if response.Code != 200 || response.Body.Len() < 100 || !strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
				t.Fatalf("ресурс не встроен: %s", asset[1])
			}
		}
	}
	for _, path := range []string{"/unknown", "/api/unknown", "/ui/assets/missing.js", "/ui/", "/ui/../meta.json"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != 404 {
			t.Fatalf("неизвестный путь %s получил %d", path, recorder.Code)
		}
	}
}
