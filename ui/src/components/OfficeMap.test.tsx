import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { AppTheme } from './Theme';
import { OfficeMap } from './OfficeMap';

// jsdom не считает размеры и не эмулирует pointer capture: подменяем только
// эти браузерные примитивы, оставляя настоящие события компонента и кнопки UIKit.
beforeEach(() => {
  vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(800);
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(400);
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(
    function (this: Element) {
      const rect = this.classList.contains('office-employee')
        ? { x: 300, y: 200, width: 100, height: 100 }
        : { x: 0, y: 0, width: 800, height: 600 };
      return {
        ...rect,
        top: rect.y,
        left: rect.x,
        right: rect.x + rect.width,
        bottom: rect.y + rect.height,
        toJSON() {},
      };
    },
  );
  vi.stubGlobal(
    'PointerEvent',
    class extends MouseEvent {
      pointerId: number;
      constructor(type: string, options: PointerEventInit = {}) {
        super(type, options);
        this.pointerId = options.pointerId ?? 1;
      }
    },
  );
  Element.prototype.setPointerCapture = vi.fn();
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  delete (Element.prototype as Partial<Element>).setPointerCapture;
});

// Базовое управление доступно с клавиатуры без зависимости от жестов мыши.
it('приближает кнопкой, перемещает стрелками и возвращает весь офис', async () => {
  const user = userEvent.setup();
  const { container } = render(
    <AppTheme>
      <OfficeMap>
        <div>Комната</div>
      </OfficeMap>
    </AppTheme>,
  );
  expect(screen.getByRole('button', { name: 'Отдалить карту' })).toBeDisabled();
  await user.click(screen.getByRole('button', { name: 'Приблизить карту' }));
  expect(screen.getByLabelText('Текущий масштаб')).toHaveTextContent('125%');
  const map = screen.getByRole('region', { name: 'Карта офиса' });
  fireEvent.keyDown(map, { key: '+' });
  fireEvent.keyDown(map, { key: 'ArrowRight' });
  expect(
    container.querySelector('.office-map-scene')?.getAttribute('style'),
  ).toContain('translate(-40px');
  await user.click(screen.getByRole('button', { name: 'Показать весь офис' }));
  expect(screen.getByLabelText('Текущий масштаб')).toHaveTextContent('100%');
  expect(
    container.querySelector('.office-map-scene')?.getAttribute('style'),
  ).toContain('translate(0px, 0px) scale(1)');
});

it('после перетаскивания не открывает чат, но следующий клик и кнопки работают', async () => {
  const click = vi.fn();
  const user = userEvent.setup();
  render(
    <AppTheme>
      <OfficeMap>
        <div className="office-employee">
          <button onClick={click}>Сотрудник</button>
        </div>
      </OfficeMap>
    </AppTheme>,
  );
  const employee = screen.getByRole('button', { name: 'Сотрудник' });
  fireEvent.pointerDown(employee, {
    pointerId: 1,
    button: 0,
    clientX: 300,
    clientY: 200,
  });
  fireEvent.pointerMove(employee, { pointerId: 1, clientX: 380, clientY: 200 });
  fireEvent.pointerUp(employee, { pointerId: 1 });
  fireEvent.click(employee, { detail: 1 });
  expect(click).not.toHaveBeenCalled();
  await user.click(screen.getByRole('button', { name: 'Приблизить карту' }));
  expect(screen.getByLabelText('Текущий масштаб')).toHaveTextContent('125%');
  await user.click(employee);
  expect(click).toHaveBeenCalledOnce();
});

it('масштабирует двумя пальцами и завершает жест при pointercancel', () => {
  render(
    <AppTheme>
      <OfficeMap>
        <div>Комната</div>
      </OfficeMap>
    </AppTheme>,
  );
  const map = screen.getByRole('region', { name: 'Карта офиса' });
  fireEvent.pointerDown(map, {
    pointerId: 1,
    button: 0,
    clientX: 300,
    clientY: 300,
  });
  fireEvent.pointerDown(map, {
    pointerId: 2,
    button: 0,
    clientX: 500,
    clientY: 300,
  });
  fireEvent.pointerMove(map, { pointerId: 2, clientX: 700, clientY: 300 });
  expect(screen.getByLabelText('Текущий масштаб')).toHaveTextContent('200%');
  fireEvent.pointerCancel(map, { pointerId: 1 });
  fireEvent.pointerCancel(map, { pointerId: 2 });
  expect(map).not.toHaveClass('office-map-dragging');
  fireEvent.pointerMove(map, { pointerId: 2, clientX: 900, clientY: 300 });
  expect(screen.getByLabelText('Текущий масштаб')).toHaveTextContent('200%');
});

// Опрос состава меняет содержимое карты, но не выбранный пользователем ракурс.
// wheel должен отменять прокрутку страницы и сохранять точку под курсором.
it('масштабирует колесом к курсору и сохраняет камеру при обновлении команды', () => {
  const { container, rerender } = render(
    <AppTheme>
      <OfficeMap>
        <div>Один сотрудник</div>
      </OfficeMap>
    </AppTheme>,
  );
  const map = screen.getByRole('region', { name: 'Карта офиса' });
  const wheel = new WheelEvent('wheel', {
    bubbles: true,
    cancelable: true,
    clientX: 500,
    clientY: 350,
    deltaY: -Math.log(2) / 0.002,
  });
  act(() => {
    map.dispatchEvent(wheel);
  });
  expect(wheel.defaultPrevented).toBe(true);
  expect(screen.getByLabelText('Текущий масштаб')).toHaveTextContent('200%');
  const style = container
    .querySelector('.office-map-scene')
    ?.getAttribute('style');
  expect(style).toContain('translate(-100px, -50px) scale(2)');
  rerender(
    <AppTheme>
      <OfficeMap>
        <div>Пятьдесят сотрудников</div>
      </OfficeMap>
    </AppTheme>,
  );
  expect(
    container.querySelector('.office-map-scene')?.getAttribute('style'),
  ).toBe(style);
  fireEvent.keyDown(map, { key: '0', ctrlKey: true });
  expect(screen.getByLabelText('Текущий масштаб')).toHaveTextContent('200%');
});
