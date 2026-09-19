import { expect, it } from 'vitest';
import { visibleSelection } from './graphViewport';

// N1: после сужения уже открытого окна выбранный узел не остаётся за краем.
it('возвращает выбранный узел после сужения без сброса масштаба', () => {
  const next = visibleSelection(
    { x: 500, y: 100, zoom: 0.8 },
    { x: 100, y: 100 },
    390,
    320,
  )!;
  expect(next.zoom).toBe(0.8);
  expect(100 * next.zoom + next.x).toBeGreaterThanOrEqual(24);
  expect(320 * next.zoom + next.x).toBeLessThanOrEqual(390 - 24);
});
it('не меняет пользовательский вид, пока выбранный узел помещается', () => {
  expect(
    visibleSelection({ x: 30, y: 30, zoom: 1 }, { x: 0, y: 0 }, 600, 400),
  ).toBeNull();
});
it('уменьшает масштаб только если сам узел не помещается', () => {
  const next = visibleSelection(
    { x: 0, y: 0, zoom: 2 },
    { x: 200, y: 500 },
    300,
    320,
  )!;
  expect(next.zoom).toBeCloseTo(252 / 220);
  expect(next.y + 500 * next.zoom).toBeGreaterThan(24);
});
