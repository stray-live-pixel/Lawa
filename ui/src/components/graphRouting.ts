import type { Box, Point, RoutedEdge } from './graphGeometry';

// Прямой отрезок пересекает открытый прямоугольник. Касание границы допустимо:
// вызывающий уже включил в размеры препятствия толщину штриха и безопасный зазор.
export function hitsBox(a: Point, b: Point, box: Box) {
  return (
    Math.max(a.x, b.x) > box.x &&
    Math.min(a.x, b.x) < box.x + box.width &&
    Math.max(a.y, b.y) > box.y &&
    Math.min(a.y, b.y) < box.y + box.height
  );
}
function intersects(a: Point, b: Point, c: Point, d: Point) {
  const cross = (p: Point, q: Point, r: Point, s: Point) =>
    p.y === q.y &&
    r.x === s.x &&
    r.x >= Math.min(p.x, q.x) &&
    r.x <= Math.max(p.x, q.x) &&
    p.y >= Math.min(r.y, s.y) &&
    p.y <= Math.max(r.y, s.y);
  return cross(a, b, c, d) || cross(c, d, a, b);
}
export function pathsCross(a: Point[], b: Point[]) {
  for (let i = 1; i < a.length; i++)
    for (let j = 1; j < b.length; j++)
      if (intersects(a[i - 1], a[i], b[j - 1], b[j])) return true;
  return false;
}
interface SearchState {
  index: number;
  axis: number;
  cost: number;
  estimate: number;
  previous?: SearchState;
}
// Минимальная очередь A*: без квадратичной сортировки всей сетки на каждом шаге.
class Queue {
  items: SearchState[] = [];
  push(value: SearchState) {
    let index = this.items.length;
    this.items.push(value);
    while (index > 0) {
      const parent = (index - 1) >> 1;
      if (this.items[parent].estimate <= value.estimate) break;
      this.items[index] = this.items[parent];
      index = parent;
    }
    this.items[index] = value;
  }
  pop() {
    const first = this.items[0],
      last = this.items.pop();
    if (this.items.length && last) {
      let i = 0;
      while (i * 2 + 1 < this.items.length) {
        let child = i * 2 + 1;
        if (
          child + 1 < this.items.length &&
          this.items[child + 1].estimate < this.items[child].estimate
        )
          child++;
        if (this.items[child].estimate >= last.estimate) break;
        this.items[i] = this.items[child];
        i = child;
      }
      this.items[i] = last;
    }
    return first;
  }
}

