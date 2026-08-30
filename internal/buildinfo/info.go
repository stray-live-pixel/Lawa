// Package buildinfo хранит версию текущей сборки Lawa.
//
// Release workflow задаёт Version через go build -ldflags -X. Все остальные
// потребители — CLI, самообновление и clientInfo протокола Codex — читают одно
// значение отсюда, поэтому в собранном бинарнике не может появиться несколько
// расходящихся версий. Локальная сборка намеренно остаётся dev.
package buildinfo

import "strings"

// Version заменяется release workflow значением вида vMAJOR.MINOR.PATCH.
// Переменная экспортирована, потому что полный путь к ней является контрактом
// флага linker -X; присваивать её во время работы приложения нельзя.
var Version = "dev"

// CodexVersion возвращает ту же версию без необязательного префикса v.
// app-server ожидает обычную строку версии клиента, тогда как пользовательский
// CLI и GitHub Release используют тег с v. Это представления одного источника,
// а не две независимо поддерживаемые константы.
func CodexVersion() string {
	return strings.TrimPrefix(Version, "v")
}
