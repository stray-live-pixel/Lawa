import {
  act,
  cleanup,
  fireEvent,
  renderHook,
  render,
  screen,
  within,
  waitFor,
} from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { historyFrames, teamAt, useTeamPlayer } from './TeamPlayer';
import { type TeamChat } from './TeamPhone';
import { AppTheme } from './Theme';
import Office from '../pages/Office';
const start = Date.parse('2026-09-17T00:00:00Z');
const date = (s: number) => new Date(start + s * 1000).toISOString();
const chat: TeamChat = {
  runId: 'order',
  goal: 'Игра',
  members: {
    boss: { name: 'Босс' },
    developer: { name: 'Разработчик' },
    human: { name: 'Чел' },
  },
  room: {
    actors: {
      boss: { status: 'idle', nextCheck: '' },
      developer: { status: 'idle', nextCheck: '' },
    },
  },
  messages: [
    { id: 'goal', authorId: 'human', date: date(0), text: 'Начальная задача' },
    {
      id: 'summon-developer',
      authorId: 'system',
      kind: 'system',
      date: date(10),
      text: 'Разработчик присоединился',
    },
    { id: 'done', authorId: 'developer', date: date(20), text: 'Игра готова' },
  ],
  history: {
    recordedFrom: date(0),
    recovered: false,
    frames: [
      {
        at: date(0),
        messageCount: 1,
        actors: {
          boss: { status: 'working', summary: 'Планирует игру', nextCheck: '' },
        },
      },
      {
        at: date(10),
        messageCount: 2,
        actors: {
          boss: { status: 'monitoring', nextCheck: '' },
          developer: {
            status: 'working',
            summary: 'Создаёт платформы',
            nextCheck: '',
          },
        },
      },
      {
        at: date(20),
        messageCount: 3,
        actors: {
          boss: { status: 'idle', nextCheck: '' },
          developer: { status: 'idle', nextCheck: '' },
        },
      },
    ],
  },
};
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.replaceState(null, '', '/office');
});
// Будущие сообщения и сотрудники не попадают в прошлое. Нет данных — unknown.
it('выбирает согласованный кадр', () => {
  const frames = historyFrames(chat);
  expect(teamAt(chat, frames, start - 1).messages).toHaveLength(0);
  expect(
    teamAt(chat, frames, start + 9999).room?.actors.developer,
  ).toBeUndefined();
  const selected = teamAt(chat, frames, start + 10000);
  expect(selected.messages).toHaveLength(2);
  expect(selected.room?.actors.developer.summary).toBe('Создаёт платформы');
  expect(
    historyFrames({ ...chat, history: undefined })[0].actors.boss.status,
  ).toBe('unknown');
});
// Polling не сдвигает курсор; смена заказа сбрасывает воспроизведение.
it('воспроизводит и возвращает живой снимок', () => {
  vi.useFakeTimers();
  vi.setSystemTime(start + 30000);
  const { result, rerender } = renderHook(({ data }) => useTeamPlayer(data), {
    initialProps: { data: chat },
  });
  act(() => result.current.toggle());
  act(() => vi.advanceTimersByTime(500));
  expect(result.current.at).toBe(start + 10000);
  act(() => result.current.toggle());
  act(() => vi.advanceTimersByTime(500));
  expect(result.current.at).toBe(start + 10000);
  const end = result.current.end;
  rerender({
    data: {
      ...chat,
      messages: [
        ...chat.messages,
        { id: 'future', authorId: 'boss', text: 'Будущее', date: date(31) },
      ],
    },
  });
  expect(result.current.end).toBe(end);
  expect(result.current.view?.messages).toHaveLength(2);
  act(() => result.current.step(-1));
  expect(result.current.at).toBe(start);
  act(() => result.current.live());
  expect(result.current.view?.messages).toHaveLength(4);
  act(() => result.current.seek(start));
  rerender({ data: { ...chat, runId: 'other' } });
  rerender({ data: chat });
  expect(result.current.historical).toBe(false);
});
// Настоящий Office и телефон читают один кадр; отправка всегда относится к текущему чату.
it('перематывает сцену и телефон вместе без запуска агентов', async () => {
  window.history.replaceState(null, '', '/office?run=order');
  const fetch = vi.fn(async (url: string, options?: RequestInit) => {
    expect(options?.method).not.toBe('POST');
    return new Response(
      JSON.stringify(
        url === '/api/teams'
          ? {
              teams: [{ id: 'order', goal: 'Игра' }],
              cwd: '/project',
              problems: [],
            }
          : chat,
      ),
    );
  });
  vi.stubGlobal('fetch', fetch);
  render(
    <AppTheme>
      <Office />
    </AppTheme>,
  );
  await screen.findByRole('button', { name: 'Разработчик: Ждёт' });
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Предыдущее событие' }));
  await user.click(screen.getByRole('button', { name: 'Предыдущее событие' }));
  expect(
    screen.getByRole('button', { name: 'Разработчик: Работает' }),
  ).toBeVisible();
  expect(screen.getByText('Создаёт платформы')).toBeVisible();
  await user.click(screen.getByRole('button', { name: 'Открыть чат команды' }));
  const phone = within(screen.getByRole('dialog', { name: 'Чат команды' }));
  expect(phone.queryByText('Игра готова')).not.toBeInTheDocument();
  await waitFor(() =>
    expect(
      phone.getByRole('textbox', { name: 'Сообщение команде' }),
    ).toBeVisible(),
  );
  expect(
    phone.queryByRole('region', { name: 'Плеер команды' }),
  ).not.toBeInTheDocument();
  await user.click(phone.getByRole('button', { name: 'Закрыть чат' }));
  await user.click(screen.getByRole('button', { name: 'Предыдущее событие' }));
  expect(
    screen.queryByAltText('Разработчик за MacBook'),
  ).not.toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: 'К текущему' }));
  await user.click(screen.getByRole('button', { name: 'Открыть чат команды' }));
  const livePhone = within(screen.getByRole('dialog', { name: 'Чат команды' }));
  expect(await livePhone.findByText('Игра готова')).toBeVisible();
  expect(
    livePhone.getByRole('textbox', { name: 'Сообщение команде' }),
  ).toBeVisible();
});

