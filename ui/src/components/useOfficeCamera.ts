import {
  useEffect,
  useRef,
  useState,
  type MouseEvent,
  type PointerEvent,
  type KeyboardEvent,
  type FocusEvent,
} from 'react';
import {
  boundCamera,
  initialOfficeCamera,
  zoomCamera,
  type CameraBounds,
  type OfficeCamera,
} from './officeCamera';

type Point = { x: number; y: number };

// Камера живёт отдельно от данных команды: опрос API и перемотка её не сбрасывают.
// Ref обновляется сразу, чтобы серия событий wheel/pointer не читала старый кадр React.
export function useOfficeCamera() {
  const viewport = useRef<HTMLDivElement>(null);
  const scene = useRef<HTMLDivElement>(null);
  const current = useRef(initialOfficeCamera);
  const bounds = useRef<CameraBounds>({
    width: 0,
    height: 0,
    sceneWidth: 0,
    sceneHeight: 0,
  });
  const pointers = useRef(new Map<number, Point>());
  const travel = useRef(0);
  const suppressClick = useRef(false);
  const [camera, setCamera] = useState(initialOfficeCamera);
  const [dragging, setDragging] = useState(false);

  // Единственная точка записи применяет границы к кнопкам, мыши, касаниям и resize.
  function update(next: OfficeCamera) {
    current.current = boundCamera(next, bounds.current);
    setCamera(current.current);
  }
  function zoom(factor: number, anchor?: Point) {
    update(zoomCamera(current.current, current.current.scale * factor, anchor));
  }
  function reset() {
    update(initialOfficeCamera);
  }
  function localPoint(point: Point): Point {
    const rect = viewport.current!.getBoundingClientRect();
    return {
      x: point.x - rect.left - rect.width / 2,
      y: point.y - rect.top - rect.height / 2,
    };
  }

  useEffect(() => {
    const element = viewport.current!;
    const image = scene.current!;
    // offset-размеры сцены не включают transform; измерять её DOMRect здесь нельзя.
    const measure = () => {
      bounds.current = {
        width: element.clientWidth,
        height: element.clientHeight,
        sceneWidth: image.offsetWidth,
        sceneHeight: image.offsetHeight,
      };
      update(current.current);
    };
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    observer.observe(image);
    measure();
    // Непассивный слушатель отменяет прокрутку/браузерный pinch только над картой.
    const wheel = (event: WheelEvent) => {
      if ((event.target as Element).closest('.office-map-controls')) return;
      event.preventDefault();
      const unit =
        event.deltaMode === 1
          ? 16
          : event.deltaMode === 2
            ? element.clientHeight
            : 1;
      zoom(
        Math.exp(-event.deltaY * unit * 0.002),
        localPoint({ x: event.clientX, y: event.clientY }),
      );
    };
    element.addEventListener('wheel', wheel, { passive: false });
    return () => {
      observer.disconnect();
      element.removeEventListener('wheel', wheel);
    };
  }, []);

  // Capture остаётся на исходной кнопке сотрудника: обычный клик работает.
  // Перетаскивание начинается после 5 px и подавляет только последующий мышиный click.
  function pointerDown(event: PointerEvent<HTMLDivElement>) {
    if (
      event.button !== 0 ||
      (event.target as Element).closest('.office-map-controls')
    )
      return;
    if (!pointers.current.size) {
      travel.current = 0;
      suppressClick.current = false;
    }
    pointers.current.set(event.pointerId, {
      x: event.clientX,
      y: event.clientY,
    });
    (event.target as Element).setPointerCapture(event.pointerId);
    if (pointers.current.size > 1) {
      suppressClick.current = true;
      setDragging(true);
    }
  }
  function pointerMove(event: PointerEvent<HTMLDivElement>) {
    const previous = pointers.current.get(event.pointerId);
    if (!previous) return;
    const before = [...pointers.current.values()];
    const point = { x: event.clientX, y: event.clientY };
    pointers.current.set(event.pointerId, point);
    const after = [...pointers.current.values()];
    if (after.length === 2) {
      const start = midpoint(before[0], before[1]);
      const end = midpoint(after[0], after[1]);
      const length = distance(before[0], before[1]);
      if (length < 1) return;
      const next = zoomCamera(
        current.current,
        (current.current.scale * distance(after[0], after[1])) / length,
        localPoint(start),
      );
      update({
        ...next,
        x: next.x + end.x - start.x,
        y: next.y + end.y - start.y,
      });
    } else if (after.length === 1) {
      const delta = { x: point.x - previous.x, y: point.y - previous.y };
      travel.current += Math.hypot(delta.x, delta.y);
      if (travel.current <= 5 && !suppressClick.current) return;
      suppressClick.current = true;
      setDragging(true);
      update({
        ...current.current,
        x: current.current.x + delta.x,
        y: current.current.y + delta.y,
      });
    }
  }
  function pointerEnd(event: PointerEvent<HTMLDivElement>) {
    pointers.current.delete(event.pointerId);
    if (!pointers.current.size) setDragging(false);
  }
  function clickCapture(event: MouseEvent<HTMLDivElement>) {
    if ((event.target as Element).closest('.office-map-controls')) return;
    if (suppressClick.current && event.detail !== 0) {
      event.preventDefault();
      event.stopPropagation();
    }
  }
  // Клавиши карты действуют только при фокусе на ней, не перехватывая ввод в UI.
  function keyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (
      event.target !== event.currentTarget ||
      event.ctrlKey ||
      event.metaKey ||
      event.altKey
    )
      return;
    if (event.key === '+' || event.key === '=') zoom(1.25);
    else if (event.key === '-') zoom(1 / 1.25);
    else if (event.key === '0' || event.key === 'Home') reset();
    else if (event.key.startsWith('Arrow')) {
      const delta = {
        ArrowLeft: [40, 0],
        ArrowRight: [-40, 0],
        ArrowUp: [0, 40],
        ArrowDown: [0, -40],
      }[event.key];
      if (!delta) return;
      update({
        ...current.current,
        x: current.current.x + delta[0],
        y: current.current.y + delta[1],
      });
    } else return;
    event.preventDefault();
  }
  // Tab к сотруднику за краем увеличенной карты подводит камеру к его кнопке.
  function focusCapture(event: FocusEvent<HTMLDivElement>) {
    const employee = (event.target as Element).closest('.office-employee');
    if (!employee) return;
    const rect = employee.getBoundingClientRect();
    const area = viewport.current!.getBoundingClientRect();
    const horizontal =
      rect.left < area.left + 24
        ? area.left + 24 - rect.left
        : rect.right > area.right - 24
          ? area.right - 24 - rect.right
          : 0;
    const vertical =
      rect.top < area.top + 24
        ? area.top + 24 - rect.top
        : rect.bottom > area.bottom - 72
          ? area.bottom - 72 - rect.bottom
          : 0;
    if (horizontal || vertical)
      update({
        ...current.current,
        x: current.current.x + horizontal,
        y: current.current.y + vertical,
      });
  }

  return {
    viewport,
    scene,
    camera,
    dragging,
    zoom,
    reset,
    pointerDown,
    pointerMove,
    pointerEnd,
    clickCapture,
    keyDown,
    focusCapture,
  };
}

// Геометрия двух пальцев: переносим середину и масштабируем расстояние между ними.
function midpoint(first: Point, second: Point): Point {
  return { x: (first.x + second.x) / 2, y: (first.y + second.y) / 2 };
}
function distance(first: Point, second: Point): number {
  return Math.hypot(first.x - second.x, first.y - second.y);
}
