import type { GraphNode } from '../types';
import type { TextMeasure } from './graphLabel';
import {
  RESERVE,
  GAP,
  PITCH,
  LANE,
  round16,
  nodeSize,
  simplify,
  type Point,
  type RoutedEdge,
  type LayoutGroup,
} from './graphGeometry';

// Плотная группа с одним общим кубиком получает боковые порты и минимальные
// каналы. undefined передаёт произвольную топологию обычной послойной раскладке.
export function denseLayout(
  nodes: GraphNode[],
  connections: RoutedEdge[],
  sizes: Map<string, { width: number; height: number }>,
  measure?: TextMeasure,
):
  | {
      positions: Map<string, Point>;
      sizes: Map<string, { width: number; height: number }>;
      groups: LayoutGroup[];
      edges: RoutedEdge[];
    }
  | undefined {
  const byID = new Map(nodes.map((n) => [n.ID, n]));
  const feedback = connections.filter((e) => e.feedback);
  const positions = new Map<string, Point>();
  const groups: LayoutGroup[] = [];
  // Плотный веер/схождение располагается по горизонтали: высота общего кубика
  // растёт под боковые порты, независимые получатели имеют зазор 24 px.
  const plain = nodes.filter((n) => !n.Marker);
  const internal = connections.filter(
    (e) => !byID.get(e.from)?.Marker && !byID.get(e.to)?.Marker,
  );
  const fanOut = plain.find(
    (n) => internal.filter((e) => e.from === n.ID).length === plain.length - 1,
  );
  const fanIn = plain.find(
    (n) => internal.filter((e) => e.to === n.ID).length === plain.length - 1,
  );
  const mixedHub =
    !feedback.length && internal.length === plain.length - 1
      ? plain.find(
          (n) =>
            internal.filter((e) => e.from === n.ID || e.to === n.ID).length ===
              plain.length - 1 &&
            internal.some((e) => e.from === n.ID) &&
            internal.some((e) => e.to === n.ID) &&
            internal.filter((e) => e.to === n.ID).length >= 3,
        )
      : undefined;
  if (mixedHub) {
    const incoming = internal.filter((e) => e.to === mixedHub.ID);
    const outgoing = internal.filter((e) => e.from === mixedHub.ID);
    const leftNodes = plain.filter(
      (n) => n.ID === mixedHub.ID || incoming.some((e) => e.from === n.ID),
    );
    const left = denseLayout(
      leftNodes,
      incoming,
      new Map(leftNodes.map((n) => [n.ID, { ...sizes.get(n.ID)! }])),
      measure,
    )!;
    for (const [id, p] of left.positions) positions.set(id, p);
    for (const [id, size] of left.sizes) sizes.set(id, size);
    const hub = positions.get(mixedHub.ID)!,
      hubSize = sizes.get(mixedHub.ID)!;
    hubSize.height = Math.max(
      hubSize.height,
      nodeSize(mixedHub, outgoing.length).height,
    );
    const labelWidth = Math.max(0, ...outgoing.map((e) => e.text.width));
    const gap = round16(
      Math.max(
        GAP,
        35 + (Math.ceil(outgoing.length / 2) - 1) * LANE,
        labelWidth ? labelWidth + 20 : 0,
      ),
    );
    outgoing.forEach((edge, index) => {
      const target = {
        x: hub.x + 226 + gap,
        y:
          hub.y +
          hubSize.height / 2 +
          (index - (outgoing.length - 1) / 2) * 110 -
          40,
      };
      positions.set(edge.to, target);
      const start = {
        x: hub.x + 223,
        y:
          hub.y +
          hubSize.height / 2 +
          (index - (outgoing.length - 1) / 2) * PITCH,
      };
      const end = { x: target.x - 3, y: target.y + 40 };
      const x =
        start.x +
        Math.max(18, labelWidth + 20) +
        Math.min(index, outgoing.length - 1 - index) * LANE;
      edge.points = simplify([start, { x, y: start.y }, { x, y: end.y }, end]);
      edge.label = { x: start.x + 12, y: start.y - 5.5 - edge.text.height };
    });
    for (const edge of incoming) {
      const routed = left.edges.find(
        (e) => e.from === edge.from && e.to === edge.to,
      )!;
      edge.points = routed.points;
      edge.label = routed.label;
    }
    denseMarkers(nodes, connections, positions, sizes);
    return { positions, sizes, groups, edges: connections };
  }
  const denseHub =
    plain.length >= 4 &&
    internal.length === plain.length - 1 &&
    !feedback.length
      ? fanOut || fanIn
      : undefined;
  if (denseHub) {
    const outgoing = denseHub === fanOut;
    const members = plain.filter((n) => n.ID !== denseHub.ID);
    const maxLabelHeight = Math.max(0, ...internal.map((e) => e.text.height));
    const portPitch =
      outgoing && maxLabelHeight ? Math.max(PITCH, maxLabelHeight + 10) : PITCH;
    sizes.set(denseHub.ID, {
      width: 220,
      height:
        Math.ceil(
          Math.max(
            sizes.get(denseHub.ID)!.height,
            32 + portPitch * (members.length - 1),
          ) / 4,
        ) * 4,
    });
    const labelWidth = Math.max(0, ...internal.map((e) => e.text.width));
    const stub = Math.max(18, labelWidth ? 12 + labelWidth + 8 : 0);
    const gap = round16(
      Math.max(GAP, stub + 17 + (Math.ceil(members.length / 2) - 1) * LANE),
    );
    let stackHeight = 0;
    const offsets = new Map(
      members.map((n) => {
        const offset = stackHeight;
        stackHeight += sizes.get(n.ID)!.height + 30;
        return [n.ID, offset];
      }),
    );
    const height = stackHeight - 30;
    const hub = {
      x: outgoing ? 0 : 226 + gap,
      y: (height - sizes.get(denseHub.ID)!.height) / 2,
    };
    positions.set(denseHub.ID, hub);
    members.forEach((node) =>
      positions.set(node.ID, {
        x: outgoing ? 226 + gap : 0,
        y: offsets.get(node.ID)!,
      }),
    );
    internal.forEach((edge) => {
      const other = outgoing ? edge.to : edge.from;
      const index = members.findIndex((n) => n.ID === other);
      const a = positions.get(edge.from)!,
        b = positions.get(edge.to)!;
      const ah = sizes.get(edge.from)!.height,
        bh = sizes.get(edge.to)!.height;
      const hubY =
        hub.y +
        sizes.get(denseHub.ID)!.height / 2 +
        (index - (members.length - 1) / 2) * portPitch;
      const start = {
        x: a.x + 220 + RESERVE,
        y: outgoing ? hubY : a.y + ah / 2,
      };
      const end = { x: b.x - RESERVE, y: outgoing ? b.y + bh / 2 : hubY };
      const lane = outgoing
        ? start.x + stub + Math.min(index, members.length - 1 - index) * LANE
        : end.x - 18 - Math.min(index, members.length - 1 - index) * LANE;
      edge.points = simplify([
        start,
        { x: lane, y: start.y },
        { x: lane, y: end.y },
        end,
      ]);
      edge.label = { x: start.x + 12, y: start.y - 5.5 - edge.text.height };
    });
    denseMarkers(nodes, connections, positions, sizes);
    return { positions, sizes, groups, edges: connections };
  }

  return undefined;
}

