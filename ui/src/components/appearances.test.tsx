import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AppTheme } from './Theme';
import { AppearancePicker } from './AppearancePicker';
import {
  AppearanceProvider,
  appearances,
  randomAppearance,
  readAppearanceChoices,
  useEmployeeSprite,
} from './appearances';

afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.restoreAllMocks();
});

// UI и чат должны получать один образ, а соседний сотрудник — сохранять свой.
function Sprites() {
  return (
    <>
      <img alt="Сцена" src={useEmployeeSprite('boss')} />
      <img alt="Чат" src={useEmployeeSprite('boss')} />
      <img alt="Разработчик" src={useEmployeeSprite('developer')} />
    </>
  );
}
function Room({ scope = 'order' }: { scope?: string }) {
  return (
    <AppTheme>
      <AppearanceProvider scope={scope}>
        <AppearancePicker
          members={{
            boss: { name: 'Босс' },
            developer: { name: 'Разработчик' },
          }}
        />
        <Sprites />
      </AppearanceProvider>
    </AppTheme>
  );
}

it('меняет обе поверхности, сохраняет выбор и изолирует его между заказами', async () => {
  const user = userEvent.setup();
  const view = render(<Room />);
  const original = screen.getByAltText('Сцена').getAttribute('src');
  const developer = screen.getByAltText('Разработчик').getAttribute('src');
  await user.click(
    screen.getByRole('button', { name: 'Внешность сотрудников' }),
  );
  await user.click(
    screen.getByRole('button', { name: 'Выбрать образ: Детектив багов' }),
  );
  expect(screen.getByAltText('Сцена')).not.toHaveAttribute('src', original);
  expect(screen.getByAltText('Чат')).toHaveAttribute(
    'src',
    screen.getByAltText('Сцена').getAttribute('src'),
  );
  expect(screen.getByAltText('Разработчик')).toHaveAttribute('src', developer);
  expect(readAppearanceChoices('order')).toEqual({ boss: 'qa-detective' });
  view.unmount();
  const next = render(<Room />);
  expect(screen.getByAltText('Сцена')).not.toHaveAttribute('src', original);
  next.rerender(<Room scope="another-order" />);
  expect(screen.getByAltText('Сцена')).toHaveAttribute('src', original);
});

it('фильтрует коллекцию и восстанавливает исходный образ', async () => {
  const user = userEvent.setup();
  render(<Room />);
  const original = screen.getByAltText('Сцена').getAttribute('src');
  await user.click(
    screen.getByRole('button', { name: 'Внешность сотрудников' }),
  );
  await user.type(
    screen.getByRole('textbox', { name: 'Поиск внешности' }),
    'Детектив',
  );
  expect(
    screen.getAllByRole('button', { name: /Выбрать образ:/ }),
  ).toHaveLength(1);
  await user.click(
    screen.getByRole('button', { name: 'Выбрать образ: Детектив багов' }),
  );
  await user.click(
    screen.getByRole('button', { name: 'Вернуть исходный образ' }),
  );
  expect(screen.getByAltText('Сцена')).toHaveAttribute('src', original);
  expect(readAppearanceChoices('order')).toEqual({});
});

it('случайный выбор не повторяет текущий образ на обоих краях диапазона', () => {
  vi.spyOn(Math, 'random').mockReturnValue(0);
  expect(randomAppearance(appearances[0].id)).toBe(appearances[1].id);
  vi.mocked(Math.random).mockReturnValue(0.999999);
  expect(randomAppearance(appearances.at(-1)!.id)).toBe(appearances.at(-2)!.id);
});

it('игнорирует повреждённые данные и внешние URL в хранилище', () => {
  localStorage.setItem('lawa-office-appearance-v1:order', '{broken');
  expect(readAppearanceChoices('order')).toEqual({});
  localStorage.setItem(
    'lawa-office-appearance-v1:order',
    JSON.stringify({
      boss: 'https://example.test/pixel.png',
      developer: 'qa-detective',
    }),
  );
  expect(readAppearanceChoices('order')).toEqual({ developer: 'qa-detective' });
});

// Неполная поставка каталога сломала бы выбор только после клика: проверяем все
// ассеты, уникальность ID и точное количество заказанных людей и животных.
it('поставляет 30 людей и по два животных каждого вида с изображениями', () => {
  expect(appearances).toHaveLength(40);
  expect(new Set(appearances.map((item) => item.id)).size).toBe(40);
  expect(appearances.filter((item) => item.kind === 'human')).toHaveLength(30);
  for (const profession of ['Кот', 'Собака', 'Попугай', 'Медведь', 'Олень']) {
    expect(
      appearances.filter((item) => item.profession === profession),
    ).toHaveLength(2);
  }
  for (const item of appearances) expect(item.sprite).toBeTruthy();
});
