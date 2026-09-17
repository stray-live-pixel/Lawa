package runstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// teamRun создаёт настоящие папки, чтобы проверить общий чат через те же
// границы процессов/файлов, которые используют координатор и HTTP.
func teamRun(t *testing.T, root, parent, goal string) Snapshot {
	t.Helper()
	s, err := Create(root, Input{WorkflowJSON: []byte(`{"id":"team","characters":{"boss":{"name":"Босс","history":"Начало","instructions":"Веди команду"}},"steps":[{"id":"work","type":"agent","character":"boss","prompt":"Работай","dependsOn":[]}]}`), Task: goal, CWD: t.TempDir(), ParentRunID: parent})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Параллельные отправители не теряют сообщения, а дочерний run не подменяет
// общую цель своей задачей. coordinator.lock не блокирует живой чат команды.
func TestTeamConcurrentChildrenAndRetry(t *testing.T) {
	root := t.TempDir()
	goal := "  Исходная цель\n\n# Комментарий пользователя\n\nЭто тоже цель  "
	parent := teamRun(t, root, "", goal)
	child := teamRun(t, root, parent.Meta.RunID, "Частное поручение")
	locked, err := OpenLocked(root, parent.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := PostTeam(t.Context(), root, child.Meta.RunID, "work", fmt.Sprint(i), "Проверка завершена"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	first, err := ReadTeam(root, parent.Meta.RunID)
	if err != nil || len(first.Messages) != 16 || first.Goal != goal {
		t.Fatalf("неверная история: %+v, %v", first, err)
	}
	old := first.Messages[0]
	repeated, err := PostTeam(t.Context(), root, child.Meta.RunID, "work", old.ID, old.Text)
	if err != nil || repeated != old {
		t.Fatalf("retry: %+v %v", repeated, err)
	}
	if _, err = PostTeam(t.Context(), root, parent.Meta.RunID, "", old.ID, old.Text); err == nil {
		t.Fatal("ID разрешил сменить автора")
	}
	chat, err := ReadTeam(root, child.Meta.RunID)
	if err != nil || chat.RunID != parent.Meta.RunID || len(chat.Messages) != 16 || chat.Members[old.AuthorID].Avatar != "boss" {
		t.Fatalf("дочерняя команда: %+v %v", chat, err)
	}
	other := teamRun(t, root, "", "Другой заказ")
	separate, err := ReadTeam(root, other.Meta.RunID)
	if err != nil || len(separate.Messages) != 0 {
		t.Fatal("заказы смешались", err)
	}
}

// Граница 50 слов одинаково работает с кириллицей и Unicode-пробелами.
// Неверные сообщения, авторы и отменённые операции не изменяют durable историю.
func TestTeamValidationAndLegacy(t *testing.T) {
	root := t.TempDir()
	s := teamRun(t, root, "", "Цель")
	for _, text := range []string{"", " \n", strings.Repeat("слово\u0085", 51), strings.Repeat("а", 8193)} {
		if _, err := PostTeam(t.Context(), root, s.Meta.RunID, "", "bad", text); err == nil {
			t.Fatal("принят неверный текст")
		}
	}
	if _, err := PostTeam(t.Context(), root, s.Meta.RunID, "unknown", "author", "текст"); err == nil {
		t.Fatal("принят чужой автор")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PostTeam(ctx, root, s.Meta.RunID, "", "cancelled", "текст"); err == nil {
		t.Fatal("игнорирована отмена")
	}
	if _, err := PostTeam(t.Context(), root, s.Meta.RunID, "", "valid", strings.Repeat("слово\u0085", 50)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, s.Meta.RunID, "team.json")); err != nil {
		t.Fatal(err)
	}
	legacy, err := ReadTeam(root, s.Meta.RunID)
	if err != nil || legacy.Goal != "Цель" || len(legacy.Messages) != 0 {
		t.Fatalf("legacy: %+v %v", legacy, err)
	}
	if _, err = os.Stat(filepath.Join(root, s.Meta.RunID, "team.json")); !os.IsNotExist(err) {
		t.Fatal("чтение записало миграцию")
	}
}
