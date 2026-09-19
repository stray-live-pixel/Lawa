import type { GraphEdge, GraphNode } from '../types';
import type { LabelSize, TextMeasure } from './graphLabel';

export interface Point {
  x: number;
  y: number;
}
export interface Box extends Point {
  width: number;
  height: number;
}
export interface RoutedEdge {
  from: string;
  to: string;
  labels: string[];
  members: GraphEdge[];
  points: Point[];
  label: Point;
  text: LabelSize;
  feedback: boolean;
}
export interface LayoutGroup extends Box {
  id: string;
  label: string;
}
export const RESERVE = 3;
export const GAP = 64;
export const PITCH = 12;
export const LANE = 16;
export const round16 = (value: number) => Math.ceil(value / 16) * 16;
export const pair = (from: string, to: string) => JSON.stringify([from, to]);

// Каркас не включает наружную рамку. Её максимальный размер резервируется
// всегда, поэтому выбор и polling меняют только цвет и точку касания стрелки.
export function nodeSize(
  node: GraphNode,
  sidePorts: number,
  measure: TextMeasure = (text, font) =>
    [...text].length * (font === 'title' ? 6.5 : 7),
) {
  return {
    width: node.Marker ? 160 : 220,
    height: node.Marker
      ? 40
      : Math.ceil(
          Math.max(
            // Две строки заголовка и две строки самого длинного статуса.
            measure(node.Title || node.ID, 'title') > (node.Icon ? 168 : 192)
              ? 92
              : 80,
            32 + PITCH * (sidePorts - 1),
          ) / 4,
        ) * 4,
  };
}

// Удаляем только лишние точки одного прямого участка, не скругляя повороты.
export function simplify(points: Point[]): Point[] {
  const result: Point[] = [];
  for (const p of points) {
    if (result.length && result.at(-1)!.x === p.x && result.at(-1)!.y === p.y)
      continue;
    const a = result.at(-2),
      b = result.at(-1);
    if (
      a &&
      b &&
      ((a.x === b.x && b.x === p.x) || (a.y === b.y && b.y === p.y))
    )
      result.pop();
    result.push(p);
  }
  return result;
}
