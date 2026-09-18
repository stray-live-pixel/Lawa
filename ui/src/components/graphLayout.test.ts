import { describe, expect, it } from 'vitest';
import { graphLayout } from './graphLayout';

// Искусственные ID проверяют, что правила не привязаны к sp-main-migration.
const nodes = ['start', 'a', 'b', 'join', 'end', 'exit'].map((ID) => ({
  ID,
  Prompt: '',
  Routes: [],
}));
const edges = [
  { From: 'start', To: 'a', Label: 'one' },
  { From: 'start', To: 'a', Label: 'two' },
  { From: 'start', To: 'b', Label: 'two' },
  { From: 'a', To: 'join', Label: '' },
  { From: 'b', To: 'join', Label: '' },
  { From: 'join', To: 'end', Label: '' },
  { From: 'end', To: 'start', Label: 'again' },
  { From: 'start', To: 'exit', Label: 'done' },
];
describe('структурная раскладка', () => {
  it('группирует цикл и параллельные ветки, сохраняет подписи и разводит кубики', () => {
    const result = graphLayout(nodes, edges);
    expect(result.groups.map((g) => g.label)).toEqual([
      'Цикл',
      'Параллельные ветки · 2',
    ]);
    expect(
      result.edges.find((e) => e.from === 'start' && e.to === 'a')?.labels,
    ).toEqual(['one', 'two']);
    expect(result.edges.filter((e) => e.feedback)).toHaveLength(1);
    const positions = [...result.positions.values()];
    for (let i = 0; i < positions.length; i++)
      for (let j = i + 1; j < positions.length; j++) {
        const a = positions[i],
          b = positions[j];
        expect(Math.abs(a.x - b.x) >= 220 || Math.abs(a.y - b.y) >= 80).toBe(
          true,
        );
      }
    expect(result.positions.get('a')!.y).toBe(result.positions.get('b')!.y);
    expect(result.positions.get('join')!.y).toBeGreaterThan(
      result.positions.get('a')!.y,
    );
    const cycle = result.groups[0],
      outside = result.positions.get('exit')!;
    expect(
      outside.x >= cycle.x + cycle.width ||
        outside.x + 220 <= cycle.x ||
        outside.y >= cycle.y + cycle.height ||
        outside.y + 80 <= cycle.y,
    ).toBe(true);
    expect(graphLayout(nodes, edges)).toEqual(result);
  });
  it('не теряет одиночные узлы, self-loop и несвязные компоненты', () => {
    const result = graphLayout(nodes, [
      { From: 'start', To: 'start', Label: 'retry' },
      { From: 'missing', To: 'a', Label: '' },
    ]);
    expect(result.positions.size).toBe(nodes.length);
    expect(result.edges).toHaveLength(1);
    const loop = result.edges[0];
    expect(loop.feedback).toBe(true);
    expect(new Set(loop.points.map((p) => p.y)).size).toBeGreaterThan(1);
    for (const p of result.positions.values())
      expect(Number.isFinite(p.x) && Number.isFinite(p.y)).toBe(true);
    expect(graphLayout([], []).positions.size).toBe(0);
  });
});

// Несколько возвратов раньше имели полосы уже собственных подписей.
it('разводит подписи возвратов на отдельных полосах', () => {
  const result = graphLayout(nodes, [
    { From: 'start', To: 'a', Label: 'ready' },
    { From: 'a', To: 'b', Label: 'ready' },
    { From: 'b', To: 'join', Label: 'ready' },
    { From: 'a', To: 'start', Label: 'retry' },
    { From: 'b', To: 'start', Label: 'retry' },
    { From: 'join', To: 'start', Label: 'retry' },
  ]);
  const labels = result.edges
    .filter((edge) => edge.feedback)
    .map((edge) => edge.label);
  expect(labels).toHaveLength(3);
  for (let i = 1; i < labels.length; i++)
    expect(Math.abs(labels[i].x - labels[i - 1].x)).toBeGreaterThan(135);
});
