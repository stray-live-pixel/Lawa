import { afterEach, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { appearances, useEmployeeSprite } from './appearances';

afterEach(() => {
  cleanup();
  localStorage.clear();
});

// Старая персонализация в localStorage не подменяет конфиг запущенного заказа.
it('отображает образ из конфига, игнорируя настройки браузера', () => {
  localStorage.setItem(
    'lawa-office-appearance-v1:order',
    JSON.stringify({ designer: 'cat-black' }),
  );
  function Employee() {
    return (
      <>
        <img
          alt="Сцена"
          src={useEmployeeSprite('designer', 'pixel-designer')}
        />
        <img alt="Чат" src={useEmployeeSprite('designer', 'pixel-designer')} />
      </>
    );
  }
  render(<Employee />);
  expect(screen.getByAltText('Сцена')).toHaveAttribute(
    'src',
    expect.stringContaining('pixel-designer.png'),
  );
  expect(screen.getByAltText('Чат')).toHaveAttribute(
    'src',
    screen.getByAltText('Сцена').getAttribute('src'),
  );
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