// Общие маркеры плотного блока находятся перед первой и после последней группы.
// Несколько стартов/финишей делят магистраль, но сохраняют отдельные наконечники.
function denseMarkers(
  nodes: GraphNode[],
  edges: RoutedEdge[],
  positions: Map<string, Point>,
  sizes: Map<string, { width: number; height: number }>,
) {
  const plain = nodes.filter((n) => !n.Marker);
  if (!plain.length) return;
  const right = Math.max(
    ...plain.map((n) => positions.get(n.ID)!.x + sizes.get(n.ID)!.width),
  );
  const bottom = Math.max(
    ...plain.map((n) => positions.get(n.ID)!.y + sizes.get(n.ID)!.height),
  );
  for (const marker of nodes.filter((n) => n.Marker)) {
    const starts = marker.Marker === 'start';
    const linked = edges
      .filter((e) => (starts ? e.from === marker.ID : e.to === marker.ID))
      .map((e) => (starts ? e.to : e.from));
    if (linked.length === 1) {
      const p = positions.get(linked[0])!,
        size = sizes.get(linked[0])!;
      positions.set(
        marker.ID,
        starts
          ? { x: p.x + size.width / 2 - 80, y: p.y - 110 }
          : { x: p.x + size.width / 2 - 80, y: p.y + size.height + 70 },
      );
    } else if (linked.length) {
      const points = linked.map((id) => ({
        ...positions.get(id)!,
        ...sizes.get(id)!,
      }));
      const centre =
        (Math.min(...points.map((p) => p.y)) +
          Math.max(...points.map((p) => p.y + p.height))) /
        2;
      positions.set(marker.ID, {
        x: starts ? Math.min(...points.map((p) => p.x)) - 230 : right + 70,
        y: centre - 20,
      });
    } else positions.set(marker.ID, { x: right + 70, y: bottom + 70 });
  }
  const markers = new Set(nodes.filter((n) => n.Marker).map((n) => n.ID));
  for (const edge of edges.filter(
    (e) => markers.has(e.from) || markers.has(e.to),
  )) {
    const a = positions.get(edge.from)!,
      b = positions.get(edge.to)!,
      as = sizes.get(edge.from)!,
      bs = sizes.get(edge.to)!;
    if (Math.abs(a.x + as.width / 2 - b.x - bs.width / 2) < 1) {
      edge.points = [
        { x: a.x + as.width / 2, y: a.y + as.height + 3 },
        { x: b.x + bs.width / 2, y: b.y - 3 },
      ];
      edge.label = { x: edge.points[0].x + 7.5, y: edge.points[0].y + 12 };
    } else {
      const start = { x: a.x + as.width + 3, y: a.y + as.height / 2 },
        end = { x: b.x - 3, y: b.y + bs.height / 2 };
      const x = markers.has(edge.from) ? end.x - 24 : start.x + 24;
      edge.points = simplify([start, { x, y: start.y }, { x, y: end.y }, end]);
      edge.label = { x: start.x + 12, y: start.y - edge.text.height - 5.5 };
    }
  }
}
