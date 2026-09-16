import { useState, type CSSProperties, type ReactNode } from 'react';

const storageKey = 'lawa-run-list-width-percent';
// Ширина хранится в процентах: при изменении окна пропорция сохраняется.
// Старый ключ с пикселями не читаем, чтобы он не подменял новый default 25%.
export function ResizableRunList({ children }: { children: ReactNode }) {
  const [width, setWidth] = useState(() => {
    try {
      const saved = Number(localStorage.getItem(storageKey));
      return Number.isFinite(saved) && saved >= 15 && saved <= 45 ? saved : 25;
    } catch {
      return 25;
    }
  });
  const update = (value: number) => {
    const next = Math.round(Math.max(15, Math.min(45, value)) * 10) / 10;
    setWidth(next);
    try {
      localStorage.setItem(storageKey, String(next));
    } catch {
      /* Размер действует до закрытия страницы. */
    }
  };
  return (
    <div
      className="inspector-body"
      style={{ '--run-list-width': `${width}%` } as CSSProperties}
    >
      {children}
      <div
        className="run-list-resizer"
        role="separator"
        aria-label="Ширина списка запусков"
        aria-orientation="vertical"
        aria-valuemin={15}
        aria-valuemax={45}
        aria-valuenow={width}
        aria-valuetext={`${width}%`}
        tabIndex={0}
        title="Потяните для изменения ширины. Двойной клик — сброс."
        onDoubleClick={() => update(25)}
        onKeyDown={(event) => {
          if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
            event.preventDefault();
            update(width + (event.key === 'ArrowRight' ? 1 : -1));
          } else if (event.key === 'Home') {
            event.preventDefault();
            update(15);
          } else if (event.key === 'End') {
            event.preventDefault();
            update(45);
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
            update(((event.clientX - box.left) / box.width) * 100);
        }}
        onPointerUp={(event) => {
          if (event.currentTarget.hasPointerCapture(event.pointerId))
            event.currentTarget.releasePointerCapture(event.pointerId);
        }}
      />
    </div>
  );
}
