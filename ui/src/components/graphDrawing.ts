import type { Point, RoutedEdge } from './graphGeometry';
export interface GraphDrawing {
  route: RoutedEdge;
  active: boolean;
  sourceSelected: boolean;
  targetSelected: boolean;
  path?: string;
  arrow?: string;
}
interface Segment {
  a: Point;
  b: Point;
  owner: number;
}

// Резерв рамки всегда 3px, видимая рамка — 1px либо 3px. Двигаем только
// крайние точки, а ствол обрываем у основания непрозрачного наконечника 8×8.
export function arrowGeometry(item: GraphDrawing) {
  const points = item.route.points.map((p) => ({ ...p }));
  const first = points[0],
    next = points[1],
    tip = points.at(-1)!,
    previous = points.at(-2)!;
  const sx = Math.sign(next.x - first.x),
    sy = Math.sign(next.y - first.y);
  const dx = Math.sign(tip.x - previous.x),
    dy = Math.sign(tip.y - previous.y);
  first.x -= sx * (item.sourceSelected ? 0 : 2);
  first.y -= sy * (item.sourceSelected ? 0 : 2);
  tip.x += dx * (item.targetSelected ? 0 : 2);
  tip.y += dy * (item.targetSelected ? 0 : 2);
  const base = { x: tip.x - dx * 8, y: tip.y - dy * 8 };
  points[points.length - 1] = base;
  return {
    points,
    arrow: `${tip.x},${tip.y} ${base.x - dy * 4},${base.y + dx * 4} ${base.x + dy * 4},${base.y - dx * 4}`,
  };
}

// Общий ствол рисуется один раз. Разрезаем совпадающие прямые по всем концам
// отрезков и отдаём каждый интервал одной связи; подтверждённая причина имеет
// приоритет. Это не меняет геометрию или причинность остальных веток.
export function graphDrawing(items: GraphDrawing[]): GraphDrawing[] {
  const result = items.map((item) => ({ ...item, path: '', arrow: '' }));
  const axes = new Map<string, Segment[]>();
  result.forEach((item, owner) => {
    if (item.route.points.length < 2) return;
    const shape = arrowGeometry(item);
    item.arrow = shape.arrow;
    for (let i = 1; i < shape.points.length; i++) {
      const a = shape.points[i - 1],
        b = shape.points[i];
      const key = a.x === b.x ? `x:${a.x}` : `y:${a.y}`;
      axes.set(key, [...(axes.get(key) || []), { a, b, owner }]);
    }
  });
  for (const [axis, segments] of axes) {
    const vertical = axis.startsWith('x:');
    const coordinate = (p: Point) => (vertical ? p.y : p.x);
    const stops = [
      ...new Set(segments.flatMap((s) => [coordinate(s.a), coordinate(s.b)])),
    ].sort((a, b) => a - b);
    for (let i = 1; i < stops.length; i++) {
      const low = stops[i - 1],
        high = stops[i];
      const candidates = segments.filter(
        (s) =>
          Math.min(coordinate(s.a), coordinate(s.b)) <= low &&
          Math.max(coordinate(s.a), coordinate(s.b)) >= high,
      );
      candidates.sort(
        (a, b) =>
          Number(result[b.owner].active) - Number(result[a.owner].active) ||
          a.owner - b.owner,
      );
      const winner = candidates[0];
      if (!winner) continue;
      result[winner.owner].path += vertical
        ? `M ${winner.a.x},${low} L ${winner.a.x},${high} `
        : `M ${low},${winner.a.y} L ${high},${winner.a.y} `;
    }
  }
  return result;
}
