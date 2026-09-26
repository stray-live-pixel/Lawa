import { useState, type CSSProperties, type ReactNode } from 'react';

// Workflow-панели хранят долю контейнера; Code Review явно задаёт px-пределы.
// Для правой панели движение
// влево увеличивает ширину; настройки независимы и не читают старые пиксели.
// Определению нужно больше места для инструкции, чем панели результата запуска.
export function ResizableRunList({
  children,
  side = 'left',
  definition = false,
  preference,
}: {
  children: ReactNode;
  side?: 'left' | 'right';
  definition?: boolean;
  preference?: {
    key: string;
    initial: number;
    min: number;
    max: number;
    unit?: '%' | 'px';
    label?: string;
  };
}) {
  const right = side === 'right';
  const unit = preference?.unit || '%';
  const defaultWidth = preference?.initial ?? (definition ? 52 : 25);
  const min = preference?.min ?? 2,
    max = preference?.max ?? 98;
  const storageKey =
    preference?.key ??
    (definition
      ? 'lawa-definition-details-width-percent'
      : right
        ? 'lawa-cube-details-width-percent'
        : 'lawa-run-list-width-percent');
  const [width, setWidth] = useState(() => {
    try {
      const saved = Number(localStorage.getItem(storageKey));
      return Number.isFinite(saved) && saved >= min && saved <= max
        ? saved
        : defaultWidth;
    } catch {
      return defaultWidth;
    }
  });
  const update = (value: number) => {
    const next = Math.round(Math.max(min, Math.min(max, value)) * 10) / 10;
    setWidth(next);
    try {
      localStorage.setItem(storageKey, String(next));
    } catch {
      /* Размер действует до закрытия страницы. */
    }
  };
  return (
    <div
      className={right ? 'graph-workspace' : 'inspector-body'}
      style={{ '--panel-width': `${width}${unit}` } as CSSProperties}
    >
      {children}
      <div
        className={`run-list-resizer ${right ? 'details-resizer' : ''}`}
        role="separator"
        aria-label={
          preference?.label ||
          (right ? 'Ширина информации о кубике' : 'Ширина списка запусков')
        }
        aria-orientation="vertical"
        aria-valuemin={min}
        aria-valuemax={max}
        aria-valuenow={width}
        aria-valuetext={`${width}${unit}`}
        tabIndex={0}
        title="Потяните для изменения ширины. Двойной клик — сброс."
        onDoubleClick={() => update(defaultWidth)}
        onKeyDown={(event) => {
          if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
            event.preventDefault();
            update(
              width +
                (event.key === 'ArrowRight' ? 1 : -1) *
                  (right ? -1 : 1) *
                  (unit === 'px' ? 8 : 1),
            );
          } else if (event.key === 'Home') {
            event.preventDefault();
            update(min);
          } else if (event.key === 'End') {
            event.preventDefault();
            update(max);
          }
        }}
        onPointerDown={(event) => {
          if (event.button !== 0) return;
          event.preventDefault();
          event.currentTarget.focus();
          event.currentTarget.setPointerCapture(event.pointerId);
        }}
        onPointerMove={(event) => {
          if (!event.currentTarget.hasPointerCapture(event.pointerId)) return;
          const box =
            event.currentTarget.parentElement!.getBoundingClientRect();
          if (box.width > 0)
            update(
              unit === 'px'
                ? right
                  ? box.right - event.clientX
                  : event.clientX - box.left
                : ((right
                    ? box.right - event.clientX
                    : event.clientX - box.left) /
                    box.width) *
                    100,
            );
        }}
        onPointerUp={(event) => {
          if (event.currentTarget.hasPointerCapture(event.pointerId))
            event.currentTarget.releasePointerCapture(event.pointerId);
        }}
      />
    </div>
  );
}
