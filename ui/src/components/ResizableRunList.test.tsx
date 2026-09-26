import { fireEvent, render, screen, cleanup } from '@testing-library/react';
import { afterEach, beforeEach, expect, it } from 'vitest';
import { ResizableRunList } from './ResizableRunList';

beforeEach(() => localStorage.clear());
afterEach(cleanup);

// Разделители двигаются в одну сторону, но ширина правой панели меняется
// противоположно левой. Граничные значения не должны зависеть от пикселей.
it('сохраняет независимые проценты, пределы и сброс обеих панелей', () => {
  const view = render(
    <>
      <ResizableRunList>Левая</ResizableRunList>
      <ResizableRunList side="right">Правая</ResizableRunList>
    </>,
  );
  const [left, right] = screen.getAllByRole('separator');
  expect(left).toHaveAttribute('aria-valuenow', '25');
  expect(right).toHaveAttribute('aria-valuenow', '25');
  fireEvent.keyDown(right, { key: 'ArrowLeft' });
  expect(right).toHaveAttribute('aria-valuenow', '26');
  expect(left).toHaveAttribute('aria-valuenow', '25');
  fireEvent.keyDown(left, { key: 'Home' });
  fireEvent.keyDown(left, { key: 'ArrowLeft' });
  expect(left).toHaveAttribute('aria-valuenow', '2');
  fireEvent.keyDown(right, { key: 'End' });
  fireEvent.keyDown(right, { key: 'ArrowLeft' });
  expect(right).toHaveAttribute('aria-valuenow', '98');
  view.unmount();
  render(
    <>
      <ResizableRunList>Левая</ResizableRunList>
      <ResizableRunList side="right">Правая</ResizableRunList>
    </>,
  );
  const restored = screen.getAllByRole('separator');
  expect(restored[0]).toHaveAttribute('aria-valuenow', '2');
  expect(restored[1]).toHaveAttribute('aria-valuenow', '98');
  fireEvent.doubleClick(restored[1]);
  expect(restored[1]).toHaveAttribute('aria-valuenow', '25');
});

// Desktop-review сохраняет ширину в пикселях, независимо от большого монитора.
it('поддерживает отдельные пиксельные пределы истории ревью', () => {
  render(
    <ResizableRunList
      preference={{
        key: 'review-width',
        initial: 280,
        min: 240,
        max: 480,
        unit: 'px',
        label: 'История ревью',
      }}
    >
      История
    </ResizableRunList>,
  );
  const handle = screen.getByRole('separator', { name: 'История ревью' });
  expect(handle).toHaveAttribute('aria-valuetext', '280px');
  fireEvent.keyDown(handle, { key: 'ArrowRight' });
  expect(handle).toHaveAttribute('aria-valuenow', '288');
  fireEvent.keyDown(handle, { key: 'Home' });
  expect(handle).toHaveAttribute('aria-valuenow', '240');
  fireEvent.keyDown(handle, { key: 'End' });
  expect(handle).toHaveAttribute('aria-valuenow', '480');
});
