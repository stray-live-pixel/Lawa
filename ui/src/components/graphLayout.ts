import {
  RESERVE,
  GAP,
  PITCH,
  LANE,
  round16,
  pair,
  nodeSize,
  simplify,
  type Point,
  type RoutedEdge,
  type LayoutGroup,
} from './graphGeometry';
import { denseLayout } from './graphDense';
export type { Point, Box, RoutedEdge, LayoutGroup } from './graphGeometry';
import { avoidObstacles } from './graphRouting';
import dagre from '@dagrejs/dagre';
import type { GraphEdge, GraphNode } from '../types';
import { graphLabel, type TextMeasure } from './graphLabel';

// Dagre определяет порядок рядов, но не рисует линии: его диагональные участки
// заменяет отдельная ортогональная маршрутизация. Обратные рёбра исключаются
// до ранжирования; их левые каналы не растягивают основной путь.
export function graphLayout(
  nodes: GraphNode[],
  edges: GraphEdge[],
  measure?: TextMeasure,
) {
  const byID = new Map(nodes.map((n) => [n.ID, n]));
  const merged = new Map<string, RoutedEdge>();
  for (const edge of edges) {
    if (!byID.has(edge.From) || !byID.has(edge.To)) continue;
    const key = pair(edge.From, edge.To);
    const item = merged.get(key) || {
      from: edge.From,
      to: edge.To,
      labels: [],
      members: [],
      points: [],
      label: { x: 0, y: 0 },
      text: graphLabel(''),
      feedback: false,
    };
    item.members.push(edge);
    if (edge.Label && !item.labels.includes(edge.Label))
      item.labels.push(edge.Label);
    merged.set(key, item);
  }
  const connections = [...merged.values()];
  for (const edge of connections)
    edge.text = graphLabel(edge.labels.join(' · '), measure);
  const visited = new Set<string>(),
    visiting = new Set<string>();
  function visit(id: string) {
    if (visited.has(id)) return;
    visiting.add(id);
    for (const edge of connections.filter((e) => e.from === id)) {
      if (visiting.has(edge.to)) edge.feedback = true;
      else visit(edge.to);
    }
    visiting.delete(id);
    visited.add(id);
  }
  // Явные старты важнее порядка steps и алфавита ключей решений.
  [...nodes.filter((n) => n.Marker === 'start' || n.Start), ...nodes].forEach(
    (n) => visit(n.ID),
  );
  const feedback = connections.filter((e) => e.feedback);
  const errorID = nodes.find((n) => n.Marker === 'failed')?.ID;
  const failures = connections.filter((e) => e.to === errorID);
  const sizes = new Map(
    nodes.map((n) => [
      n.ID,
      nodeSize(
        n,
        Math.max(
          feedback.filter((e) => e.from === n.ID).length +
            feedback.filter((e) => e.to === n.ID).length,
          failures.filter((e) => e.from === n.ID).length,
        ),
        measure,
      ),
    ]),
  );
  // Несколько подписанных выходов на одной грани требуют места для текста,
  // а не наложения подписей поверх соседних портов.
  const leftPitch = new Map(
    nodes.map((n) => {
      const outputs = feedback.filter((e) => e.from === n.ID);
      const inputCount = feedback.filter((e) => e.to === n.ID).length;
      const pitch =
        outputs.length && outputs.length + inputCount > 1
          ? Math.max(PITCH, ...outputs.map((e) => e.text.height + 14))
          : PITCH;
      const count = outputs.length + inputCount;
      const size = sizes.get(n.ID)!;
      if (!n.Marker)
        size.height =
          Math.ceil(Math.max(size.height, 32 + pitch * (count - 1)) / 4) * 4;
      return [n.ID, pitch];
    }),
  );
  const positions = new Map<string, Point>();
  const groups: LayoutGroup[] = [];
  if (!nodes.length) return { positions, sizes, groups, edges: connections };

  const dense = denseLayout(nodes, connections, sizes, measure);
  if (dense) return dense;

  const graph = new dagre.graphlib.Graph()
    .setGraph({
      rankdir: 'TB',
      nodesep: 64,
      ranksep: 70,
      marginx: 0,
      marginy: 0,
    })
    .setDefaultEdgeLabel(() => ({}));
  for (const node of nodes)
    if (node.ID !== errorID) {
      const size = sizes.get(node.ID)!;
      graph.setNode(node.ID, {
        width: size.width + 6,
        height: size.height + 6,
      });
    }
  for (const edge of connections)
    if (!edge.feedback && edge.to !== errorID)
      graph.setEdge(edge.from, edge.to, {});
  if (graph.nodeCount()) dagre.layout(graph);
  const rows = new Map<number, string[]>();
  for (const id of graph.nodes()) {
    const row = graph.node(id).y;
    rows.set(row, [...(rows.get(row) || []), id]);
  }
  let y = 0;
  const rowEntries = [...rows].sort((a, b) => a[0] - b[0]);
  rowEntries.forEach(([, ids]) => {
    const outgoing = connections.filter(
      (e) => ids.includes(e.from) && !e.feedback && e.to !== errorID,
    );
    const labelHeight = Math.max(0, ...outgoing.map((e) => e.text.height));
    const branchCount = Math.max(
      0,
      ...ids.map((id) => outgoing.filter((e) => e.from === id).length),
    );
    const height = Math.max(...ids.map((id) => sizes.get(id)!.height));
    for (const id of ids)
      positions.set(id, {
        x: graph.node(id).x - sizes.get(id)!.width / 2,
        y: y + (height - sizes.get(id)!.height) / 2,
      });
    y +=
      height +
      6 +
      round16(
        Math.max(
          GAP,
          labelHeight + (branchCount > 1 ? 40 : 20),
          35 +
            (branchCount > 1 ? labelHeight : 0) +
            (Math.ceil(branchCount / 2) - 1) * Math.max(LANE, labelHeight + 14),
        ),
      );
  });
  if (errorID) {
    const right = Math.max(
      0,
      ...[...positions].map(([id, p]) => p.x + sizes.get(id)!.width),
    );
    const labelWidth = Math.max(0, ...failures.map((e) => e.text.width));
    const gap = round16(
      Math.max(GAP, 12 + labelWidth + 8, 35 + (failures.length - 1) * LANE),
    );
    const bottom = Math.max(
      0,
      ...[...positions]
        .filter(([id]) => byID.get(id)?.Marker !== 'succeeded')
        .map(([id, p]) => p.y + sizes.get(id)!.height),
    );
    const terminalY =
      bottom + 6 + round16(Math.max(GAP, 35 + (failures.length - 1) * LANE));
    positions.set(errorID, { x: right + 6 + gap, y: terminalY });
    // Оба исхода находятся в одном терминальном ряду.
    for (const node of nodes)
      if (node.Marker === 'succeeded') positions.get(node.ID)!.y = terminalY;
  }

  // Короткие возвраты получают внутренние каналы. Внешний вход выше внутреннего:
  // горизонтальный подход не пересекает вертикаль более короткого возврата.
  feedback.sort(
    (a, b) =>
      Math.abs(positions.get(a.from)!.y - positions.get(a.to)!.y) -
        Math.abs(positions.get(b.from)!.y - positions.get(b.to)!.y) ||
      pair(a.from, a.to).localeCompare(pair(b.from, b.to)),
  );
  const leftPorts = new Map<string, { edge: RoutedEdge; source: boolean }[]>();
  for (const edge of feedback)
    for (const source of [true, false]) {
      const id = source ? edge.from : edge.to;
      leftPorts.set(id, [...(leftPorts.get(id) || []), { edge, source }]);
    }
  for (const ports of leftPorts.values())
    ports.sort(
      (a, b) =>
        Number(a.source) - Number(b.source) ||
        (a.source
          ? feedback.indexOf(a.edge) - feedback.indexOf(b.edge)
          : feedback.indexOf(b.edge) - feedback.indexOf(a.edge)),
    );
  const portY = (id: string, edge: RoutedEdge, source: boolean) => {
    const ports = leftPorts.get(id)!;
    return (
      positions.get(id)!.y +
      sizes.get(id)!.height / 2 +
      (ports.findIndex((p) => p.edge === edge && p.source === source) -
        (ports.length - 1) / 2) *
        leftPitch.get(id)!
    );
  };
  const lanes: { top: number; bottom: number; x: number }[] = [];
  for (const edge of feedback) {
    const a = positions.get(edge.from)!,
      b = positions.get(edge.to)!;
    const start = { x: a.x - RESERVE, y: portY(edge.from, edge, true) };
    const end = { x: b.x - RESERVE, y: portY(edge.to, edge, false) };
    const top = Math.min(start.y, end.y),
      bottom = Math.max(start.y, end.y);
    const obstacles = [...positions].filter(
      ([id, p]) => p.y <= bottom && p.y + sizes.get(id)!.height >= top,
    );
    let x =
      Math.min(start.x, end.x, ...obstacles.map(([, p]) => p.x - RESERVE)) -
      Math.max(18, 12 + edge.text.width + 8);
    for (const lane of lanes)
      if (lane.top <= bottom && lane.bottom >= top)
        x = Math.min(x, lane.x - LANE);
    lanes.push({ top, bottom, x });
    edge.points = simplify([start, { x, y: start.y }, { x, y: end.y }, end]);
    edge.label = {
      x: start.x - 12 - edge.text.width,
      y: start.y - 5.5 - edge.text.height,
    };
  }

  const forward = connections.filter((e) => !e.feedback && e.to !== errorID);
  for (const edge of forward) {
    const a = positions.get(edge.from)!,
      b = positions.get(edge.to)!;
    const as = sizes.get(edge.from)!,
      bs = sizes.get(edge.to)!;
    const outgoing = forward
      .filter((e) => e.from === edge.from)
      .sort((u, v) => positions.get(u.to)!.x - positions.get(v.to)!.x);
    const incoming = forward
      .filter((e) => e.to === edge.to)
      .sort((u, v) => positions.get(u.from)!.x - positions.get(v.from)!.x);
    const start = {
      x:
        a.x +
        as.width / 2 +
        (outgoing.indexOf(edge) - (outgoing.length - 1) / 2) * PITCH,
      y: a.y + as.height + RESERVE,
    };
    const end = {
      x:
        b.x +
        bs.width / 2 +
        (incoming.indexOf(edge) - (incoming.length - 1) / 2) * PITCH,
      y: b.y - RESERVE,
    };
    const maxHeight = Math.max(0, ...outgoing.map((e) => e.text.height));
    const fanLane = Math.min(
      outgoing.indexOf(edge),
      outgoing.length - 1 - outgoing.indexOf(edge),
    );
    const mid =
      start.y +
      Math.max(18, maxHeight + 18) +
      fanLane * Math.max(LANE, maxHeight + 14);
    edge.points = simplify([
      start,
      { x: start.x, y: mid },
      { x: end.x, y: mid },
      end,
    ]);
    edge.label =
      outgoing.length > 1 && Math.abs(end.x - start.x) >= edge.text.width + 20
        ? {
            x: end.x < start.x ? start.x - 12 - edge.text.width : start.x + 12,
            y: mid - 5.5 - edge.text.height,
          }
        : { x: start.x + 7.5, y: start.y + 12 };
  }
  failures.sort((a, b) => positions.get(b.from)!.y - positions.get(a.from)!.y);
  failures.forEach((edge, index) => {
    const a = positions.get(edge.from)!,
      b = positions.get(edge.to)!;
    const as = sizes.get(edge.from)!,
      bs = sizes.get(edge.to)!;
    const start = { x: a.x + as.width + RESERVE, y: a.y + as.height / 2 };
    const end = {
      x: b.x + bs.width / 2 + (index - (failures.length - 1) / 2) * PITCH,
      y: b.y - RESERVE,
    };
    const lane = b.x - 18 + index * LANE;
    const approach = end.y - 18 - index * LANE;
    edge.points = simplify([
      start,
      { x: lane, y: start.y },
      { x: lane, y: approach },
      { x: end.x, y: approach },
      end,
    ]);
    edge.label = { x: start.x + 12, y: start.y - 5.5 - edge.text.height };
  });
  avoidObstacles(
    connections,
    new Map([...positions].map(([id, p]) => [id, { ...p, ...sizes.get(id)! }])),
  );
  for (const edge of connections) edge.points = simplify(edge.points);
  return { positions, sizes, groups, edges: connections };
}
