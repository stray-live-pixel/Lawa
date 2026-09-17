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
// Отображаем сохранённую внешность заказа. Локальные настройки браузера не
// читаются: конфигурация приходит только из CLI, одинаково для всех наблюдателей.
export function useEmployeeSprite(
  actor: string,
  fallback = actor,
): string | undefined {
  return (
    characterGallery.find((item) => item.id === fallback)?.sprite ||
    // Неизвестный ID внешности не прячет рабочее место. Служебные авторы
    // human/system сохраняют буквенную аватарку; сотрудник получает базовый образ.
    (actor !== 'human' && actor !== 'system'
      ? actor === 'boss'
        ? boss
        : developer
      : undefined)
  );
}