// Галочка относится к времени достижения, а не ко всей истории заказа.
it('показывает достигнутую цель в офисе и pin, но не переносит её в прошлое', async () => {
  window.history.replaceState(null, '', '/office?run=order');
  const achieved: TeamChat = {
    ...chat,
    room: { ...chat.room!, achievedAt: date(20) },
    history: {
      ...chat.history!,
      frames: chat.history!.frames.map((f, i) =>
        i === 2 ? { ...f, achievedAt: date(20) } : f,
      ),
    },
    messages: [
      ...chat.messages.slice(0, 2),
      {
        id: 'achievement',
        authorId: 'system',
        kind: 'achievement',
        text: 'Босс отметил цель достигнутой.',
        date: date(20),
      },
      {
        id: 'done',
        authorId: 'boss',
        to: 'human',
        text: '@human Игра готова',
        date: date(20),
      },
    ],
  };
  achieved.history!.frames[2].messageCount = 4;
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async (url: string) =>
        new Response(
          JSON.stringify(
            url === '/api/teams'
              ? {
                  teams: [{ id: 'order', goal: 'Игра' }],
                  cwd: '/project',
                  problems: [],
                }
              : achieved,
          ),
        ),
    ),
  );
  render(
    <AppTheme>
      <Office />
    </AppTheme>,
  );
  await screen.findByText('Цель достигнута');
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Открыть чат команды' }));
  const phone = within(screen.getByRole('dialog', { name: 'Чат команды' }));
  await waitFor(() =>
    expect(phone.getByText('Цель команды').closest('section')).toHaveClass(
      'team-pin-achieved',
    ),
  );
  expect(phone.getByText('Босс отметил цель достигнутой.')).toBeVisible();
  await waitFor(() =>
    expect(
      phone.getByRole('textbox', { name: 'Сообщение команде' }),
    ).toBeVisible(),
  );
  expect(phone.queryByRole('slider')).not.toBeInTheDocument();
  await user.click(phone.getByRole('button', { name: 'Закрыть чат' }));
  await user.click(screen.getByRole('button', { name: 'Предыдущее событие' }));
  expect(screen.queryByText('Цель достигнута')).not.toBeInTheDocument();
  expect(
    screen.queryByText('Восстановлено по ходам Codex и сообщениям'),
  ).not.toBeInTheDocument();
});

// Даже исторический кадр завершённого заказа не блокирует новый вопрос.
it('отправляет из истории в текущий чат и возвращает live', async () => {
  window.history.replaceState(null, '', '/office?run=order');
  const completed = { ...chat, room: { ...chat.room!, achievedAt: date(20) } };
  const sent: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, options?: RequestInit) => {
      if (options?.method === 'POST') {
        expect(url).toBe('/api/teams/order/messages');
        const message = JSON.parse(String(options.body));
        sent.push(message.text);
        return new Response(
          JSON.stringify({ ...message, authorId: 'human', date: date(30) }),
        );
      }
      return new Response(
        JSON.stringify(
          url === '/api/teams'
            ? { teams: [], cwd: '/project', problems: [] }
            : completed,
        ),
      );
    }),
  );
  render(
    <AppTheme>
      <Office />
    </AppTheme>,
  );
  await screen.findByText('Цель достигнута');
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Предыдущее событие' }));
  await user.click(screen.getByRole('button', { name: 'Открыть чат команды' }));
  const phone = within(screen.getByRole('dialog', { name: 'Чат команды' }));
  await waitFor(() => expect(phone.getByText('История')).toBeVisible());
  expect(phone.getByRole('button', { name: 'К текущему чату' })).toBeVisible();
  fireEvent.change(phone.getByRole('textbox', { name: 'Сообщение команде' }), {
    target: { value: '@developer Как работает прыжок?' },
  });
  await user.click(phone.getByRole('button', { name: 'Отправить сообщение' }));
  await waitFor(() =>
    expect(phone.queryByText('История')).not.toBeInTheDocument(),
  );
  expect(sent).toEqual(['@developer Как работает прыжок?']);
  expect(phone.getByRole('textbox', { name: 'Сообщение команде' })).toHaveValue(
    '',
  );
});

it('сохраняет прежнюю цель при перемотке после смены pin', () => {
  const updated = { ...chat, goal: 'Второй уровень' };
  const frames = historyFrames(chat).map((f) => ({ ...f, goal: 'Игра' }));
  expect(teamAt(updated, frames, start + 10000).goal).toBe('Игра');
});
