import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { AppTheme } from './Theme';
import { TeamPhone, wordCount, type TeamChat } from './TeamPhone';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
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
  expect(screen.getByText('Цель команды')).toBeVisible();
  const pin = screen.getByRole('button', { name: 'Развернуть цель' });
  expect(pin).toHaveAttribute('aria-expanded', 'false');
  fireEvent.click(pin);
  expect(screen.getByRole('button', { name: 'Свернуть цель' })).toHaveAttribute(
    'aria-expanded',
    'true',
  );
  expect(
    within(screen.getByRole('log')).getByRole('button', {
      name: 'Свернуть цель',
    }),
  ).toBeVisible();
  // Клик по полному тексту сворачивает карточку; копирование не меняет её вид.
  fireEvent.click(screen.getByText(chat.goal));
  expect(
    screen.getByRole('button', { name: 'Развернуть цель' }),
  ).toHaveAttribute('aria-expanded', 'false');
  const copy = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText: copy },
  });
  fireEvent.click(screen.getByRole('button', { name: 'Скопировать цель' }));
  await waitFor(() => expect(copy).toHaveBeenCalledWith(chat.goal));
  expect(
    screen.getByRole('button', { name: 'Развернуть цель' }),
  ).toHaveAttribute('aria-expanded', 'false');
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

// Телефон открывает только сохранённые заказы. Выбор из списка не отправляет
// POST и не меняет конфиг — в UI нет формы создания даже без выбранного run.
it('открывает существующий заказ без создания команды', async () => {
  const fetcher = vi.fn(async (url: string, options?: RequestInit) => {
    expect(options?.method).not.toBe('POST');
    return response(
      url === '/api/teams'
        ? { teams: [{ id: 'order', goal: chat.goal }], problems: [] }
        : { ...chat, messages: [] },
    );
  });
  vi.stubGlobal('fetch', fetcher);
  render(
    <AppTheme>
      <TeamPhone onClose={() => {}} />
    </AppTheme>,
  );
  fireEvent.click(await screen.findByRole('button', { name: chat.goal }));
  expect(
    await screen.findByRole('textbox', { name: 'Сообщение команде' }),
  ).toBeVisible();
  expect(
    screen.queryByRole('button', { name: 'Закрепить цель' }),
  ).not.toBeInTheDocument();
  expect(
    screen.queryByRole('button', { name: 'Загрузить состав команды' }),
  ).not.toBeInTheDocument();
  expect(window.location.search).toBe('?run=order');
});

// Адрес выбирается среди присутствующих сотрудников; технические реплики не
// маскируются под человека, изображение автора разрешается по реестру команды.
it('подставляет тег и показывает приглашение с аватаром Разработчика', async () => {
  window.history.replaceState(null, '', '/office?run=order');
  const room: TeamChat = {
    ...chat,
    members: {
      human: { name: 'Чел' },
      boss: { name: 'Босс', avatar: 'boss' },
      developer: { name: 'Разработчик', avatar: 'developer' },
    },
    room: {
      actors: {
        boss: { status: 'monitoring', nextCheck: '' },
        developer: { status: 'idle', nextCheck: '' },
      },
    },
    messages: [
      {
        id: 'system',
        kind: 'system',
        authorId: 'system',
        date: '2026-09-17T09:30:00Z',
        text: 'Босс пригласил Разработчика',
      },
      {
        id: 'dev',
        kind: 'reply',
        authorId: 'developer',
        to: 'boss',
        date: '2026-09-17T09:31:00Z',
        text: '@boss Готово',
      },
    ],
  };
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) =>
      response(
        url === '/api/teams'
          ? { teams: [], cwd: '/project', problems: [] }
          : room,
      ),
    ),
  );
  render(
    <AppTheme>
      <TeamPhone onClose={() => {}} />
    </AppTheme>,
  );
  expect(await screen.findByText('Босс пригласил Разработчика')).toBeVisible();
  expect(
    screen.getByLabelText('Аватар: Разработчик').querySelector('img'),
  ).toHaveAttribute('src', expect.stringContaining('developer.png'));
  const field = screen.getByRole('textbox', { name: 'Сообщение команде' });
  fireEvent.change(field, { target: { value: 'Проверь прыжок' } });
  fireEvent.click(screen.getByRole('button', { name: '@developer' }));
  expect(field).toHaveValue('@developer Проверь прыжок');
  fireEvent.click(screen.getByRole('button', { name: '@boss' }));
  expect(field).toHaveValue('@boss Проверь прыжок');
});

