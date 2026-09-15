package dashboard

import (
	"fmt"
	"net/http"

	"github.com/stray-live-pixel/Lawa/internal/statusreport"
)

// imageSlots ограничивает дорогой локальный рендер: параллельные запросы не
// запускают неограниченное число JVM. Занятый слот возвращает повторяемую ошибку.
var imageSlots = make(chan struct{}, 1)

// graphImage отдаёт свежую картинку inline либо attachment, не используя старый
// workflow-status.png. Имя файла составлено из проверенного runID и enum темы.
func (h handler) graphImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	theme, err := statusreport.ImageTheme(r.URL.Query().Get("theme"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case imageSlots <- struct{}{}:
		defer func() { <-imageSlots }()
	default:
		http.Error(w, "Генерация уже идёт. Повторите запрос позже.", http.StatusTooManyRequests)
		return
	}
	data, err := statusreport.RenderRunImage(r.Context(), h.root, r.PathValue("run"), theme, nil)
	if err != nil {
		http.Error(w, diagnostic(err), http.StatusUnprocessableEntity)
		return
	}
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="workflow-%s-%s.png"`, disposition, r.PathValue("run"), theme))
	_, _ = w.Write(data)
}
