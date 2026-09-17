package runstore

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// SetTeamGoal обновляет текущий pin по явному решению Босса. Первоначальный
// заказ и его task.md не переписываются; прежние формулировки остаются в кадрах.
// Обычный вопрос Чела не является новой целью: это различает Босс по контексту.
func SetTeamGoal(ctx context.Context, root, run, author, id, goal string) (TeamMessage, error) {
	var result TeamMessage
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil || author != "boss" {
			return errors.New("цель уточняет только Босс")
		}
		goal = strings.TrimSpace(goal)
		if goal == "" || len(goal) > 32000 || !utf8.ValidString(goal) || id == "" {
			return errors.New("нужна цель до 32 КБ и ID события")
		}
		for _, m := range chat.Messages {
			if m.ID == id {
				if m.Kind == "goal_updated" && m.Goal == goal {
					result = m
					return nil
				}
				return errors.New("ID занят")
			}
		}
		boss := chat.Room.Actors["boss"]
		if chat.Room.AchievedAt != nil || boss == nil || boss.Delivery == nil || boss.Status != "working" {
			return errors.New("нет активного поручения Босса")
		}
		// Старые версии кадров не имели поля goal. До смены заполняем их прежним
		// pin, иначе UI ошибочно покажет новую формулировку в прошлом.
		if chat.History != nil {
			for i := range chat.History.Frames {
				if chat.History.Frames[i].Goal == "" {
					chat.History.Frames[i].Goal = chat.Goal
				}
			}
		}
		chat.Goal = goal
		chat.Members["system"] = TeamMember{Name: "Lawa"}
		result = TeamMessage{ID: id, AuthorID: "system", Kind: "goal_updated", Text: "Босс обновил цель команды.", Goal: goal, Date: time.Now().UTC()}
		chat.Messages = append(chat.Messages, result)
		return nil
	})
	return result, err
}
