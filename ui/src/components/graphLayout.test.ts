import { describe, expect, it } from 'vitest';
import { graphLayout, type RoutedEdge, type Point } from './graphLayout';
import { graphLabel } from './graphLabel';
import type { GraphNode, GraphEdge } from '../types';
const node = (ID: string): GraphNode => ({ ID, Prompt: '', Routes: [] });
const edge = (From: string, To: string, Label = ''): GraphEdge => ({
  From,
  To,
  Label,
});

// Проверяем геометрию, а не конкретный снимок координат: перпендикулярные
// участки разных связей не должны пересекаться внутри своих отрезков.
function crossings(edges: RoutedEdge[]) {
  const failures: string[] = [];
  for (let i = 0; i < edges.length; i++)
    for (let j = i + 1; j < edges.length; j++) {
      const a = edges[i],
        b = edges[j];
      for (let ai = 1; ai < a.points.length; ai++)
        for (let bi = 1; bi < b.points.length; bi++) {
          const p = a.points[ai - 1],
            q = a.points[ai],
            r = b.points[bi - 1],
            s = b.points[bi];
          const intersects = (p: Point, q: Point, r: Point, s: Point) =>
            p.y === q.y &&
            r.x === s.x &&
            r.x > Math.min(p.x, q.x) &&
            r.x < Math.max(p.x, q.x) &&
            p.y > Math.min(r.y, s.y) &&
            p.y < Math.max(r.y, s.y);
          if (intersects(p, q, r, s) || intersects(r, s, p, q))
            failures.push(`${a.from}→${a.to} × ${b.from}→${b.to}`);
        }
    }
  return failures;
}
function orthogonal(edges: RoutedEdge[]) {
  for (const e of edges)
    for (let i = 1; i < e.points.length; i++)
      expect(
        e.points[i].x === e.points[i - 1].x ||
          e.points[i].y === e.points[i - 1].y,
      ).toBe(true);
}
describe('геометрия графа', () => {
  it('возвращает пустой граф и не создаёт узел по повреждённой ссылке', () => {
    expect(graphLayout([], []).positions.size).toBe(0);
    expect(graphLayout([node('a')], [edge('a', 'missing')]).edges).toEqual([]);
  });
  it('сохраняет ключи разных решений на общей геометрической связи', () => {
    const result = graphLayout(
      [node('a'), node('b')],
      [
        { ...edge('a', 'b', 'Первый'), Key: 'one' },
        { ...edge('a', 'b', 'Второй'), Key: 'two' },
      ],
    );
    expect(result.edges[0].members.map((e) => e.Key)).toEqual(['one', 'two']);
    expect(result.groups).toEqual([]);
  });
  it('разводит возвраты development-cycle слева от внутренних к внешним', () => {
    const nodes = ['developer', 'reviewer', 'ui', 'qa'].map(node);
    const edges = [
      edge('developer', 'reviewer'),
      edge('reviewer', 'ui', 'Ревью пройдено'),
      edge('ui', 'qa', 'UI проверен / не менялся'),
      edge('reviewer', 'developer', 'Есть замечания'),
      edge('ui', 'developer', 'Замечания к UI'),
      edge('qa', 'developer', 'Найдены дефекты'),
    ];
    const result = graphLayout(nodes, edges);
    expect(result.groups).toEqual([]);
    const back = result.edges.filter((e) => e.feedback);
    expect(back).toHaveLength(3);
    expect(back.map((e) => e.label.y)).toEqual(
      [...back.map((e) => e.label.y)].sort((a, b) => a - b),
    );
    for (const e of back)
      expect(Math.min(...e.points.map((p) => p.x))).toBeLessThan(
        result.positions.get('developer')!.x - 16,
      );
    expect(crossings(result.edges)).toEqual([]);
    orthogonal(result.edges);
    expect(graphLayout(nodes, edges)).toEqual(result);
  });
  it('восемь выходов растят кубик до 116px, канал до 96px, не пересекаются', () => {
    const nodes = [
      'hub',
      ...Array.from({ length: 8 }, (_, i) => `target${i}`),
    ].map(node);
    const result = graphLayout(
      nodes,
      nodes.slice(1).map((n) => edge('hub', n.ID)),
    );
    expect(result.sizes.get('hub')!.height).toBe(116);
    expect(
      result.positions.get('target0')!.x - result.positions.get('hub')!.x - 226,
    ).toBe(96);
    expect(
      result.positions.get('target1')!.y -
        result.positions.get('target0')!.y -
        86,
    ).toBe(24);
    expect(crossings(result.edges)).toEqual([]);
    orthogonal(result.edges);
  });
  it('десять входов используют разные порты с шагом 12px и высоту 140px', () => {
    const nodes = [
      'hub',
      ...Array.from({ length: 10 }, (_, i) => `source${i}`),
    ].map(node);
    const result = graphLayout(
      nodes,
      nodes.slice(1).map((n) => edge(n.ID, 'hub')),
    );
    expect(result.sizes.get('hub')!.height).toBe(140);
    const ports = result.edges
      .map((e) => e.points.at(-1)!.y)
      .sort((a, b) => a - b);
    for (let i = 1; i < ports.length; i++)
      expect(ports[i] - ports[i - 1]).toBe(12);
    expect(crossings(result.edges)).toEqual([]);
  });
  it('self-loop имеет разные вход и выход на левой грани', () => {
    const result = graphLayout(
      [node('self')],
      [edge('self', 'self', 'Повторить')],
    );
    expect(result.edges[0].feedback).toBe(true);
    expect(result.edges[0].points[0].y).not.toBe(
      result.edges[0].points.at(-1)!.y,
    );
  });
  it('подпись ограничена 120px и тремя строками с многоточием', () => {
    const label = graphLabel(
      'Очень длинная причина возврата на предыдущую проверку после нескольких неудачных попыток',
      (text) => text.length * 7,
    );
    expect(label.width).toBeLessThanOrEqual(120);
    expect(label.height).toBe(54);
    expect(label.lines[2].endsWith('…')).toBe(true);
  });
});

