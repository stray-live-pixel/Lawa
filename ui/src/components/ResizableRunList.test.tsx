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
