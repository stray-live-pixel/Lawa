import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { AppTheme } from './Theme';
import { CharacterGallery } from './CharacterGallery';

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  localStorage.clear();
});

// Справочник должен работать без заказа и копировать именно ID внешности,
// не рабочий @id агента и не локализованное имя. Просмотр не меняет предпочтения.
it('находит персонажа по ID и копирует его без изменения внешности', async () => {
  vi.useFakeTimers();
  const key = 'lawa-office-appearance-v1:order';
  const preferences = JSON.stringify({ developer: 'cat-ginger' });
  localStorage.setItem(key, preferences);
  render(
    <AppTheme>
      <CharacterGallery />
    </AppTheme>,
  );
  const trigger = screen.getByRole('button', { name: 'Галерея персонажей' });
  trigger.focus();
  fireEvent.click(trigger);
  expect(screen.getAllByRole('listitem')).toHaveLength(42);
  const search = screen.getByRole('textbox', { name: 'Поиск персонажей' });
  // Modal запускает анимацию в кадре, затем по таймеру включает focus manager,
  // который переносит фокус в следующем кадре. Управляем временем, чтобы нагрузка
  // CI не исчерпывала таймаут ожидания. Сам фокус устанавливает настоящий UIKit.
  await act(() => vi.advanceTimersToNextFrame());
  await act(() => vi.runOnlyPendingTimersAsync());
  await act(() => vi.advanceTimersToNextFrame());
  expect(search).toHaveFocus();
  vi.useRealTimers();
  const user = userEvent.setup();
  await user.type(search, 'deer-reindeer');
  expect(search).toHaveValue('deer-reindeer');
  expect(search).toHaveFocus();
  expect(screen.getAllByRole('listitem')).toHaveLength(1);
  expect(screen.getByText('Северный тимлид')).toBeVisible();
  await user.click(
    screen.getByRole('button', { name: 'Скопировать ID deer-reindeer' }),
  );
  expect(await navigator.clipboard.readText()).toBe('deer-reindeer');
  expect(localStorage.getItem(key)).toBe(preferences);
});
