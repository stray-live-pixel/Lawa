package coordinator

import (
	"path/filepath"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// characterPermissions добавляет только память назначенной личности. Права на
// проект и read-only доступ к run остаются прежними; другие личности недоступны
// для записи. ID уже проверен workflow.Validate и не может выйти из memory.
func characterPermissions(w workflow.Workflow, step workflow.Step, runDir, ownMemory, executionID string) *codex.PermissionProfile {
	profile := stepPermissions(runDir, ownMemory, executionID)
	if step.Character != "" {
		profile.WritePaths = append(profile.WritePaths, filepath.Join(runDir, workflow.CharacterMemory(step.Character)))
	}
	return profile
}
