package runstore

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWriteCompactionFixture создаёт только явный синтетический заказ для QA.
// Текущая цель достигнута: serve не запускает модель. Нужен отдельный пустой root.
// LAWA_COMPACTION_FIXTURE_ROOT=/tmp/... go test ./internal/runstore -run TestWriteCompactionFixture -v
func TestWriteCompactionFixture(t *testing.T) {
	root := os.Getenv("LAWA_COMPACTION_FIXTURE_ROOT")
	if root == "" {
		t.Skip("fixture создаётся только по явному запросу")
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatal("нужен пустой временный root")
	}
	s, err := Create(root, Input{Order: true, Team: true, CWD: root, Task: "Синтетическая проверка: контекст команды, архив и версии сводки", WorkflowJSON: []byte(`{"id":"compaction-fixture","characters":{"boss":{"name":"Босс","history":"Синтетический сценарий","instructions":"Не запускать модель"}},"steps":[{"id":"boss","type":"agent","character":"boss","prompt":"Fixture","dependsOn":[]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	err = UpdateTeam(t.Context(), root, s.Meta.RunID, func(chat *TeamChat) error {
		chat.Messages = nil
		chat.History = nil
		chat.Members["developer"] = TeamMember{Name: "Программист", Avatar: "developer"}
		at := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
		for i := 1; i <= 90; i++ {
			author := "developer"
			if i%3 == 0 {
				author = "boss"
			}
			text := fmt.Sprintf("Проверка %d: исходные сообщения сохранены, страницы читаются последовательно. Открытый вопрос: кто проверит узкое окно? Решение о цвете пока только предложено.", i)
			chat.Messages = append(chat.Messages, TeamMessage{ID: fmt.Sprintf("source-%03d", i), AuthorID: author, Text: text, Date: at.Add(time.Duration(i) * time.Minute)})
			if i == 40 || i == 75 {
				through := len(chat.Messages) - 10
				summary := &TeamSummary{RequestID: fmt.Sprintf("fixture-%d", i), Through: through, Text: fmt.Sprintf("Сводка %d. Договорились сохранять оригиналы и читать свежие сообщения целиком.\n\nОткрыто: проверить узкое окно. Предложение цвета ещё не принято.\n\nРезультат: ограниченные страницы работают; приёмка QA впереди.", i), SourceIDs: []string{"source-001", fmt.Sprintf("source-%03d", i-12)}}
				chat.Messages = append(chat.Messages, TeamMessage{ID: fmt.Sprintf("summary-%d", i), AuthorID: "boss", Kind: "compaction", Text: "Босс сжал прошлую переписку. Оригиналы сохранены.", Date: at.Add(time.Duration(i)*time.Minute + time.Second), Summary: summary})
			}
			recordTeamFrame(chat, at.Add(time.Duration(i)*time.Minute+2*time.Second))
		}
		done := at.Add(2 * time.Hour)
		chat.Messages = append(chat.Messages, TeamMessage{ID: "fixture-achieved", AuthorID: "system", Kind: "achievement", Date: done, Text: "Синтетическая проверка подготовлена"}, TeamMessage{ID: "fixture-final", AuthorID: "boss", To: "human", Date: done, Text: "@human Это синтетические данные, не реальный модельный эксперимент."})
		chat.Room.AchievedAt = &done
		chat.Room.Actors["boss"].Status = "idle"
		chat.Room.Actors["boss"].NextCheck = time.Time{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "fixture-run-id"), []byte(s.Meta.RunID), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Синтетический заказ %s; /office?run=%s", s.Meta.RunID, s.Meta.RunID)
}
