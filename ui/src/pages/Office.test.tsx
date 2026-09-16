import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it } from 'vitest';
import { AppTheme } from '../components/Theme';
import Office from './Office';

afterEach(cleanup);

// Проверяем весь демо-цикл с клавиатуры: состояние доступно без различения
// цветов, а возвращение в ожидание убирает прежнюю активную реплику.
it('переключает состояния Босса и очищает облачко в ожидании', async () => {
  const user = userEvent.setup();
  render(
    <AppTheme>
      <Office />
    </AppTheme>,
  );
  const boss = screen.getByRole('button', { name: 'Босс: Ждёт' });
  expect(screen.getByRole('status')).toBeEmptyDOMElement();
  boss.focus();
  await user.keyboard('{Enter}');
  expect(boss).toHaveAccessibleName('Босс: Работает');
  expect(screen.getByRole('status')).toHaveTextContent('Изучаю задачу');
  await user.keyboard(' ');
  expect(boss).toHaveAccessibleName('Босс: Мониторит');
  expect(screen.getByRole('status')).toHaveTextContent('Слежу за командой');
  await user.click(boss);
  expect(boss).toHaveAccessibleName('Босс: Ждёт');
  expect(screen.getByRole('status')).toBeEmptyDOMElement();
});
