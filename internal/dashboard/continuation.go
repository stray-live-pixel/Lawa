package dashboard

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// continuationPrompt переносит ссылки на сохранённый контекст в новый чат.
// Вызывающий код передаёт абсолютный root и проверенный snapshot. Это инструкция
// для чтения, а не команда resume: исходный запуск может ещё изменять репозиторий.
// Для v2 выбираем точный visit, для legacy — отдельный ThreadID файла памяти.
// ID и пути записаны в кавычках с экранированием, а не исполняемым shell-кодом.
func continuationPrompt(root string, snapshot runstore.Snapshot, stepID, visitID string) string {
	var text strings.Builder
	dir := filepath.Join(root, snapshot.Meta.RunID)
	fmt.Fprintln(&text, "Продолжи работу в этом новом интерактивном чате по моей дополнительной задаче в конце сообщения.")
	fmt.Fprintf(&text, "\nКонтекст Lawa:\n- runId: %q\n- workflowId: %q\n- рабочая папка (cwd): %q\n- корень запусков: %q\n", snapshot.Meta.RunID, snapshot.Workflow.ID, snapshot.Meta.CWD, root)
	if snapshot.Meta.ParentRunID != "" {
		fmt.Fprintf(&text, "- parentRunId: %q; папка родителя: %q\n", snapshot.Meta.ParentRunID, filepath.Join(root, snapshot.Meta.ParentRunID))
	}
	if stepID == "" {
		fmt.Fprintln(&text, "- область: весь workflow; сверь все steps/visits и их зависимости.")
	} else {
		fmt.Fprintf(&text, "- область: кубик stepId=%q, visitId=%q (пустой visitId означает legacy или ещё не созданное посещение).\n", stepID, visitID)
		memoryID, threadID, turnID := "", "", ""
		for _, step := range snapshot.Meta.Steps {
			if step.ID == stepID {
				memoryID, threadID, turnID = step.ThreadID, step.CodexThreadID, step.TurnID
			}
		}
		for _, visit := range snapshot.Meta.Visits {
			if visit.VisitID == visitID && visit.StepID == stepID {
				memoryID, threadID, turnID = visit.VisitID, visit.CodexThreadID, visit.TurnID
				fmt.Fprintf(&text, "- проход: %d; итерация: %d; попытка: %d\n", visit.Visit, visit.Iteration, visit.Attempt)
			}
		}
		fmt.Fprintf(&text, "- codexThreadId: %q; turnId: %q (пустые ID означают отсутствие сохранённой сессии).\n", threadID, turnID)
		if memoryID != "" {
			fmt.Fprintf(&text, "- память выбранного исполнения: %q\n", filepath.Join(dir, "memory", memoryID+".md"))
		}
	}
	fmt.Fprintf(&text, `
Сначала прочитай инструкции репозитория в cwd и сохранённые файлы:
- постановка: %q
- схема, prompts и зависимости: %q
- состояния, ID сессий и связи посещений: %q
- история сообщений и действий: %q
- память исполнений: %q

Прочитай историю выбранного исполнения целиком, включая прошлые попытки, а текущую попытку различай по turnId. Для v2 фильтруй по visitId, для legacy — по stepId. Прочитай память зависимостей и источников trigger.sourceVisitIds; для всего workflow — всех исполнений. Проверь артефакты по ссылкам из сообщений и памяти. Если доступно чтение Codex thread по ID, используй его для дополнительной истории; если нет, используй сохранённые файлы и явно сообщи о пробелах. Не считай отсутствующую историю успехом.

Файлы могут обновляться: перечитай meta.json перед началом работы, отдели завершённое от активного. Не запускай автоматически lawa run/resume и не меняй служебные файлы исходного run. Если в той же папке ещё работает агент, согласуй способ избежать одновременных правок. Продолжай интерактивно в этом чате; кратко объясни найденный результат, затем выполни дополнительную задачу. Если она не заполнена, спроси её.

Моя дополнительная задача: [впишите задачу]
`, filepath.Join(dir, "task.md"), filepath.Join(dir, "workflow.json"), filepath.Join(dir, "meta.json"), filepath.Join(dir, "events.jsonl"), filepath.Join(dir, "memory"))
	return text.String()
}