// Сетка видимости содержит только границы препятствий и координаты портов.
// Стоимость сначала минимизирует длину, затем число поворотов. Стабильный порядок
// координат обеспечивает одинаковый результат при polling и выборе посещения.
function shortest(
  start: Point,
  end: Point,
  boxes: Box[],
  other: Point[][],
  preferred: Point[],
): Point[] | undefined {
  const xs = [
    ...new Set([
      start.x,
      end.x,
      ...preferred.map((p) => p.x),
      ...boxes.flatMap((b) => [b.x, b.x + b.width]),
    ]),
  ].sort((a, b) => a - b);
  const ys = [
    ...new Set([
      start.y,
      end.y,
      ...preferred.map((p) => p.y),
      ...boxes.flatMap((b) => [b.y, b.y + b.height]),
    ]),
  ].sort((a, b) => a - b);
  const width = xs.length;
  const point = (index: number) => ({
    x: xs[index % width],
    y: ys[Math.floor(index / width)],
  });
  const initial = ys.indexOf(start.y) * width + xs.indexOf(start.x),
    goal = ys.indexOf(end.y) * width + xs.indexOf(end.x);
  const queue = new Queue(),
    best = new Map<string, number>();
  queue.push({ index: initial, axis: 0, cost: 0, estimate: 0 });
  while (queue.items.length) {
    const current = queue.pop()!;
    if (
      current.cost > (best.get(`${current.index}:${current.axis}`) ?? Infinity)
    )
      continue;
    if (current.index === goal) {
      const result: Point[] = [];
      let item: SearchState | undefined = current;
      while (item) {
        result.push(point(item.index));
        item = item.previous;
      }
      return result.reverse();
    }
    const a = point(current.index),
      x = current.index % width,
      y = Math.floor(current.index / width);
    for (const [dx, dy, axis] of [
      [-1, 0, 1],
      [1, 0, 1],
      [0, -1, 2],
      [0, 1, 2],
    ]) {
      const nx = x + dx,
        ny = y + dy;
      if (nx < 0 || nx >= xs.length || ny < 0 || ny >= ys.length) continue;
      const index = ny * width + nx,
        b = point(index);
      if (boxes.some((box) => hitsBox(a, b, box))) continue;
      if (other.some((path) => pathsCross([a, b], path))) continue;
      const cost =
        current.cost +
        (Math.abs(a.x - b.x) + Math.abs(a.y - b.y)) * 1000 +
        (current.axis && current.axis !== axis ? 1 : 0);
      const key = `${index}:${axis}`;
      if (cost >= (best.get(key) ?? Infinity)) continue;
      best.set(key, cost);
      queue.push({
        index,
        axis,
        cost,
        estimate: cost + (Math.abs(b.x - end.x) + Math.abs(b.y - end.y)) * 1000,
        previous: current,
      });
    }
  }
  return undefined;
}

// Локальные каналы дают компактный нормальный случай. Если длинная связь
// пересекает промежуточный кубик или чужую подпись, прокладываем обход по их
// границам. Собственные порты имеют прямые участки 18px и не обходятся изнутри.
export function avoidObstacles(edges: RoutedEdge[], nodes: Map<string, Box>) {
  const routed: RoutedEdge[] = [];
  for (const edge of [...edges].sort(
    (a, b) => Number(b.feedback) - Number(a.feedback),
  )) {
    const boxes = [...nodes.values()].map((b) => ({
      x: b.x - 20.5,
      y: b.y - 20.5,
      width: b.width + 41,
      height: b.height + 41,
    }));
    const labels = edges
      .filter((e) => e.text.width)
      .map((e) => {
        const pad = e === edge ? 5.5 : 9.5;
        return {
          x: e.label.x - pad,
          y: e.label.y - pad,
          width: e.text.width + pad * 2,
          height: e.text.height + pad * 2,
        };
      });
    const other = routed
      .filter((e) => e.from !== edge.from && e.to !== edge.to)
      .map((e) => e.points);
    const collides =
      edge.points.some(
        (p, i) =>
          i > 0 &&
          [...nodes].some(
            ([id, box]) =>
              id !== edge.from &&
              id !== edge.to &&
              hitsBox(edge.points[i - 1], p, {
                x: box.x - 20.5,
                y: box.y - 20.5,
                width: box.width + 41,
                height: box.height + 41,
              }),
          ),
      ) ||
      edge.points.some(
        (p, i) =>
          i > 0 && labels.some((box) => hitsBox(edge.points[i - 1], p, box)),
      ) ||
      other.some((p) => pathsCross(edge.points, p));
    if (collides && edge.points.length >= 2) {
      const first = edge.points[0],
        second = edge.points[1],
        last = edge.points.at(-1)!,
        previous = edge.points.at(-2)!;
      const start = {
        x: first.x + Math.sign(second.x - first.x) * 18,
        y: first.y + Math.sign(second.y - first.y) * 18,
      };
      const end = {
        x: last.x - Math.sign(last.x - previous.x) * 18,
        y: last.y - Math.sign(last.y - previous.y) * 18,
      };
      const path = shortest(
        start,
        end,
        [...boxes, ...labels],
        other,
        edge.points,
      );
      if (path) edge.points = [first, ...path, last];
    }
    routed.push(edge);
  }
}
