import { fireEvent, render, screen, cleanup } from '@testing-library/react';
import { afterEach, expect, it } from 'vitest';
import { SidebarToggle } from './SidebarToggle';

afterEach(cleanup);

// CSS скрывает меню по aria-pressed; одной смены иконки недостаточно.
// Проверяем реальный DOM Gravity, который получает состояние через selected.
it('передаёт состояние раскрытия в атрибут, используемый сеткой', () => {
  render(<SidebarToggle />);
  const button = screen.getByRole('button', { name: 'Развернуть граф' });
  expect(button).toHaveAttribute('aria-pressed', 'false');
  fireEvent.click(button);
  expect(button).toHaveAttribute('aria-pressed', 'true');
  expect(button).toHaveAccessibleName('Свернуть граф');
  fireEvent.click(button);
  expect(button).toHaveAttribute('aria-pressed', 'false');
});
