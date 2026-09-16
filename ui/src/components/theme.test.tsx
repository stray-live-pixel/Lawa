import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { AppTheme, ThemePicker, readTheme } from './Theme';

// Проверяем поведение приложения с настоящим ThemeProvider: ОС может менять
// палитру в открытой вкладке, а явный выбор пользователя должен её фиксировать.
afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.restoreAllMocks();
});
it('следует ОС по умолчанию, сохраняет явный выбор и возвращается к системному', async () => {
  let dark = false;
  const listeners = new Set<(event: { matches: boolean }) => void>();
  vi.spyOn(window, 'matchMedia').mockImplementation(
    (media) =>
      ({
        media,
        get matches() {
          return media.includes('prefers-color-scheme') && dark;
        },
        addEventListener: (
          _event: string,
          listener: (event: { matches: boolean }) => void,
        ) => {
          if (media.includes('prefers-color-scheme')) listeners.add(listener);
        },
        removeEventListener: (
          _event: string,
          listener: (event: { matches: boolean }) => void,
        ) => {
          listeners.delete(listener);
        },
      }) as MediaQueryList,
  );
  const view = render(
    <AppTheme>
      <ThemePicker />
    </AppTheme>,
  );
  expect(
    screen.getByRole('button', { name: 'Тема интерфейса' }),
  ).toHaveAttribute('title', 'Тема: Системная');
  expect(document.body).toHaveClass('g-root_theme_light');
  act(() => {
    dark = true;
    listeners.forEach((listener) => listener({ matches: true }));
  });
  expect(document.body).toHaveClass('g-root_theme_dark');
  fireEvent.click(screen.getByRole('button', { name: 'Тема интерфейса' }));
  fireEvent.click(await screen.findByRole('menuitem', { name: 'Светлая' }));
  expect(localStorage.getItem('lawa-theme')).toBe('light');
  expect(document.body).toHaveClass('g-root_theme_light');
  act(() => {
    listeners.forEach((listener) => listener({ matches: true }));
  });
  expect(document.body).toHaveClass('g-root_theme_light');
  view.unmount();
  render(
    <AppTheme>
      <ThemePicker />
    </AppTheme>,
  );
  expect(
    screen.getByRole('button', { name: 'Тема интерфейса' }),
  ).toHaveAttribute('title', 'Тема: Светлая');
  fireEvent.click(screen.getByRole('button', { name: 'Тема интерфейса' }));
  fireEvent.click(await screen.findByRole('menuitem', { name: 'Системная' }));
  expect(document.body).toHaveClass('g-root_theme_dark');
});
it('недоступное или повреждённое хранилище не мешает системной теме', () => {
  localStorage.setItem('lawa-theme', 'unexpected');
  expect(readTheme()).toBe('system');
  vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
    throw new Error('denied');
  });
  expect(readTheme()).toBe('system');
});
