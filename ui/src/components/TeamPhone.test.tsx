import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { AppTheme } from './Theme';
import { TeamPhone, wordCount, type TeamChat } from './TeamPhone';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState(null, '', '/office');
});
const chat: TeamChat = {
  runId: 'order',
  goal: 'Создать платформер',
  members: {
    human: { name: 'Чел' },
    'order:character:boss': { name: 'Босс', avatar: 'boss' },
  },
  messages: [
    {
      id: 'agent',
      authorId: 'order:character:boss',
      date: '2026-09-17T09:30:00Z',
      text: 'Начинаем с управления героем',
    },
  ],
};
const response = (value: unknown) =>
  new Response(JSON.stringify(value), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });

// Лимит совпадает с Go для пробелов Unicode; не режем русский текст по байтам.
it('считает слова по Unicode-пробелам', () => {
  expect(wordCount(' \n')).toBe(0);
  expect(wordCount('Раз\u0085два\u00a0три')).toBe(3);
  expect(wordCount('слово '.repeat(50))).toBe(50);
});

// После сетевой неопределённости повтор использует прежний ID. Человек не
// выбирает автора, а сообщения других участников берут имя из общего реестра.
it('показывает pin и автора, проверяет 50 слов и безопасно повторяет отправку', async () => {
  window.history.replaceState(null, '', '/office?run=order');
  const sent: { id: string; text: string }[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, options?: RequestInit) => {
      if (options?.method === 'POST') {
        const input = JSON.parse(String(options.body));
        sent.push(input);
        if (sent.length === 1) throw new Error('Сеть недоступна');
        return response({
          ...input,
          authorId: 'human',
          date: '2026-09-17T10:00:00Z',
        });
      }
      return response(
        url === '/api/teams'
          ? {
              teams: [{ id: 'order', goal: chat.goal }],
              cwd: '/project',
              problems: [],
            }
          : chat,
      );
    }),
  );
  const onClose = vi.fn();
  render(
    <AppTheme>
      <TeamPhone onClose={onClose} />
    </AppTheme>,
  );
  expect(await screen.findByText('Начинаем с управления героем')).toBeVisible();
  expect(screen.getByText('Босс')).toBeVisible();
  expect(screen.getByText('Цель команды · закреплено')).toBeVisible();
  const field = screen.getByRole('textbox', { name: 'Сообщение команде' });
  const send = screen.getByRole('button', { name: 'Отправить сообщение' });
  fireEvent.change(field, { target: { value: 'слово '.repeat(51) } });
  expect(send).toBeDisabled();
  fireEvent.change(field, { target: { value: 'Проверено' } });
  fireEvent.click(send);
  expect(await screen.findByText('Error: Сеть недоступна')).toBeVisible();
  await waitFor(() => expect(send).toBeEnabled());
  fireEvent.click(send);
  expect(await screen.findByText('Проверено')).toBeVisible();
  expect(sent).toHaveLength(2);
  expect(sent[0]).toEqual(sent[1]);
  expect(Object.keys(sent[0]).sort()).toEqual(['id', 'text']);
  expect(field).toHaveValue('');
  await userEvent.setup().keyboard('{Escape}');
  expect(onClose).toHaveBeenCalled();
});

// Создание из пустого телефона отправляет только цель и выбранную папку;
// дальше открывает чат сохранённого заказа без команды запуска агентов.
it('создаёт заказ из введённой цели и открывает его чат', async () => {
  const sent: unknown[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, options?: RequestInit) => {
      if (options?.method === 'POST') {
        expect(url).toBe('/api/teams');
        sent.push(JSON.parse(String(options.body)));
        return response({ runId: 'order' });
      }
      return response(
        url === '/api/teams'
          ? { teams: [], cwd: '/project', problems: [] }
          : { ...chat, messages: [] },
      );
    }),
  );
  render(
    <AppTheme>
      <TeamPhone onClose={() => {}} />
    </AppTheme>,
  );
  await waitFor(() =>
    expect(screen.getByRole('textbox', { name: 'Папка проекта' })).toHaveValue(
      '/project',
    ),
  );
  fireEvent.change(screen.getByRole('textbox', { name: 'Цель команды' }), {
    target: { value: chat.goal },
  });
  fireEvent.click(screen.getByRole('button', { name: 'Закрепить цель' }));
  expect(
    await screen.findByRole('textbox', { name: 'Сообщение команде' }),
  ).toBeVisible();
  expect(sent).toEqual([{ goal: chat.goal, cwd: '/project' }]);
  expect(window.location.search).toBe('?run=order');
});
