package workflow

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Character — неизменяемое описание личности, а не поручение кубика. History
// задаёт предысторию; приобретённый опыт агент сохраняет в отдельной памяти run.
// Один ID означает одну личность внутри запуска, но не между заказами человека.
type Character struct {
	Name         string `json:"name"`
	History      string `json:"history"`
	Instructions string `json:"instructions"`
}

// characterID ограничивает ID безопасным компонентом имени файла памяти.
var characterID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Boss возвращает стартовую личность. Её описание сохраняется в workflow.json,
// поэтому обновление бинарника не меняет характер уже созданного Босса.
func Boss() Character {
	return Character{
		Name:         "Босс",
		History:      "Ты впервые встретился с Челом — человеком, который принёс заказ. Это начало твоей истории в этом заказе; не выдумывай прошлые встречи и выполненную работу.",
		Instructions: "Отвечай за понимание цели и целостный результат. Сам выбирай необходимые исследования и действия внутри заказа. Если данных недостаточно, сформулируй конкретные вопросы Челу. Сохраняй ограничения и права человека; не подменяй его цель удобной задачей. Помни свои решения, проверенные факты, вопросы и оставшиеся обязательства.",
	}
}

// validateCharacters проверяет и неиспользуемые описания: опечатка не должна
// оставаться скрытой до первого поручения этой личности. Пустой реестр совместим
// со всеми прежними workflow; ссылка шага всегда обязана разрешаться явно.
func (w Workflow) validateCharacters() error {
	for id, character := range w.Characters {
		if !characterID.MatchString(id) || strings.TrimSpace(character.Name) == "" ||
			strings.TrimSpace(character.History) == "" || strings.TrimSpace(character.Instructions) == "" ||
			!utf8.ValidString(character.Name+character.History+character.Instructions) {
			return fmt.Errorf("characters.%s: нужны безопасный ID, непустые name, history и instructions в UTF-8", id)
		}
	}
	for _, step := range w.Steps {
		if step.Character != "" {
			if _, exists := w.Characters[step.Character]; !exists {
				return fmt.Errorf("шаг %q: неизвестная личность %q", step.ID, step.Character)
			}
		}
	}
	return nil
}

// CharacterMemory возвращает относительный путь отдельной памяти личности.
// Префикс исключает совпадение с hex-ID памяти посещений. Вызывать после Validate.
func CharacterMemory(id string) string { return "memory/character-" + id + ".md" }

// CharacterPrompt сохраняет личность при смене поручений и посещений. История
// самого чата остаётся у Codex, а общая для посещений память — у Lawa. Факты из
// завершённых посещений дополнительно передаёт обычный контекст coordinator.
func (w Workflow) CharacterPrompt(step Step, memory string) string {
	if step.Character == "" {
		return ""
	}
	character := w.Characters[step.Character]
	return fmt.Sprintf("Личность: %s (characterId: %s).\nПредыстория:\n%s\nПринципы и границы:\n%s\n"+
		"Ты участник этого заказа, а кубик — твоё текущее поручение. Понимай его вклад в общую цель; исследуй и проверяй столько, сколько нужно для качественного результата в границах поручения.\n"+
		"Память личности: %s\nПрочитай её перед работой; сохраняй здесь приобретённый опыт, решения, результаты и открытые вопросы. Это не чужая память: тебе разрешено обновлять её вместе с памятью текущего исполнения. Не выдумывай историю.\n",
		character.Name, step.Character, character.History, character.Instructions, memory)
}
