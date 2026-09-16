import { createContext, useContext, useState, type ReactNode } from 'react';
import { ThemeProvider, Select, Icon } from '@gravity-ui/uikit';
import { Display, Sun, Moon } from '@gravity-ui/icons';

export type ThemePreference = 'system' | 'light' | 'dark';
const storageKey = 'lawa-theme';
const ThemeChoice = createContext<{
  value: ThemePreference;
  update: (value: ThemePreference) => void;
}>({ value: 'system', update: () => {} });

// Без сохранённого выбора следуем ОС. Некорректное/недоступное хранилище не
// мешает запуску; Gravity отслеживает изменение prefers-color-scheme вживую.
export function readTheme(): ThemePreference {
  try {
    const value = localStorage.getItem(storageKey);
    return value === 'light' || value === 'dark' ? value : 'system';
  } catch {
    return 'system';
  }
}
export function AppTheme({ children }: { children: ReactNode }) {
  const [value, setValue] = useState<ThemePreference>(readTheme);
  const update = (next: ThemePreference) => {
    setValue(next);
    try {
      localStorage.setItem(storageKey, next);
    } catch {
      /* Выбор действует до закрытия страницы. */
    }
  };
  return (
    <ThemeChoice.Provider value={{ value, update }}>
      <ThemeProvider theme={value} lang="ru">
        {children}
      </ThemeProvider>
    </ThemeChoice.Provider>
  );
}
// Выбор доступен на dashboard и самостоятельной странице графа.
export function ThemePicker() {
  const { value, update } = useContext(ThemeChoice);
  return (
    <Select
      aria-label="Тема интерфейса"
      className="theme-picker"
      value={[value]}
      onUpdate={([next]) => update(next as ThemePreference)}
      options={[
        {
          value: 'system',
          content: (
            <span className="theme-option">
              <Icon data={Display} />
              Системная
            </span>
          ),
          text: 'Системная',
        },
        {
          value: 'light',
          content: (
            <span className="theme-option">
              <Icon data={Sun} />
              Светлая
            </span>
          ),
          text: 'Светлая',
        },
        {
          value: 'dark',
          content: (
            <span className="theme-option">
              <Icon data={Moon} />
              Тёмная
            </span>
          ),
          text: 'Тёмная',
        },
      ]}
    />
  );
}
