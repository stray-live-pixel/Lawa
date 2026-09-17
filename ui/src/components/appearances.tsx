import { createContext, useContext, useState, type ReactNode } from 'react';
import catalog from '../assets/office/characters/catalog.json';
import boss from '../assets/office/boss.png';
import developer from '../assets/office/developer.png';

// Реестр внешности не определяет полномочия или профессию агента. Один PNG
// используется в сцене и в аватарке: лицо в чате всегда соответствует персонажу.
export interface Appearance {
  id: string;
  name: string;
  profession: string;
  kind: string;
  sprite: string;
}
const sprites = import.meta.glob<string>('../assets/office/characters/*.png', {
  eager: true,
  query: '?url',
  import: 'default',
});
export const appearances: Appearance[] = catalog.map((item) => ({
  ...item,
  sprite: sprites[`../assets/office/characters/${item.id}.png`],
}));
const defaults: Record<string, string> = { boss, developer };
// Галерея включает также исходные образы: их ID уже используются в реестре
// участников. Эти два образа не меняют состав случайного выбора из 40 новых.
export const characterGallery: Appearance[] = [
  {
    id: 'boss',
    name: 'Босс',
    profession: 'Исходный образ',
    kind: 'human',
    sprite: boss,
  },
  {
    id: 'developer',
    name: 'Разработчик',
    profession: 'Исходный образ',
    kind: 'human',
    sprite: developer,
  },
  ...appearances,
];
type Choices = Record<string, string>;
const storageKey = (scope: string) =>
  `lawa-office-appearance-v1:${scope || 'draft'}`;

// Повреждённые или устаревшие записи игнорируем. Никаких произвольных URL из
// localStorage: допускаются только ID поставляемого вместе с приложением каталога.
export function readAppearanceChoices(scope: string): Choices {
  try {
    const value: unknown = JSON.parse(
      localStorage.getItem(storageKey(scope)) || '{}',
    );
    if (!value || typeof value !== 'object' || Array.isArray(value)) return {};
    return Object.fromEntries(
      Object.entries(value).filter(
        ([, id]) =>
          typeof id === 'string' && appearances.some((item) => item.id === id),
      ),
    );
  } catch {
    return {};
  }
}

// При повторном случайном выборе гарантируем другой образ. Функция выделена,
// чтобы проверять границы выбора без генерации или загрузки изображений.
export function randomAppearance(current?: string): string {
  const candidates = appearances.filter((item) => item.id !== current);
  return candidates[Math.floor(Math.random() * candidates.length)].id;
}

const AppearanceContext = createContext<{
  choices: Choices;
  choose: (actor: string, appearance?: string) => void;
}>({ choices: {}, choose: () => {} });

// Внешность — локальная настройка отображения на один заказ. Не пишет в общий чат,
// не запускает агента и не переписывает историю. Provider перемонтируется по scope.
export function AppearanceProvider({
  scope,
  children,
}: {
  scope: string;
  children: ReactNode;
}) {
  return (
    <ScopedAppearanceProvider key={scope} scope={scope}>
      {children}
    </ScopedAppearanceProvider>
  );
}
function ScopedAppearanceProvider({
  scope,
  children,
}: {
  scope: string;
  children: ReactNode;
}) {
  const [choices, setChoices] = useState(() => readAppearanceChoices(scope));
  const choose = (actor: string, appearance?: string) => {
    if (appearance && !appearances.some((item) => item.id === appearance))
      return;
    const next = { ...choices };
    if (appearance) next[actor] = appearance;
    else delete next[actor];
    setChoices(next);
    try {
      localStorage.setItem(storageKey(scope), JSON.stringify(next));
    } catch {
      // При недоступном хранилище выбор остаётся действующим до перезагрузки.
    }
  };
  return (
    <AppearanceContext.Provider value={{ choices, choose }}>
      {children}
    </AppearanceContext.Provider>
  );
}
export function useAppearances() {
  return useContext(AppearanceContext);
}
export function useEmployeeSprite(
  actor: string,
  fallback = actor,
): string | undefined {
  const { choices } = useAppearances();
  return (
    appearances.find((item) => item.id === choices[actor])?.sprite ||
    defaults[fallback]
  );
}
