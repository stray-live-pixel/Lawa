package main

import (
	"context"
	"io"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/statusreport"
)

// defaultChatInterval ограничивает расход токенов чата-инициатора. Подробные
// локальные файлы обновляются координатором каждую минуту и при каждом изменении,
// но одинаково подробный текст больше не переносится в чат на каждом событии.
const defaultChatInterval = 5 * time.Minute

// statusPublisher разделяет локальный и чат-интерфейс одного снимка. Publish
// вызывается координатором последовательно, поэтому mutex не нужен: сначала
// атомарно обновляются Markdown и PlantUML, затем при наступлении отдельного
// пятиминутного срока печатается только короткая статистика и VS Code-ссылка.
// Первый снимок и любой терминальный исход видны сразу, иначе пользователь мог
// бы пять минут не знать runId или не получить подтверждение ошибки agent-графа.
type statusPublisher struct {
	ctx          context.Context
	out          io.Writer
	runDir       string
	renderer     statusreport.Renderer
	chatInterval time.Duration
	now          func() time.Time
	lastChat     time.Time
	chatPrinted  bool
}

// newStatusPublisher заполняет production-defaults и оставляет часы заменяемыми
// для быстрых тестов без реального ожидания пяти минут.
func newStatusPublisher(
	ctx context.Context,
	out io.Writer,
	runDir string,
	renderer statusreport.Renderer,
	chatInterval time.Duration,
	now func() time.Time,
) *statusPublisher {
	if chatInterval <= 0 {
		chatInterval = defaultChatInterval
	}
	if now == nil {
		now = time.Now
	}
	return &statusPublisher{
		ctx: ctx, out: out, runDir: runDir, renderer: renderer,
		chatInterval: chatInterval, now: now,
	}
}

// Publish всегда обновляет локальный подробный отчёт. Ошибка PlantUML или записи
// артефактов не останавливает workflow: ближайшая чат-сводка коротко предупредит,
// что файлы обновлены не полностью. Ошибка stdout остаётся фатальной, потому что
// без канала вывода управляющий агент потеряет даже редкий heartbeat и финал.
func (p *statusPublisher) Publish(status coordinator.Status) error {
	_, artifactErr := statusreport.WriteReport(p.ctx, p.runDir, status, p.renderer)
	now := p.now()
	chatDue := !p.chatPrinted || status.Terminal || status.Complete || !now.Before(p.lastChat.Add(p.chatInterval))
	if !chatDue {
		return nil
	}
	if _, err := io.WriteString(p.out, statusreport.Summary(status, p.runDir, artifactErr)); err != nil {
		return err
	}
	p.lastChat = now
	p.chatPrinted = true
	return nil
}
