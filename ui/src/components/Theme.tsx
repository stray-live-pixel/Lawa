import { createContext, useContext, useState, type ReactNode } from 'react';
import { ThemeProvider, DropdownMenu, Icon } from '@gravity-ui/uikit';
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
// Иконка показывает текущий выбор; меню сохраняет явный системный режим.
export function ThemePicker() {
  const { value, update } = useContext(ThemeChoice);
  const choices = [
    { value: 'system', label: 'Системная', icon: Display },
    { value: 'light', label: 'Светлая', icon: Sun },
    { value: 'dark', label: 'Тёмная', icon: Moon },
  ] as const;
  const current = choices.find((choice) => choice.value === value)!;
  return (
    <DropdownMenu
      icon={<Icon data={current.icon} />}
      defaultSwitcherProps={{
        'aria-label': 'Тема интерфейса',
        title: `Тема: ${current.label}`,
        view: 'flat',
      }}
      items={choices.map((choice) => ({
        text: `${choice.label}${choice.value === value ? ' ✓' : ''}`,
        iconStart: <Icon data={choice.icon} />,
        action: () => update(choice.value),
      }))}
    />
  );
}
