import type { Viewport, XYPosition } from '@xyflow/react';

// При сужении окна или выборе вне кадра возвращаем выбранный узел в видимую
// область. Масштаб сохраняется, кроме случая, когда сам узел шире/выше окна.
// null означает, что текущий пользовательский вид уже подходит.
export function visibleSelection(
  viewport: Viewport,
  position: XYPosition,
  width: number,
  height: number,
  size: { width: number; height: number } = { width: 220, height: 80 },
): Viewport | null {
  const margin = 24;
  const zoom = Math.min(
    viewport.zoom,
    Math.max(0.1, (width - margin * 2) / size.width),
    Math.max(0.1, (height - margin * 2) / size.height),
  );
  const left = position.x * viewport.zoom + viewport.x;
  const top = position.y * viewport.zoom + viewport.y;
  if (
    zoom === viewport.zoom &&
    left >= margin &&
    top >= margin &&
    left + size.width * zoom <= width - margin &&
    top + size.height * zoom <= height - margin
  )
    return null;
  return {
    x: width / 2 - (position.x + size.width / 2) * zoom,
    y: height / 2 - (position.y + size.height / 2) * zoom,
    zoom,
  };
}
