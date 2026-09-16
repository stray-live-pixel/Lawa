import { useState, type CSSProperties, type ReactNode } from 'react';

// Обе панели хранят долю своего контейнера. Для правой панели движение
// влево увеличивает ширину; настройки независимы и не читают старые пиксели.
export function ResizableRunList({
  children,
  side = 'left',
}: {
  children: ReactNode;
  side?: 'left' | 'right';
}) {
  const right = side === 'right';
  const storageKey = right
    ? 'lawa-cube-details-width-percent'
    : 'lawa-run-list-width-percent';
  const [width, setWidth] = useState(() => {
    try {
      const saved = Number(localStorage.getItem(storageKey));
      return Number.isFinite(saved) && saved >= 2 && saved <= 98 ? saved : 25;
    } catch {
      return 25;
    }
  });
  const update = (value: number) => {
    const next = Math.round(Math.max(2, Math.min(98, value)) * 10) / 10;
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
      style={{ '--panel-width': `${width}%` } as CSSProperties}
    >
      {children}
      <div
        className={`run-list-resizer ${right ? 'details-resizer' : ''}`}
        role="separator"
        aria-label={
          right ? 'Ширина информации о кубике' : 'Ширина списка запусков'
        }
        aria-orientation="vertical"
        aria-valuemin={2}
        aria-valuemax={98}
        aria-valuenow={width}
        aria-valuetext={`${width}%`}
        tabIndex={0}
        title="Потяните для изменения ширины. Двойной клик — сброс."
        onDoubleClick={() => update(25)}
        onKeyDown={(event) => {
          if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
            event.preventDefault();
            update(
              width + (event.key === 'ArrowRight' ? 1 : -1) * (right ? -1 : 1),
            );
          } else if (event.key === 'Home') {
            event.preventDefault();
            update(2);
          } else if (event.key === 'End') {
            event.preventDefault();
            update(98);
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
              ((right ? box.right - event.clientX : event.clientX - box.left) /
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
