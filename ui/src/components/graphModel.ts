import type { Execution, Graph, GraphEdge, GraphNode } from '../types';

// Отмена pending не доказывает старт. Attempt>0 сохраняет факт запуска после
// отмены/ошибки; активное и успешное состояния покрывают старые DTO без Attempt.
function hasStarted(execution: Execution) {
  return (
    !['pending', 'skipped', 'not_started'].includes(execution.State) &&
    (execution.Attempt > 0 ||
      ['starting', 'running', 'waiting_for_approval', 'succeeded'].includes(
        execution.State,
      ))
  );
}

// Служебные ID выбираются вне пространства пользовательских шагов. Маркеры
// существуют только в представлении и никогда не становятся исполняемыми шагами.
export function graphTopology(
  graph: Pick<Graph, 'Nodes' | 'Edges' | 'Version'>,
) {
  const nodes = [...(graph.Nodes || [])];
  const edges = [...(graph.Edges || [])];
  const ids = new Set(nodes.map((n) => n.ID));
  function marker(kind: NonNullable<GraphNode['Marker']>) {
    let id = `@lawa/${kind}`;
    while (ids.has(id)) id += '/';
    ids.add(id);
    nodes.push({ ID: id, Marker: kind, Prompt: '', Routes: [] });
    return id;
  }
  if (!nodes.length) return { nodes, edges };
  const start = marker('start');
  const done = marker('succeeded');
  const failed = marker('failed');
  for (const node of graph.Nodes || []) {
    if (
      node.Start ||
      (graph.Version !== 2 && !edges.some((e) => e.To === node.ID))
    )
      edges.push({ From: start, To: node.ID, Label: '' });
    if (!edges.some((e) => e.From === node.ID))
      edges.push({ From: node.ID, To: done, Label: '' });
  }
  return {
    nodes,
    edges: edges.map((edge) =>
      edge.Finish
        ? { ...edge, To: edge.Finish === 'succeeded' ? done : failed }
        : edge,
    ),
  };
}

// Цвет причинного ребра доказывается сохранённым trigger выбранного visit.
// Pending/skipped ещё не означают запуск агента; старые снимки без Cause нейтральны.
export function isCause(
  edge: GraphEdge,
  shown: Execution | undefined,
  executions: Execution[],
  sourceMarker?: string,
) {
  if (!shown?.Cause || !hasStarted(shown)) return false;
  const cause = shown.Cause;
  if (sourceMarker === 'start') return cause.kind === 'start';
  if (cause.kind !== 'after' && cause.kind !== 'decision') return false;
  if (cause.kind === 'decision' && edge.Key !== cause.decisionKey) return false;
  if (cause.kind === 'after' && edge.Key) return false;
  return (cause.sourceVisitIds || []).some((id) =>
    executions.some((visit) => visit.Key === id && visit.StepID === edge.From),
  );
}

// Старт подтверждается фактическим исполнением, а не созданием pending visits.
export function markerState(kind: string, graph: Graph) {
  if (graph.Definition) return 'not_started';
  if (kind === 'start')
    return (graph.Executions || []).some(hasStarted)
      ? 'running'
      : 'not_started';
  return graph.State === kind ? kind : 'not_started';
}
