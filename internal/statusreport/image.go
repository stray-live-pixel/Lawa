package statusreport

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/coordinator"
)

// ImageTheme ограничивает оформление двумя встроенными палитрами. Тема никогда
// не вставляется в PlantUML как произвольный код или имя внешнего include.
func ImageTheme(theme string) (string, error) {
	if theme == "" {
		theme = "dark"
	}
	if theme != "dark" && theme != "light" {
		return "", fmt.Errorf("тема должна быть dark или light")
	}
	return theme, nil
}

// RenderRunImage создаёт PNG по запросу, не изменяя файлы запуска. Для v2
// показана история посещений и причинных переходов, как в существующем PlantUML.
// Отмена запроса и таймаут CommandRenderer завершают внешний процесс.
func RenderRunImage(ctx context.Context, root, runID, theme string, renderer Renderer) ([]byte, error) {
	theme, err := ImageTheme(theme)
	if err != nil {
		return nil, err
	}
	status, err := coordinator.ReadStatus(root, runID)
	if err != nil {
		return nil, err
	}
	source, err := PlantUML(status)
	if err != nil {
		return nil, err
	}
	text := string(source)
	style := "skinparam backgroundColor #FFFFFF\nskinparam defaultFontColor #111111\n"
	if theme == "dark" {
		// Заменяем только собственные hex-цвета: внешний текст экранирован
		// plantText и не может совпасть с этими синтаксическими токенами.
		text = strings.NewReplacer("#F3F4F6", "#242424", "#FDE68A", "#45371C", "#93C5FD", "#17344F",
			"#86EFAC", "#193B24", "#CBD5E1", "#30343B", "#FCA5A5", "#4A2424",
			"#FBCFE8", "#472A40", "#FDBA74", "#49301C", "#D1D5DB", "#333333").Replace(text)
		style = "skinparam backgroundColor #080808\nskinparam defaultFontColor #F5F5F5\nskinparam ArrowColor #999999\nskinparam rectangleBorderColor #777777\nskinparam legendBackgroundColor #181818\nskinparam legendBorderColor #444444\nskinparam noteBackgroundColor #181818\nskinparam noteBorderColor #777777\n"
	}
	source = []byte(strings.Replace(text, "@startuml\n", "@startuml\n"+style, 1))
	if renderer == nil {
		renderer = CommandRenderer{}
	}
	image, err := renderer.Render(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("создать PNG; проверьте установку PlantUML: %w", err)
	}
	if !bytes.HasPrefix(image, pngSignature) {
		return nil, fmt.Errorf("PlantUML не вернул PNG")
	}
	return image, nil
}
