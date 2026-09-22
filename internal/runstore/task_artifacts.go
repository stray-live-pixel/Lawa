package runstore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// TaskBasis описывает только явно перечисленные файлы. Fingerprints не доказывают качество кода
// и не гарантируют атомарный снимок внешней файловой системы.
type TaskBasis struct {
	Files map[string]string `json:"files"`
}

// CaptureTaskBasis ограничивает чтение каталогом задачи через os.Root: абсолютные
// пути, выход через .., ссылки наружу и специальные файлы не допускаются.
// Размер ограничен до чтения; документы/секреты за пределами cwd недоступны.
func CaptureTaskBasis(cwd string, paths []string) (TaskBasis, error) {
	out := TaskBasis{Files: map[string]string{}}
	if len(paths) == 0 || len(paths) > 100 {
		return out, errors.New("укажи 1–100 относительных файлов результата")
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return out, err
	}
	defer root.Close()
	total := int64(0)
	for _, path := range paths {
		clean := filepath.Clean(path)
		if !filepath.IsLocal(path) || clean != path || strings.HasPrefix(path, ".git/") || path == ".git" {
			return out, errors.New("артефакт должен быть относительным файлом внутри cwd, вне .git")
		}
		if _, ok := out.Files[path]; ok {
			return out, errors.New("повторный файл результата")
		}
		info, err := root.Lstat(path)
		if err != nil {
			return out, err
		}
		if !info.Mode().IsRegular() || info.Size() > 10<<20 {
			return out, errors.New("артефакт должен быть обычным файлом до 10 МиБ")
		}
		f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return out, err
		}
		opened, statErr := f.Stat()
		if statErr != nil || !opened.Mode().IsRegular() {
			f.Close()
			return out, errors.New("артефакт изменил тип во время открытия")
		}
		hash := sha256.New()
		n, readErr := io.Copy(hash, io.LimitReader(f, (10<<20)+1))
		closeErr := f.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return out, err
		}
		total += n
		if n > 10<<20 || total > 32<<20 {
			return out, errors.New("превышен лимит 10 МиБ на файл или 32 МиБ на результат")
		}
		out.Files[path] = fmt.Sprintf("%x", hash.Sum(nil))
	}

	return out, nil
}

// CheckTaskBasis сравнивает только заявленные файлы; посторонний файл не делает
// проверку устаревшей. Ссылка на внешнюю версию не заменяет сравнение файлов.
func CheckTaskBasis(cwd string, basis *TaskBasis) error {
	if basis == nil {
		return errors.New("нет проверенной основы результата")
	}
	paths := make([]string, 0, len(basis.Files))
	for path := range basis.Files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	current, err := CaptureTaskBasis(cwd, paths)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if current.Files[path] != basis.Files[path] {
			return fmt.Errorf("проверка устарела: изменён артефакт %s", path)
		}
	}
	return nil
}

// refreshTaskEvidence вызывается при явной операции/выдаче работы, а не на GET.
// Обнаруженное изменение сохраняется один раз с причиной и требует новой сдачи.
// Проверки не запускаются автоматически; меняется только актуальность доказательств.
func refreshTaskEvidence(chat *TeamChat, cwd string) {
	if chat.Room == nil {
		return
	}
	for _, task := range chat.Room.Tasks {
		c := task.Card
		if c == nil || c.Cancellation != "" || c.StaleReason != "" || len(c.Results) == 0 || (c.Status != "done" && c.Status != "in_review") {
			continue
		}
		r := c.Results[len(c.Results)-1]
		if len(r.Artifacts) == 0 {
			continue
		}
		if err := CheckTaskBasis(cwd, r.Basis); err != nil {
			c.StaleReason = err.Error()
			c.Status = "in_review"
			c.Version++
			key := fmt.Sprintf("stale-%s-%d", task.ID, c.Version)
			change := TaskChange{ID: key, Action: "evidence_stale", Author: "system", Date: time.Now().UTC(), FromRevision: c.Revision, Revision: c.Revision, Reason: c.StaleReason}
			c.History = append(c.History, change)
			snapshot := compactTask(*task)
			message := TeamMessage{ID: key, AuthorID: "system", To: "boss", Kind: "task_change", TaskID: task.ID, TaskRevision: c.Revision, TaskSnapshot: &snapshot, Date: change.Date, Text: c.StaleReason}
			chat.Messages = append(chat.Messages, message)
			wakeForMessage(chat, message)
			invalidateTaskDependents(chat, task.ID)
		}
	}
}
