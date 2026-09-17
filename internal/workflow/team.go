package workflow

import "fmt"

// DefaultTeamCharacters сохраняет прежнюю команду при создании без конфига.
// Возвращается новая карта: настройки одного заказа не меняют следующие.
func DefaultTeamCharacters() map[string]Character {
	boss := Boss()
	boss.Avatar = "boss"
	return map[string]Character{
		"boss":      boss,
		"developer": {Name: "Разработчик", History: "Опытный инженер, который исследует, реализует и проверяет решения.", Instructions: "Выполняй поручение в границах общей цели. Проверяй результат и сообщай о препятствиях Боссу.", Avatar: "developer"},
	}
}

// ValidTeamID допускает безопасный компонент имени lock-файла и адрес @id.
// human и system принадлежат человеку и системным событиям, а не агентам.
func ValidTeamID(id string) bool {
	return characterID.MatchString(id) && id != "human" && id != "system"
}

// ValidateTeamCharacters проверяет офисный реестр отдельно от фиксированных
// workflow. Босс обязателен, остальные роли произвольны. Внешность — ID, не URL;
// неизвестный безопасный ID UI покажет стандартным образом сотрудника.
func ValidateTeamCharacters(characters map[string]Character) error {
	if len(characters) > 20 {
		return fmt.Errorf("команда: не более 20 личностей, включая Босса")
	}
	if _, ok := characters["boss"]; !ok {
		return fmt.Errorf("команда: отсутствует boss")
	}
	if err := (Workflow{Characters: characters}).validateCharacters(); err != nil {
		return err
	}
	for id, c := range characters {
		if !ValidTeamID(id) {
			return fmt.Errorf("команда: недопустимый ID %q", id)
		}
		if c.Avatar != "" && !characterID.MatchString(c.Avatar) {
			return fmt.Errorf("characters.%s.avatar: нужен ID внешности из галереи", id)
		}
	}
	return nil
}
