package coordinator

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// TestCurrentAgentStatusIncludesStaticAndDurableDetails проверяет границу между
// snapshot и интерфейсами: статус получает причинность visit, лимит и все
// статические маршруты в стабильном порядке, не возвращая указатели хранилища.
func TestCurrentAgentStatusIncludesStaticAndDurableDetails(t *testing.T) {
	_, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"status","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"maxVisits":2,"onLimit":"failed","decisions":{
      "stop":{"finish":"succeeded"},"again":{"to":["choice"]}
    }}
  ]}`)
	status, signature, err := currentStatus(run)
	if err != nil {
		t.Fatal(err)
	}
	visit := status.Steps[0]
	succeeded := workflow.OutcomeSucceeded
	if status.RunState != runstore.RunRunning || status.Terminal || status.StopReason != "" || status.StopVisitID != "" ||
		visit.VisitID != initial.Meta.Visits[0].VisitID || visit.Visit != 1 || visit.Iteration != 1 || visit.Attempt != 0 ||
		visit.Trigger.Kind != runstore.TriggerStart || visit.MaxVisits == nil || *visit.MaxVisits != 2 ||
		visit.OnLimit == nil || *visit.OnLimit != workflow.OutcomeFailed ||
		!reflect.DeepEqual(visit.DecisionRoutes, []DecisionRouteStatus{
			{Key: "again", To: []string{"choice"}}, {Key: "stop", Finish: &succeeded},
		}) {
		t.Fatalf("статус v4 потерял поля посещения: %+v, signature=%q", status, signature)
	}
	for _, fragment := range []string{"max=2", "limit=failed", `route="again":["choice"]:`, `route="stop":[]:succeeded`} {
		if !strings.Contains(signature, fragment) {
			t.Fatalf("сигнатура не учитывает %q по значению: %q", fragment, signature)
		}
	}
}
