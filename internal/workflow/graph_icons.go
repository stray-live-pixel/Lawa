package workflow

import (
	_ "embed"
	"strings"
)

// graphIconCatalog содержит имена экспортов установленного @gravity-ui/icons.
// Генератор ui/scripts/generate-graph-icons.mjs обновляет каталог при обновлении пакета.
//
//go:embed graph-icons.txt
var graphIconCatalog string

var graphIcons = func() map[string]bool {
	result := make(map[string]bool)
	for _, name := range strings.Fields(graphIconCatalog) {
		result[name] = true
	}
	return result
}()