// Лейбл относится к следующей проверке, а не к работе модели. После достижения
// даже устаревшие даты не должны показывать таймер; статус остаётся доступным по title.
it.each([false, true])(
  'показывает точки и только действующие таймеры: достигнута=%s',
  async (achieved) => {
    const now = Date.now();
    vi.spyOn(Date, 'now').mockReturnValue(now);
    const nextCheck = new Date(now + 300000).toISOString();
    window.history.replaceState(null, '', '/office?run=order');
    const data: TeamChat = {
      ...chat,
      room: {
        achievedAt: achieved ? new Date(now).toISOString() : undefined,
        actors: {
          boss: { status: 'monitoring', nextCheck },
          developer: { status: 'working', nextCheck },
          waiting: { status: 'idle', nextCheck: '0001-01-01T00:00:00Z' },
          blocked: { status: 'blocked', nextCheck },
        },
      },
    };
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) =>
        response(
          url === '/api/teams'
            ? { teams: [], cwd: '/project', problems: [] }
            : data,
        ),
      ),
    );
    render(
      <AppTheme>
        <TeamPhone onClose={() => {}} />
      </AppTheme>,
    );
    const boss = await screen.findByRole('button', { name: '@boss' });
    expect(boss.querySelector('.team-recipient-dot')).toHaveClass(
      achieved ? 'team-recipient-dot_idle' : 'team-recipient-dot_monitoring',
    );
    expect(
      screen
        .getByRole('button', { name: '@developer' })
        .querySelector('.team-recipient-dot'),
    ).toHaveClass(
      achieved ? 'team-recipient-dot_idle' : 'team-recipient-dot_working',
    );
    expect(screen.queryAllByText('5:00')).toHaveLength(achieved ? 0 : 1);
    expect(screen.getByRole('button', { name: '@waiting' })).toHaveAttribute(
      'title',
      'Ждёт обращения',
    );
    expect(screen.getByRole('button', { name: '@blocked' })).toHaveAttribute(
      'title',
      achieved ? 'Ждёт обращения' : 'Нужна помощь',
    );
    const field = screen.getByRole('textbox', { name: 'Сообщение команде' });
    const hint = screen.getByText('Агенты отвечают только на явный @тег');
    expect(
      field.compareDocumentPosition(hint) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  },
);

// Немодальное окно не захватывает фон. Геометрия меняется без пересоздания
// чата: черновик и его фокус сохраняются при движении и изменении размера.
it('показывает плавающее окно с доступным фоном, перемещением и ресайзом', async () => {
  window.history.replaceState(null, '', '/office?run=order');
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) =>
      response(
        url === '/api/teams'
          ? { teams: [], cwd: '/project', problems: [] }
          : chat,
      ),
    ),
  );
  const outside = vi.fn();
  const close = vi.fn();
  render(
    <AppTheme>
      <button onClick={outside}>Плеер</button>
      <TeamPhone onClose={close} />
    </AppTheme>,
  );
  const field = await screen.findByRole('textbox', {
    name: 'Сообщение команде',
  });
  fireEvent.change(field, { target: { value: 'Черновик' } });
  const phone = screen.getByRole('dialog', { name: 'Чат команды' });
  expect(phone).not.toHaveAttribute('aria-modal', 'true');
  const left = parseFloat(phone.style.left);
  const width = parseFloat(phone.style.width);
  fireEvent.keyDown(
    screen.getByRole('button', { name: 'Переместить телефон' }),
    { key: 'ArrowLeft' },
  );
  expect(parseFloat(phone.style.left)).toBe(left - 10);
  fireEvent.keyDown(
    screen.getByRole('button', { name: 'Изменить размер телефона' }),
    { key: 'ArrowRight', shiftKey: true },
  );
  expect(parseFloat(phone.style.width)).toBe(width + 40);
  expect(field).toHaveValue('Черновик');
  fireEvent.click(screen.getByRole('button', { name: 'Плеер' }));
  expect(outside).toHaveBeenCalledOnce();
  expect(close).not.toHaveBeenCalled();
});
