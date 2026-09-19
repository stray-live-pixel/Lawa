import { it, expect } from 'vitest';
import { arrowGeometry, graphDrawing, type GraphDrawing } from './graphDrawing';
import { graphLayout } from './graphLayout';
const route = graphLayout(
  ['a', 'b'].map((ID) => ({ ID, Prompt: '', Routes: [] })),
  [{ From: 'a', To: 'b', Label: '' }],
).edges[0];
const drawing: GraphDrawing = {
  route,
  active: false,
  sourceSelected: false,
  targetSelected: false,
};
it('наконечник касается видимой рамки, ствол не проходит внутри него', () => {
  const normal = arrowGeometry(drawing);
  const selected = arrowGeometry({ ...drawing, targetSelected: true });
  const end = route.points.at(-1)!;
  expect(selected.points.at(-1)!.y).toBe(end.y - 8);
  expect(normal.points.at(-1)!.y).toBe(end.y - 6);
  expect(selected.arrow.startsWith(`${end.x},${end.y} `)).toBe(true);
  expect(normal.arrow.startsWith(`${end.x},${end.y + 2} `)).toBe(true);
});
it('общий ствол рисуется один раз и сохраняет цвет причины', () => {
  const drawings = graphDrawing([drawing, { ...drawing, active: true }]);
  expect(drawings[0].path).toBe('');
  expect(drawings[1].path).toContain('M');
});
