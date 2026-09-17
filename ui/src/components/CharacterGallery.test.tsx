import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it } from 'vitest';
import { AppTheme } from './Theme';
import { CharacterGallery } from './CharacterGallery';

afterEach(() => {
  cleanup();
  localStorage.clear();
});

// Справочник должен работать без заказа и копировать именно ID внешности,
// не рабочий @id агента и не локализованное имя. Просмотр не меняет предпочтения.
it('находит персонажа по ID и копирует его без изменения внешности', async () => {
  const user = userEvent.setup();
  const key = 'lawa-office-appearance-v1:order';
  const preferences = JSON.stringify({ developer: 'cat-ginger' });
  localStorage.setItem(key, preferences);
  render(
    <AppTheme>
      <CharacterGallery />
    </AppTheme>,
  );
  await user.click(screen.getByRole('button', { name: 'Галерея персонажей' }));
  expect(screen.getAllByRole('listitem')).toHaveLength(42);
  await user.type(
    screen.getByRole('textbox', { name: 'Поиск персонажей' }),
    'deer-reindeer',
  );
  expect(screen.getAllByRole('listitem')).toHaveLength(1);
  expect(screen.getByText('Северный тимлид')).toBeVisible();
  await user.click(
    screen.getByRole('button', { name: 'Скопировать ID deer-reindeer' }),
  );
  expect(await navigator.clipboard.readText()).toBe('deer-reindeer');
  expect(localStorage.getItem(key)).toBe(preferences);
});
