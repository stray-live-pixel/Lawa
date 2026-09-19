package teamruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// bossResponsibilityPrompt задаёт критерии самостоятельной работы Босса.
// Учёт доставки проверяет runtime; смысл результата и соответствие исходной
// цели оценивает модель по реальным артефактам, а не по факту окончания turn.
const bossResponsibilityPrompt = `
Определи проверяемые признаки достижения цели. Поручай работу с ожидаемым результатом.
В team_read.room.tasks — обязательства. Ответ, вопрос и завершение turn не означают выполнения.
Проверь артефакты и поведение; прими выполненные поручения через team_accept:
task_ids, result_id отчёта сотрудника, evidence — что и как проверил, до 40 слов.
При недочётах дай уточняющее поручение; прими исходное и уточняющее после проверки.
Если коллега ещё работает, заверши ход: окончание его turn разбудит тебя, если ты уже получил отчёт.
Системный @boss требует решения. Без отчёта запроси результат уже сделанной работы.
При доказанном сбое до отправки используй team_retry с id. Не повторяй неоднозначную доставку:
исследуй препятствие и обращайся к Челу лишь когда сам решить его не можешь.
Для старого thread используй team_post: /accept {"task_ids":[...],"result_id":"...","evidence":"..."} или /retry id.
Закрывай цель лишь после приёмки поручений и собственной проверки всей исходной цели.
Не подменяй её достигнутой частью; если результат не проверен, честно сообщи препятствие.`

// controlTool реализует приёмку и безопасное восстановление силами Босса.
// Старые thread имеют неизменяемую schema dynamic tools, поэтому те же операции
// доступны через команды team_post. Авторство всегда захватывается runtime.
func (e *Engine) controlTool(ctx context.Context, run, author string, call codex.DynamicToolCall) (string, bool, error) {
	name, args := call.Tool, call.Arguments
	if name == "team_post" {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(args, &in, json.RejectUnknownMembers(true)); err != nil {
			return "", false, nil
		}
		if value, ok := strings.CutPrefix(in.Text, "/wait "); ok {
			var in struct {
				Kind      string `json:"kind"`
				ActorID   string `json:"actor_id"`
				MessageID string `json:"message_id"`
				Text      string `json:"text"`
			}
			if err := json.Unmarshal([]byte(value), &in, json.RejectUnknownMembers(true)); err != nil {
				return "", true, err
			}
			err := runstore.SetTeamWait(ctx, e.Root, run, author, in.Kind, in.ActorID, in.MessageID, in.Text)
			return `{"saved":true}`, true, err
		}
		if value, ok := strings.CutPrefix(in.Text, "/accept "); ok {
			name = "team_accept"
			args = []byte(value)
		} else if value, ok := strings.CutPrefix(in.Text, "/retry "); ok {
			name = "team_retry"
			args, _ = json.Marshal(map[string]string{"id": strings.TrimSpace(value)})
		}
	}
	if name != "team_accept" && name != "team_retry" {
		return "", false, nil
	}
	if author != "boss" {
		return "", true, errors.New("приёмка и восстановление доступны только Боссу")
	}
	if call.CallID == "" {
		return "", true, errors.New("нет callId")
	}
	key := fmt.Sprintf("tool-%x", sha256.Sum256([]byte(call.ThreadID+"\x00"+call.TurnID+"\x00"+call.CallID)))
	var result any
	var err error
	if name == "team_accept" {
		var in struct {
			TaskIDs  []string `json:"task_ids"`
			ResultID string   `json:"result_id"`
			Evidence string   `json:"evidence"`
		}
		if err = json.Unmarshal(args, &in, json.RejectUnknownMembers(true)); err == nil {
			result, err = runstore.AcceptTeamTasks(ctx, e.Root, run, author, key, in.TaskIDs, in.ResultID, in.Evidence)
		}
	} else {
		var in struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(args, &in, json.RejectUnknownMembers(true)); err == nil {
			result, err = e.retryByBoss(ctx, run, key, in.ID)
		}
	}
	if err != nil {
		return "", true, err
	}
	data, err := json.Marshal(result)
	return string(data), true, err
}

// retryByBoss атомарно сохраняет решение Босса и разрешает только недоставленную
// порцию. Повтор tool call после потери подтверждения возвращает старое событие,
// даже если сотрудник уже выполняет новый turn: второго запуска не возникает.
func (e *Engine) retryByBoss(ctx context.Context, run, eventID, id string) (runstore.TeamMessage, error) {
	var result runstore.TeamMessage
	err := runstore.UpdateTeam(ctx, e.Root, run, func(chat *runstore.TeamChat) error {
		if chat.Room == nil {
			return errors.New("нет комнаты")
		}
		text := fmt.Sprintf("Босс возобновил работу @%s после сбоя до отправки.", id)
		for _, m := range chat.Messages {
			if m.ID == eventID {
				if m.AuthorID != "system" || m.Text != text {
					return errors.New("ID занят")
				}
				result = m
				return nil
			}
		}
		boss := chat.Room.Actors["boss"]
		if id == "boss" || boss == nil || boss.Status != "working" || boss.Delivery == nil {
			return errors.New("нужно активное поручение Босса и ID сотрудника")
		}
		if err := resetUnsent(chat, id); err != nil {
			return err
		}
		result = runstore.TeamMessage{ID: eventID, AuthorID: "system", Kind: "system", Date: time.Now().UTC(), Text: text}
		chat.Members["system"] = runstore.TeamMember{Name: "Lawa"}
		chat.Messages = append(chat.Messages, result)
		return nil
	})
	return result, err
}

// resetUnsent разделяется между UI и Боссом: ни один путь не разрешает повтор
// потенциально выполненной работы. Вызывается только под общей блокировкой чата.
func resetUnsent(chat *runstore.TeamChat, id string) error {
	if chat.Room == nil || chat.Room.Actors[id] == nil {
		return errors.New("нет сотрудника")
	}
	if chat.Room.AchievedAt != nil {
		return errors.New("цель уже достигнута")
	}
	a := chat.Room.Actors[id]
	if a.Status != "blocked" || a.Delivery == nil || a.Delivery.Attempted {
		return errors.New("повтор неоднозначной доставки запрещён; проверьте историю Codex")
	}
	a.Delivery = nil
	a.ApprovalPending = false
	a.CapacityPending = false
	a.Status = "idle"
	a.Error = ""
	a.NextCheck = time.Now().UTC()
	return nil
}
