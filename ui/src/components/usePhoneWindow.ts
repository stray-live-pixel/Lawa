import {
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type PointerEvent,
} from 'react';

interface Bounds {
  left: number;
  top: number;
  width: number;
  height: number;
}
type Operation = 'move' | 'resize';
const margin = 12;

// Сохраняем всё окно в пределах экрана. На маленьком экране минимальный размер
// уступает доступному месту, чтобы шапка и закрытие всегда оставались доступны.
export function fitPhone(
  bounds: Bounds,
  viewportWidth: number,
  viewportHeight: number,
): Bounds {
  const availableWidth = Math.max(1, viewportWidth - margin * 2);
  const availableHeight = Math.max(1, viewportHeight - margin * 2);
  const width = Math.min(availableWidth, Math.max(320, bounds.width));
  const height = Math.min(availableHeight, Math.max(420, bounds.height));
  return {
    width,
    height,
    left: Math.max(
      margin,
      Math.min(bounds.left, viewportWidth - width - margin),
    ),
    top: Math.max(
      margin,
      Math.min(bounds.top, viewportHeight - height - margin),
    ),
  };
}

// Локальное окно не блокирует офис и не закрывается по клику снаружи.
// Pointer capture сохраняет drag за пределами ручки; отмена жеста прекращает
// движение. Размер и позиция живут только до закрытия телефона.
export function usePhoneWindow() {
  const [bounds, setBounds] = useState(() =>
    fitPhone(
      {
        left: (window.innerWidth - 390) / 2,
        top: (window.innerHeight - 760) / 2,
        width: 390,
        height: 760,
      },
      window.innerWidth,
      window.innerHeight,
    ),
  );
  const panel = useRef<HTMLElement>(null);
  const gesture = useRef<{
    operation: Operation;
    pointerId: number;
    x: number;
    y: number;
    bounds: Bounds;
  } | null>(null);

  useLayoutEffect(() => {
    const previous = document.activeElement;
    const element = panel.current;
    element?.focus();
    const resize = () => {
      gesture.current = null;
      setBounds((current) =>
        fitPhone(current, window.innerWidth, window.innerHeight),
      );
    };
    window.addEventListener('resize', resize);
    return () => {
      window.removeEventListener('resize', resize);
      // Не забираем фокус обратно, если пользователь уже перешёл в офис.
      if (
        element?.contains(document.activeElement) &&
        previous instanceof HTMLElement &&
        previous.isConnected
      )
        previous.focus();
    };
  }, []);

  function update(start: Bounds, operation: Operation, dx: number, dy: number) {
    const next =
      operation === 'move'
        ? { ...start, left: start.left + dx, top: start.top + dy }
        : {
            ...start,
            width: Math.min(
              start.width + dx,
              window.innerWidth - start.left - margin,
            ),
            height: Math.min(
              start.height + dy,
              window.innerHeight - start.top - margin,
            ),
          };
    setBounds(fitPhone(next, window.innerWidth, window.innerHeight));
  }

  function pointerProps(operation: Operation) {
    return {
      onPointerDown(event: PointerEvent<HTMLElement>) {
        if (
          event.button !== 0 ||
          (operation === 'move' &&
            (event.target as Element).closest('button, a, input, textarea'))
        )
          return;
        event.preventDefault();
        event.currentTarget.setPointerCapture(event.pointerId);
        gesture.current = {
          operation,
          pointerId: event.pointerId,
          x: event.clientX,
          y: event.clientY,
          bounds,
        };
      },
      onPointerMove(event: PointerEvent<HTMLElement>) {
        const start = gesture.current;
        if (!start || start.pointerId !== event.pointerId) return;
        update(
          start.bounds,
          start.operation,
          event.clientX - start.x,
          event.clientY - start.y,
        );
      },
      onPointerUp(event: PointerEvent<HTMLElement>) {
        if (gesture.current?.pointerId !== event.pointerId) return;
        gesture.current = null;
        event.currentTarget.releasePointerCapture(event.pointerId);
      },
      onPointerCancel() {
        gesture.current = null;
      },
      onLostPointerCapture() {
        gesture.current = null;
      },
    };
  }

  // Доступны те же операции без мыши: стрелки на заголовке или ручке размера.
  function keyboard(operation: Operation, event: KeyboardEvent<HTMLElement>) {
    const direction = {
      ArrowLeft: [-1, 0],
      ArrowRight: [1, 0],
      ArrowUp: [0, -1],
      ArrowDown: [0, 1],
    }[event.key];
    if (!direction) return;
    event.preventDefault();
    const step = event.shiftKey ? 40 : 10;
    update(bounds, operation, direction[0] * step, direction[1] * step);
  }
  return {
    bounds,
    panel,
    move: pointerProps('move'),
    resize: pointerProps('resize'),
    keyboard,
  };
}
