package runstore

import "testing"

// Повреждённый список не должен удвоить интервалы либо упасть на nil при чтении.
func TestValidateTeamMetrics(t *testing.T) {
	for _, executions := range [][]*TeamExecution{
		{nil},
		{{ID: "a/1", ActorID: "a", Attempt: 1}, {ID: "a/1", ActorID: "a", Attempt: 1}},
		{{ID: "first", ActorID: "a", Attempt: 1}, {ID: "second", ActorID: "a", Attempt: 1}},
	} {
		if validateTeamMetrics(&TeamMetrics{Executions: executions}) == nil {
			t.Fatal("приняты повреждённые метрики")
		}
	}
	if err := validateTeamMetrics(nil); err != nil {
		t.Fatal(err)
	}
}
