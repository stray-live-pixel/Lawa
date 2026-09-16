import dagre from '@dagrejs/dagre';
import type { GraphEdge, GraphNode } from '../types';

export interface Point {
  x: number;
  y: number;
}
export interface RoutedEdge {
  from: string;
  to: string;
  labels: string[];
  points: Point[];
  label: Point;
  feedback: boolean;
}
export interface LayoutGroup {
  id: string;
  label: string;
  x: number;
  y: number;
  width: number;
  height: number;
}

// Геометрия совпадает с .cube. Полная топология — единственный вход: polling
// статусов не меняет порядок узлов, группы или масштаб. Имена шагов не анализируем.
export function graphLayout(nodes: GraphNode[], edges: GraphEdge[]) {
  const ids = [...new Set(nodes.map((n) => n.ID))];
  const known = new Set(ids);
  const topology = new dagre.graphlib.Graph().setGraph({});
  ids.forEach((id) => topology.setNode(id, {}));
  const merged = new Map<
    string,
    { from: string; to: string; labels: string[] }
  >();
  for (const e of edges) {
    // Повреждённая ссылка не создаёт невидимый узел нулевого размера в Dagre.
    if (!known.has(e.From) || !known.has(e.To)) continue;
    const key = JSON.stringify([e.From, e.To]);
    const item = merged.get(key) || { from: e.From, to: e.To, labels: [] };
    if (e.Label && !item.labels.includes(e.Label)) item.labels.push(e.Label);
    merged.set(key, item);
    topology.setEdge(e.From, e.To, {});
  }
  const connections = [...merged.values()];
  const feedback = new Set<string>();
  const visiting = new Set<string>();
  const visited = new Set<string>();
  // Обратные рёбра DFS выносим за схему до ранжирования. Это сохраняет прямое
  // чтение сверху вниз, включая self-loop, и исключает циклы из compound layout.
  function visit(id: string) {
    if (visited.has(id)) return;
    visiting.add(id);
    for (const next of topology.successors(id) || []) {
      if (visiting.has(next)) feedback.add(JSON.stringify([id, next]));
      else visit(next);
    }
    visiting.delete(id);
    visited.add(id);
  }
  ids.forEach(visit);
  const graph = new dagre.graphlib.Graph({ compound: true })
    .setGraph({
      rankdir: 'TB',
      nodesep: 45,
      edgesep: 25,
      ranksep: 65,
      marginx: 35,
      marginy: 45,
    })
    .setDefaultEdgeLabel(() => ({}));
  ids.forEach((id) => graph.setNode(id, { width: 220, height: 80 }));
  const groupNames = new Map<string, string>();
  // Сильно связная компонента — структурный цикл. Название не обещает бизнес-
  // смысл, которого нет в API. Отдельная конечная ветка остаётся за его рамкой.
  let serial = 0;
  function group(label: string) {
    let id: string;
    do {
      id = `__layout_group_${serial++}`;
    } while (known.has(id));
    graph.setNode(id, { label });
    groupNames.set(id, label);
    return id;
  }
  for (const component of dagre.graphlib.alg.tarjan(topology)) {
    if (component.length < 2) continue;
    const parent = group('Цикл');
    component.forEach((id) => graph.setParent(id, parent));
  }
  // Одинаковый набор входов/выходов означает структурно параллельные ветки.
  // Группируем только внутри одного цикла/контейнера, без пересекающихся рамок.
  const siblings = new Map<string, string[]>();
  for (const id of ids) {
    const before = (topology.predecessors(id) || []).sort();
    const after = (topology.successors(id) || []).sort();
    if (!before.length || !after.length) continue;
    const key = JSON.stringify([graph.parent(id), before, after]);
    siblings.set(key, [...(siblings.get(key) || []), id]);
  }
  for (const members of siblings.values()) {
    if (members.length < 2) continue;
    const parent = graph.parent(members[0]);
    const id = group(`Параллельные ветки · ${members.length}`);
    if (parent) graph.setParent(id, parent);
    members.forEach((member) => graph.setParent(member, id));
  }
  for (const e of connections) {
    if (feedback.has(JSON.stringify([e.from, e.to]))) continue;
    graph.setEdge(e.from, e.to, {
      width: e.labels.length ? 135 : 0,
      height: e.labels.length
        ? Math.ceil(e.labels.join(' · ').length / 20) * 16 + 10
        : 0,
      labelpos: 'c',
    });
  }
  if (ids.length) dagre.layout(graph);
  const positions = new Map(
    ids.map((id) => {
      const n = graph.node(id);
      return [id, { x: n.x - 110, y: n.y - 40 }];
    }),
  );
  const groups: LayoutGroup[] = [...groupNames].map(([id, label]) => {
    const n = graph.node(id);
    return {
      id,
      label,
      x: n.x - n.width / 2,
      y: n.y - n.height / 2,
      width: n.width,
      height: n.height,
    };
  });
  const right = Math.max(
    0,
    ...ids.map((id) => positions.get(id)!.x + 220),
    ...groups.map((g) => g.x + g.width),
  );
  let lane = 0;
  const routed: RoutedEdge[] = connections.map((e) => {
    const back = feedback.has(JSON.stringify([e.from, e.to]));
    if (back) {
      const source = positions.get(e.from)!;
      const target = positions.get(e.to)!;
      const x = right + 55 + lane++ * 55;
      return {
        ...e,
        feedback: true,
        points:
          e.from === e.to
            ? [
                { x: source.x + 220, y: source.y + 40 },
                { x, y: source.y + 40 },
                { x, y: source.y - 25 },
                { x: source.x + 110, y: source.y - 25 },
                { x: source.x + 110, y: source.y },
              ]
            : [
                { x: source.x + 220, y: source.y + 40 },
                { x, y: source.y + 40 },
                { x, y: target.y + 40 },
                { x: target.x + 220, y: target.y + 40 },
              ],
        label: { x, y: (source.y + target.y) / 2 + 40 },
      };
    }
    const route = graph.edge(e.from, e.to);
    const points = route.points as Point[];
    return {
      ...e,
      feedback: false,
      points,
      label: {
        x: route.x ?? points[Math.floor(points.length / 2)].x,
        y: route.y ?? points[Math.floor(points.length / 2)].y,
      },
    };
  });
  return { positions, groups, edges: routed };
}
