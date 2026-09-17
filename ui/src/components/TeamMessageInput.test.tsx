import { useState } from 'react';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { AppTheme } from './Theme';
import { TeamMessageInput } from './TeamMessageInput';

afterEach(cleanup);

// Настоящее управляемое поле проверяет и замену черновика, и позицию курсора.
function setup(initial = '') {
  const submit = vi.fn();
  const escape = vi.fn();
  function Form() {
    const [value, setValue] = useState(initial);
    return (
      <AppTheme>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            submit();
          }}
          onKeyDown={(event) => {
            if (event.key === 'Escape') escape();
          }}
        >
          <TeamMessageInput
            value={value}
            onUpdate={setValue}
            employees={[
              { id: 'boss', name: 'Босс' },
              { id: 'developer', name: 'Разработчик' },
            ]}
            renderAvatar={(id) => <span aria-label={`Аватар: ${id}`} />}
            busy={false}
            canSend={Boolean(value)}
            placeholder="Сообщение"
          />
        </form>
      </AppTheme>
    );
  }
  render(<Form />);
  return {
    field: screen.getByRole('textbox', {
      name: 'Сообщение команде',
    }) as HTMLTextAreaElement,
    user: userEvent.setup(),
    submit,
    escape,
  };
}

it('открывает список с аватарами, выбирает стрелками и Enter без отправки', async () => {
  const { field, user, submit } = setup();
  await user.type(field, '@');
  expect(screen.getAllByRole('option')).toHaveLength(2);
  expect(screen.getByLabelText('Аватар: developer')).toBeVisible();
  await user.keyboard('{ArrowUp}');
  expect(
    screen.getByRole('option', { name: 'Разработчик @developer' }),
  ).toHaveAttribute('aria-selected', 'true');
  await user.keyboard('{ArrowDown}{ArrowDown}{Enter}');
  expect(field).toHaveValue('@developer ');
  expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
  expect(submit).not.toHaveBeenCalled();
  await user.type(field, 'Проверь игру');
  await user.click(screen.getByRole('button', { name: 'Отправить сообщение' }));
  expect(submit).toHaveBeenCalledOnce();
});

it.each(['РАЗ', 'dev'])(
  'фильтрует имя и ID (%s), выбор мышью сохраняет текст',
  async (query) => {
    const { field, user } = setup(`@${query} Проверь игру`);
    await user.click(field);
    field.setSelectionRange(query.length + 1, query.length + 1);
    fireEvent.select(field);
    expect(screen.getAllByRole('option')).toHaveLength(1);
    await user.click(
      screen.getByRole('option', { name: 'Разработчик @developer' }),
    );
    expect(field).toHaveValue('@developer Проверь игру');
  },
);

it('Escape закрывает только подсказку, обычный Enter сохраняет перенос строки', async () => {
  const { field, user, escape, submit } = setup();
  await user.type(field, '@');
  await user.keyboard('{Escape}');
  expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
  expect(escape).not.toHaveBeenCalled();
  await user.keyboard('{Enter}');
  expect(field).toHaveValue('@\n');
  expect(submit).not.toHaveBeenCalled();
});

it('не подставляет сотрудника при пустом поиске и не перехватывает ввод IME', async () => {
  const { field, user, submit } = setup();
  await user.type(field, '@nobody');
  expect(screen.getByText('Сотрудник не найден')).toBeVisible();
  await user.keyboard('{Enter}');
  expect(field).toHaveValue('@nobody');
  await user.clear(field);
  await user.type(field, '@');
  fireEvent.keyDown(field, { key: 'Enter', isComposing: true });
  expect(field).toHaveValue('@');
  expect(submit).not.toHaveBeenCalled();
});

// Подсветка означает реального адресата, а не просто похожее имя или часть ID.
it.each([
  ['@boss Проверь игру', '@boss'],
  ['@developer', '@developer'],
  ['@dev Проверь игру', null],
  ['@developerX Проверь игру', null],
  ['@unknown Проверь игру', null],
  ['@Разработчик Проверь игру', null],
  ['Проверь игру @boss', null],
])('подсвечивает только полный ID адресата: %s', (draft, expected) => {
  const { field } = setup(draft);
  const colored = field
    .closest('.team-compose-row')!
    .querySelector('.team-compose-mention');
  expect(colored?.textContent || null).toBe(expected);
  expect(field).toHaveValue(draft);
});

it('обновляет подсветку при редактировании тега и сохраняет многострочный черновик', () => {
  const { field } = setup('@developer Строка\nещё строка');
  const mirror = field
    .closest('.team-compose-row')!
    .querySelector('.team-compose-highlights')!;
  expect(mirror).toHaveAttribute('aria-hidden', 'true');
  fireEvent.change(field, {
    target: { value: '@develope Строка\nещё строка' },
  });
  expect(mirror.querySelector('.team-compose-mention')).toBeNull();
  fireEvent.change(field, { target: { value: '@boss Строка\nещё строка' } });
  expect(mirror.querySelector('.team-compose-mention')).toHaveTextContent(
    '@boss',
  );
  expect(field).toHaveValue('@boss Строка\nещё строка');
});