// Реальная топология примера защищает совместимость общего маршрутизатора с
// договорённым дизайном: три возврата и три разных входа в терминальную ошибку.
it('development-cycle с обоими исходами не пересекает линии, карточки и подписи', () => {
  const nodes: GraphNode[] = ['developer', 'reviewer', 'ui', 'qa'].map(node);
  nodes.push(
    { ...node('start'), Marker: 'start' },
    { ...node('done'), Marker: 'succeeded' },
    { ...node('error'), Marker: 'failed' },
  );
  nodes[0].Start = true;
  const edges = [
    edge('start', 'developer'),
    edge('developer', 'reviewer'),
    edge('reviewer', 'ui', 'Ревью пройдено'),
    edge('ui', 'qa', 'UI проверен / не менялся'),
    edge('qa', 'done', 'Проверки пройдены'),
    ...['reviewer', 'ui', 'qa'].flatMap((id) => [
      edge(id, 'developer', 'Есть замечания'),
      edge(id, 'error', 'Агент не может продолжить'),
    ]),
  ];
  const result = graphLayout(nodes, edges);
  expect(crossings(result.edges)).toEqual([]);
  for (const e of result.edges) {
    for (let i = 1; i < e.points.length; i++) {
      const a = e.points[i - 1],
        b = e.points[i];
      for (const [id, p] of result.positions) {
        if (id === e.from || id === e.to) continue;
        const size = result.sizes.get(id)!;
        const collision =
          Math.max(a.x, b.x) > p.x - 19 &&
          Math.min(a.x, b.x) < p.x + size.width + 19 &&
          Math.max(a.y, b.y) > p.y - 19 &&
          Math.min(a.y, b.y) < p.y + size.height + 19;
        expect(collision, `${e.from}→${e.to} проходит через ${id}`).toBe(false);
      }
    }
    if (!e.text.width) continue;
    for (const other of result.edges)
      for (let i = 1; i < other.points.length; i++) {
        const a = other.points[i - 1],
          b = other.points[i];
        const pad = e === other ? 0 : 8;
        const collision =
          Math.max(a.x, b.x) > e.label.x - pad &&
          Math.min(a.x, b.x) < e.label.x + e.text.width + pad &&
          Math.max(a.y, b.y) > e.label.y - pad &&
          Math.min(a.y, b.y) < e.label.y + e.text.height + pad;
        expect(
          collision,
          `подпись ${e.from}→${e.to} пересекает ${other.from}→${other.to}`,
        ).toBe(false);
      }
  }
});
it('развилка с двумя подписями использует общий уровень поворотов', () => {
  const nodes = ['choice', 'yes', 'no', 'join'].map(node);
  const result = graphLayout(nodes, [
    edge('choice', 'yes', 'Можно публиковать'),
    edge('choice', 'no', 'Нужна доработка'),
    edge('yes', 'join'),
    edge('no', 'join'),
  ]);
  const branches = result.edges.filter((e) => e.from === 'choice');
  expect(branches[0].points[1].y).toBe(branches[1].points[1].y);
  expect(crossings(result.edges)).toEqual([]);
  const a = branches[0],
    b = branches[1];
  expect(
    a.label.x + a.text.width + 8 <= b.label.x ||
      b.label.x + b.text.width + 8 <= a.label.x,
  ).toBe(true);
});
it('длинный переход обходит промежуточные кубики', () => {
  const nodes = ['a', 'b', 'c', 'd'].map(node);
  const result = graphLayout(nodes, [
    edge('a', 'b'),
    edge('b', 'c'),
    edge('c', 'd'),
    edge('a', 'd', 'Пропустить проверки'),
  ]);
  orthogonal(result.edges);
  for (const e of result.edges)
    for (let i = 1; i < e.points.length; i++)
      for (const [id, p] of result.positions) {
        if (id === e.from || id === e.to) continue;
        const a = e.points[i - 1],
          b = e.points[i],
          size = result.sizes.get(id)!;
        expect(
          Math.max(a.x, b.x) > p.x - 19 &&
            Math.min(a.x, b.x) < p.x + size.width + 19 &&
            Math.max(a.y, b.y) > p.y - 19 &&
            Math.min(a.y, b.y) < p.y + size.height + 19,
          `${e.from}→${e.to} задевает ${id}`,
        ).toBe(false);
      }
});
it('десять входов и два выхода используют высокий кубик и общую вертикаль выходов', () => {
  const nodes = [
    'join',
    ...Array.from({ length: 10 }, (_, i) => `in${i}`),
    'out1',
    'out2',
  ].map(node);
  const result = graphLayout(nodes, [
    ...nodes.slice(1, 11).map((n) => edge(n.ID, 'join')),
    edge('join', 'out1'),
    edge('join', 'out2'),
  ]);
  expect(result.sizes.get('join')!.height).toBe(140);
  const out = result.edges.filter((e) => e.from === 'join');
  expect(out[0].points[1].x).toBe(out[1].points[1].x);
  expect(crossings(result.edges)).toEqual([]);
});
it('длинные заголовки не уменьшают зазор плотного столбца', () => {
  const nodes = ['hub', 'a', 'b', 'c', 'd'].map((ID) => ({
    ...node(ID),
    Title: ID === 'hub' ? 'Hub' : 'Очень длинное название проверки',
    Icon: 'Code',
  }));
  const result = graphLayout(
    nodes,
    nodes.slice(1).map((n) => edge('hub', n.ID)),
  );
  expect(result.sizes.get('a')!.height).toBeGreaterThan(80);
  expect(
    result.positions.get('b')!.y -
      result.positions.get('a')!.y -
      result.sizes.get('a')!.height -
      6,
  ).toBe(24);
});
