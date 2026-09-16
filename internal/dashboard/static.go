package dashboard

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
)

// web содержит результат npm run build из ui/. Статика встроена в бинарник:
// установленному пользователю не нужны Node.js, npm, CDN или отдельный frontend.
// Сборка хранится в Git; CI проверяет её соответствие исходникам и lock-файлу.
//
//go:embed all:web
var web embed.FS

// serveUI возвращает оболочку dashboard, preview, офиса и графа. Путь выбирает
// React Router; неизвестные HTTP/API пути не становятся SPA.
func serveUI(w http.ResponseWriter, r *http.Request) {
	data, err := web.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "UI не собран", http.StatusInternalServerError)
		return
	}
	privateHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// serveUIAssets раздаёт только встроенную папку, не файлы пользовательского run.
// Хешированные ресурсы Vite неизменяемы; отсутствующий ресурс возвращает 404.
func serveUIAssets(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if !fs.ValidPath(name) || name == "." {
		http.NotFound(w, r)
		return
	}
	data, err := web.ReadFile("web/ui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".json"):
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	_, _ = w.Write(data)
}

// privateHeaders запрещает кеширование приватных данных и передачу URL referrer.
func privateHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// writeJSON сохраняет штатное экранирование encoding/json; React выводит строки
// как текст. Ни backend, ни frontend не исполняют HTML из workflow или сообщений.
func writeJSON(w http.ResponseWriter, value any) {
	privateHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
