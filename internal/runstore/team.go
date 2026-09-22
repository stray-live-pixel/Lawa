package runstore

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// TeamChat — общая доска корневого заказа. Текущая цель версионируется кадрами, сообщения добавляются
// последовательно. Память кубиков остаётся рабочими заметками, чат — общими фактами.
type TeamChat struct {
	ContextPolicy *TeamContextPolicy    `json:"contextPolicy,omitempty"`
	Compaction    *TeamCompaction       `json:"compaction,omitempty"`
	Metrics       *TeamMetrics          `json:"metrics,omitempty"`
	History       *TeamHistory          `json:"history,omitempty"`
	Room          *TeamRoom             `json:"room,omitempty"`
	RunID         string                `json:"runId"`
	Goal          string                `json:"goal"`
	Members       map[string]TeamMember `json:"members"`
	Messages      []TeamMessage         `json:"messages"`
}

// TeamMember хранит отображение автора по ID. Avatar задаёт известный UI образ;
// для остальных участников UI строит стабильную аватарку по тому же ID.
type TeamMember struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

// TeamMessage получает время и автора на стороне Lawa. ID — ключ повтора:
// потеря сетевого подтверждения не должна удваивать сообщение при retry.
type TeamMessage struct {
	LinkInput       string       `json:"linkInput,omitempty"`
	TaskSnapshot    *TeamTask    `json:"taskSnapshot,omitempty"`
	TaskID          string       `json:"taskId,omitempty"`
	TaskRevision    uint64       `json:"taskRevision,omitempty"`
	Position        int          `json:"position,omitempty"` // Позиция в ответе чтения; исходный журнал задаёт порядок индексом.
	Summary         *TeamSummary `json:"summary,omitempty"`
	DiscussionID    string       `json:"discussionId,omitempty"`
	DiscussionCycle int          `json:"discussionCycle,omitempty"`
	DiscussionInput string       `json:"discussionInput,omitempty"` // Исходная команда для проверки повторного ID.
	Suppressed      bool         `json:"suppressed,omitempty"`      // История остаётся видимой, автоматическая доставка остановлена.

	TaskIDs    []string  `json:"taskIds,omitempty"`  // Поручения, явно принятые Боссом этим событием.
	ResultID   string    `json:"resultId,omitempty"` // Сообщение с проверенным результатом.
	Goal       string    `json:"goal,omitempty"`
	NotifyBoss bool      `json:"notifyBoss,omitempty"`
	To         string    `json:"to,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	ReplyToIDs []string  `json:"replyToIds,omitempty"` // Входы порции, на которые отвечает реплика или опирается вопрос.
	ReplyTo    string    `json:"replyTo,omitempty"`
	ID         string    `json:"id"`
	AuthorID   string    `json:"authorId"`
	Date       time.Time `json:"date"`
	Text       string    `json:"text"`
}

// newTeam создаёт pin точного входного задания, без ограничения в 50 слов.
func newTeam(runID, goal string) TeamChat {
	return TeamChat{RunID: runID, Goal: goal, Members: map[string]TeamMember{"human": {Name: "Чел"}}, Messages: []TeamMessage{}}
}

// TeamRoot разрешает принадлежность команде только по сохранённым родителям.
// Набор visited останавливает повреждённую цепочку вместо вечного обхода.
func TeamRoot(root, runID string) (Snapshot, error) {
	seen := map[string]bool{}
	for {
		if seen[runID] {
			return Snapshot{}, errors.New("цикл родителей команды")
		}
		seen[runID] = true
		s, err := Load(root, runID)
		if err != nil {
			return Snapshot{}, err
		}
		if s.Meta.ParentRunID == "" {
			return s, nil
		}
		runID = s.Meta.ParentRunID
	}
}

// readTeam поддерживает старые run без миграции при чтении. Для новых запусков
// точная цель записана Create; legacy task.md содержит также служебные заголовки.
func readTeam(dir *os.Root, s Snapshot) (TeamChat, error) {
	data, err := readFile(dir, "team.json")
	if errors.Is(err, os.ErrNotExist) {
		goal := strings.TrimPrefix(s.Task, "# Постановка задачи\n\n")
		goal, _, _ = strings.Cut(goal, "\n\n# Комментарий пользователя\n\n")
		return newTeam(s.Meta.RunID, strings.TrimSpace(goal)), nil
	}
	if err != nil {
		return TeamChat{}, err
	}
	var chat TeamChat
	if err = json.Unmarshal(data, &chat); err != nil {
		return chat, err
	}
	if chat.RunID != s.Meta.RunID || chat.Members == nil || strings.TrimSpace(chat.Goal) == "" {
		return TeamChat{}, errors.New("повреждена общая база команды")
	}
	if chat.Room != nil {
		// Legacy-комнаты не имели каталога. Не добавляем им новые роли из конфига:
		// прежние thread/tools знают только Босса и Разработчика.
		if chat.Room.Catalog == nil {
			chat.Room.Catalog = workflow.DefaultTeamCharacters()
			if boss, ok := s.Workflow.Characters["boss"]; ok {
				chat.Room.Catalog["boss"] = boss
			}
		}
		initializeTeamTasks(&chat)
		if err := chat.validateRoom(); err != nil {
			return TeamChat{}, err
		}
	}
	if err := validateTeamContext(chat); err != nil {
		return TeamChat{}, err
	}
	if err := validateTeamMetrics(chat.Metrics); err != nil {
		return TeamChat{}, err
	}
	return chat, nil
}

// ReadTeam видит целый старый или новый снимок благодаря атомарному Rename.
func ReadTeam(root, runID string) (TeamChat, error) {
	s, err := TeamRoot(root, runID)
	if err != nil {
		return TeamChat{}, err
	}
	dir, err := openRun(root, s.Meta.RunID)
	if err != nil {
		return TeamChat{}, err
	}
	defer dir.Close()
	return readTeam(dir, s)
}

// PostTeam связывает автора с реальным кубиком sourceRun. Пустой stepID означает
// человека; HTTP не принимает произвольный authorId, агентский tool всегда
// передаёт свой захваченный stepID. Это локальная идентификация, не аутентификация.
// Отдельный flock не конфликтует с долгим coordinator.lock. Нельзя удалять его
// файл: все процессы должны блокировать один inode. Запись атомарна с fsync.
func PostTeam(ctx context.Context, root, sourceRun, stepID, id, text string) (TeamMessage, error) {
	text = strings.TrimSpace(text)
	if text == "/compact_retry" && stepID == "" {
		return RetryTeamCompaction(ctx, root, sourceRun, "human", id)
	}
	words := len(strings.Fields(text))
	if words == 0 || words > 50 || len(text) > 8192 || !utf8.ValidString(text) {
		return TeamMessage{}, errors.New("сообщение должно содержать от 1 до 50 слов (до 8 КБ)")
	}
	if id == "" || len(id) > 200 || !utf8.ValidString(id) {
		return TeamMessage{}, errors.New("нужен ID сообщения до 200 байт")
	}
	source, err := Load(root, sourceRun)
	if err != nil {
		return TeamMessage{}, err
	}
	authorID, member := "human", TeamMember{Name: "Чел"}
	if stepID != "" {
		found := false
		for _, step := range source.Workflow.Steps {
			if step.ID != stepID {
				continue
			}
			found = true
			authorID, member.Name = sourceRun+":step:"+step.ID, step.ID
			if step.Character != "" {
				authorID = sourceRun + ":character:" + step.Character
				member.Name = source.Workflow.Characters[step.Character].Name
				if step.Character == "boss" {
					member.Avatar = "boss"
				}
			}
			break
		}
		if !found {
			return TeamMessage{}, fmt.Errorf("неизвестный участник %q", stepID)
		}
	}
	var message TeamMessage
	err = UpdateTeam(ctx, root, sourceRun, func(chat *TeamChat) error {
		var postErr error
		if chat.Room != nil {
			if stepID != "" {
				return errors.New("комнатой управляет командный runtime")
			}
			message, postErr = appendRoomMessage(chat, "human", id, text)
			return postErr
		}
		for _, previous := range chat.Messages {
			if previous.ID != id {
				continue
			}
			if previous.AuthorID != authorID || previous.Text != text {
				return errors.New("ID уже принадлежит другому сообщению")
			}
			message = previous
			return nil
		}
		message = TeamMessage{ID: id, AuthorID: authorID, Date: time.Now().UTC(), Text: text}
		chat.Members[authorID] = member
		chat.Messages = append(chat.Messages, message)
		return nil
	})
	return message, err
}

// UpdateTeam сериализует короткую транзакцию общей базы между процессами.
// Callback не выполняет сеть/модель и не вызывает UpdateTeam повторно.
func UpdateTeam(ctx context.Context, root, runID string, update func(*TeamChat) error) error {
	s, err := TeamRoot(root, runID)
	if err != nil {
		return err
	}
	dir, err := openRun(root, s.Meta.RunID)
	if err != nil {
		return err
	}
	defer dir.Close()
	// os.Root сам разрешает ссылки внутри области; Lstat запрещает их до открытия.
	// O_NONBLOCK не позволяет зависнуть на подставленном FIFO. Не используем
	// O_NOFOLLOW вместе с O_CREATE: на macOS это даёт ENOENT при гонке создания.
	if info, statErr := dir.Lstat("team.lock"); statErr == nil {
		if !info.Mode().IsRegular() {
			return errors.New("team.lock должен быть обычным файлом")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	lock, err := dir.OpenFile("team.lock", os.O_CREATE|os.O_RDWR|syscall.O_NONBLOCK, 0o600)
	// macOS иногда возвращает ENOENT для O_CREATE, когда соседний процесс уже
	// создал тот же файл. Открываем существующий inode без создания; если файл
	// действительно отсутствует, ошибка сохранится. Lock никогда не удаляем.
	if errors.Is(err, os.ErrNotExist) {
		lock, err = dir.OpenFile("team.lock", os.O_RDWR|syscall.O_NONBLOCK, 0)
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("team.lock должен быть обычным файлом")
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	chat, err := readTeam(dir, s)
	if err != nil {
		return err
	}
	// Старой комнате сначала сохраняем достоверную точку «сейчас». Более
	// ранние состояния восстанавливаются отдельно, без догадок при обычном GET.
	if chat.Room != nil && chat.History == nil {
		recordTeamFrame(&chat, time.Now())
	}
	if chat.Room != nil {
		initializeDiscussions(&chat)
	}
	if err = update(&chat); err != nil {
		return err
	}
	if chat.Room != nil {
		wakeReadyTasks(&chat)
	}
	ensureTeamCompaction(&chat, time.Now())
	RefreshTeamWaits(&chat, time.Now())
	recordTeamFrame(&chat, time.Now())
	data, err := json.Marshal(chat)
	if err != nil {
		return err
	}
	return saveRunFile(dir, "team.json", data, (*os.File).Sync)
}
