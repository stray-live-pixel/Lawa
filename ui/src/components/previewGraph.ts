import type { Graph, Run } from '../types';

// Макет не обращается к реальному runstore и не выдаёт пример за реальную историю.
export function previewGraph(run: Run): Graph {
  return {
    ID: run.ID,
    Name: run.Name,
    State: run.State,
    StopReason: run.StopReason,
    Prompt:
      'Демонстрационный workflow. Выберите сохранённый запуск для продолжения.',
    Nodes: (run.Steps || []).map((step) => ({
      ID: step.StepID || step.ID,
      Prompt: 'Демонстрационный кубик.',
      Routes: [],
    })),
    Edges: [],
    Executions: (run.Steps || []).map((step) => ({
      Key: step.Key,
      StepID: step.StepID || step.ID,
      State: step.State,
      Result: '',
      Note: step.Message || 'Демонстрационные данные.',
      Decision: step.Decision,
      Trigger: step.Trigger,
      TraceURL: '',
      MemoryURL: '',
      Prompt: 'Демонстрационный кубик.',
      Visit: step.Visit,
      Attempt: step.Attempt,
    })),
  };
}
