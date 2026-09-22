package teamruntime

import (
	"context"
	"encoding/json/v2"
	"errors"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// taskTool работает и в новых dynamic tools, и через старый team_post.
// Автор захвачен обработчиком личности; ID операции выбирает агент и сохраняет
// при повторе, чтобы повторный turn не создавал новое поручение.
func (e *Engine) taskTool(ctx context.Context, run, author string, call codex.DynamicToolCall) (string, bool, error) {
	name, args := call.Tool, call.Arguments
	if name == "team_post" {
		var in struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(args, &in, json.RejectUnknownMembers(true)) != nil {
			return "", false, nil
		}
		command, value, ok := runstore.TaskCommandText(in.Text)
		if !ok {
			return "", false, nil
		}
		if command == "/task" {
			name = "team_task"
		} else {
			name = "team_task_read"
		}
		args = []byte(value)
	}
	var result any
	var err error
	switch name {
	case "team_task":
		var in runstore.TaskCommand
		err = json.Unmarshal(args, &in, json.RejectUnknownMembers(true))
		if err == nil {
			result, err = runstore.ApplyTaskCommand(ctx, e.Root, run, author, in)
		}
	case "team_task_read":
		var in runstore.TaskReadOptions
		err = json.Unmarshal(args, &in, json.RejectUnknownMembers(true))
		if err == nil {
			result, err = runstore.ReadTasks(e.Root, run, in)
		}
	default:
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	data, err := json.Marshal(result)
	if len(data) > 512000 {
		return "", true, errors.New("ответ слишком велик; уменьши страницу")
	}
	return string(data), true, err
}

// taskBoardPrompt одинаков для старых и новых thread. VCS и изоляция — действия
// агентов, а не обязательные внешние зависимости runtime Lawa.
const taskBoardPrompt = `
Работайте через карточки: team_task или team_post с /task <JSON>; чтение team_task_read или /task_read <JSON>.
Обычные реплики не создают задач. Босс создаёт, уточняет, назначает, принимает и отменяет задачи; Чел не обязан участвовать в операционке.
Команда: id (стабильный ключ повтора), action, taskId, version из свежей карточки.
create: title, body, expected, criteria[], assignee приглашённого сотрудника, priority (-100..100), dependencies[].
edit заменяет title/body/expected/criteria и повышает revision; assign меняет assignee; priority/dependencies меняют одноимённое поле.
Исполнитель: read фиксирует ознакомление, acknowledge подтверждает текущую revision, start берёт готовую задачу; это разные действия.
block с reason сообщает препятствие, пустой reason снимает его. report передаёт result {summary,artifacts:[относительные файлы внутри cwd],links:[внешние ссылки],versionRef:необязательная версия,checks:[фактические проверки],limitations}.
Проверяющий: review с resultId, verdict approve|changes_requested, reason — собственная проверка. Босс: return с reason либо accept с resultId, reason и integration (как результат включён/где доступен).
Перед результатом и приёмкой перечитай карточку, изменения и зависимости. Ответ о готовности связан с первоначальным поручением автоматически.
Изоляция по возможности: выбери отдельную папку/копию, worktree, клон или средства VCS проекта. VCS может отсутствовать. Не смешивай задачи, сохраняй чужие изменения. workspace с workspace {path,method,limitations} фиксирует способ и ограничения; отсутствие изоляции не блокирует работу, но нельзя обещать гарантированный откат общей папки. Backend не выполняет VCS-команд.
Босс проверяет и собирает результат средствами проекта. Зависимая работа использует принятые результаты предшественников; их ссылки и проверки читай в results/reviews.
cancel с reason запрещает сдачу/приёмку и запрашивает прекращение работы. Агент останавливается на безопасной границе, сохраняет архив и сообщает cancel_evidence с reason: как вклад исключён без потери чужого. Если вклад уже включён, Босс организует исключение и повторную проверку зависимых задач. cancel_finish с reason доступен Боссу после завершения хода исполнителя и доказательств исключения. Отмена не означает успех и не откатывает внешние публикации.
Чтение: taskId (карточка), section history|results|reviews|messages, offset, limit. Без taskId — список с after. Следуй явным продолжениям; личный thread не переписывается.
Связанный чат: team_post {text,taskId,replyTo,revision}; для старой schema используй /message {text,taskId,replyTo,revision}. Общий пост без адресата допустим и никого не запускает.
`

// taskCommandSchema описывает только входы; автора, время и fingerprints добавляет сервер.
const taskCommandSchema = `{"type":"object","properties":{"id":{"type":"string"},"action":{"type":"string","enum":["create","edit","assign","priority","dependencies","read","acknowledge","start","workspace","block","report","review","return","accept","cancel","cancel_evidence","cancel_finish"]},"taskId":{"type":"string"},"version":{"type":"integer","minimum":0},"title":{"type":"string"},"body":{"type":"string"},"expected":{"type":"string"},"criteria":{"type":"array","items":{"type":"string"}},"assignee":{"type":"string"},"priority":{"type":"integer"},"dependencies":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"},"resultId":{"type":"string"},"verdict":{"type":"string"},"integration":{"type":"string"},"workspace":{"type":"object","properties":{"path":{"type":"string"},"method":{"type":"string"},"limitations":{"type":"string"}},"required":["path","method","limitations"],"additionalProperties":false},"result":{"type":"object","properties":{"summary":{"type":"string"},"artifacts":{"type":"array","items":{"type":"string"}},"links":{"type":"array","items":{"type":"string"}},"versionRef":{"type":"string"},"checks":{"type":"array","items":{"type":"string"}},"limitations":{"type":"string"}},"required":["summary","checks","limitations"],"additionalProperties":false}},"required":["id","action","taskId","version"],"additionalProperties":false}`
