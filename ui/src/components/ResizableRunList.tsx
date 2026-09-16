import { useState, type CSSProperties, type ReactNode } from 'react';

const storageKey = 'lawa-run-list-width';
// Пожелание пользователя хранится отдельно от CSS-ограничения: при уменьшении
// окна граф сохраняет место, а при расширении возвращается выбранная ширина.
export function ResizableRunList({ children }: { children: ReactNode }) {
  const [width, setWidth] = useState(() => {
    try {
      const saved = Number(localStorage.getItem(storageKey));
      return Number.isFinite(saved) && saved >= 220 && saved <= 800
        ? saved
        : 285;
    } catch {
      return 285;
    }
  });
  const update = (value: number) => {
    const next = Math.round(Math.max(220, Math.min(800, value)));
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
      style={{ '--run-list-width': `${width}px` } as CSSProperties}
    >
      {children}
      <div
        className="run-list-resizer"
        role="separator"
        aria-label="Ширина списка запусков"
        aria-orientation="vertical"
        aria-valuemin={220}
        aria-valuemax={800}
        aria-valuenow={width}
        tabIndex={0}
        title="Потяните для изменения ширины. Двойной клик — сброс."
        onDoubleClick={() => update(285)}
        onKeyDown={(event) => {
          if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
            event.preventDefault();
            update(width + (event.key === 'ArrowRight' ? 20 : -20));
          } else if (event.key === 'Home') {
            event.preventDefault();
            update(220);
          } else if (event.key === 'End') {
            event.preventDefault();
            update(800);
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
          update(Math.min(event.clientX - box.left, box.width * 0.45));
        }}
        onPointerUp={(event) => {
          if (event.currentTarget.hasPointerCapture(event.pointerId))
            event.currentTarget.releasePointerCapture(event.pointerId);
        }}
      />
    </div>
  );
}
