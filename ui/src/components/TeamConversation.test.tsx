import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { AppTheme } from './Theme';
import { TeamConversation, conversationBoundary } from './TeamConversation';
import { teamAt, historyFrames } from './TeamPlayer';
import { type TeamChat, type TeamMessage } from './TeamPhone';
afterEach(cleanup);
const messages: TeamMessage[] = [
  {
    id: 'old',
    authorId: 'human',
    date: '2026-09-22T08:00:00Z',
    text: 'Исходный открытый вопрос',
  },
  {
    id: 'fresh',
    authorId: 'boss',
    date: '2026-09-22T08:01:00Z',
    text: 'Свежий ответ',
  },
  {
    id: 'summary1',
    authorId: 'boss',
    date: '2026-09-22T08:02:00Z',
    text: 'Сжатие',
    summary: {
      requestId: 'one',
      through: 1,
      text: 'Вопрос ещё открыт',
      sourceIds: ['old'],
    },
  },
  {
    id: 'later',
    authorId: 'human',
    date: '2026-09-22T08:03:00Z',
    text: 'Позднее сообщение',
  },
  {
    id: 'summary2',
    authorId: 'boss',
    date: '2026-09-22T08:04:00Z',
    text: 'Сжатие',
    summary: {
      requestId: 'two',
      through: 3,
      text: 'Вопрос остаётся открытым',
      sourceIds: ['old', 'fresh'],
    },
  },
];
// Один cut содержит исходники и старую версию ровно один раз; источник раскрывает
// архив и переводит фокус на оригинал, а новый кадр не содержит будущей сводки.
it('раскрывает оригинал из второй сводки без дублей и сохраняет версии', () => {
  Element.prototype.scrollIntoView = vi.fn();
  render(
    <AppTheme>
      <TeamConversation
        messages={messages}
        preserveReading={false}
        renderMessage={(m) => (
          <article key={m.id} id={`team-message-${m.id}`} tabIndex={-1}>
            {m.text}
          </article>
        )}
      />
    </AppTheme>,
  );
  expect(
    screen.queryByText('Исходный открытый вопрос'),
  ).not.toBeInTheDocument();
  expect(screen.getByText('Вопрос остаётся открытым')).toBeVisible();
  fireEvent.click(screen.getByRole('button', { name: /^old$/ }));
  expect(screen.getAllByText('Исходный открытый вопрос')).toHaveLength(1);
  expect(screen.getByText('Вопрос ещё открыт')).toBeVisible();
  expect(document.activeElement?.id).toBe('team-message-old');
  fireEvent.click(screen.getByRole('button', { name: /Скрыть прошлую/ }));
  expect(
    screen.queryByText('Исходный открытый вопрос'),
  ).not.toBeInTheDocument();
});
it('исторический курсор выбирает только уже опубликованную границу', () => {
  const chat: TeamChat = {
    runId: 'fixture',
    goal: 'Цель',
    members: {},
    messages,
  };
  const frames = historyFrames(chat);
  expect(
    conversationBoundary(
      teamAt(chat, frames, Date.parse(messages[1].date)).messages,
    ),
  ).toBe(0);
  expect(
    conversationBoundary(
      teamAt(chat, frames, Date.parse(messages[3].date)).messages,
    ),
  ).toBe(1);
  expect(
    conversationBoundary(
      teamAt(chat, frames, Date.parse(messages[4].date)).messages,
    ),
  ).toBe(3);
});

// Высокий текст нельзя центрировать: его начало уйдёт под закреплённую цель.
// Проверяем геометрию перехода и повторный расчёт после раскрытия цели.
it('открывает начало сводки под текущей высотой закреплённой цели', () => {
  render(
    <AppTheme>
      <div className="team-messages" style={{ rowGap: 18 }}>
        <div className="team-pin-layer" />
        <TeamConversation
          messages={messages}
          preserveReading={false}
          renderMessage={() => null}
        />
      </div>
    </AppTheme>,
  );
  const list = document.querySelector<HTMLElement>('.team-messages')!;
  const pin = list.querySelector<HTMLElement>('.team-pin-layer')!;
  const summary = document.getElementById('team-message-summary2')!;
  let pinHeight = 80;
  vi.spyOn(list, 'getBoundingClientRect').mockImplementation(
    () => ({ top: 100 }) as DOMRect,
  );
  vi.spyOn(pin, 'getBoundingClientRect').mockImplementation(
    () => ({ height: pinHeight }) as DOMRect,
  );
  vi.spyOn(summary, 'getBoundingClientRect').mockImplementation(
    () => ({ top: 100 + 1000 - list.scrollTop, height: 2500 }) as DOMRect,
  );
  const go = screen.getByRole('button', { name: 'К сводке Босса' });
  fireEvent.click(go);
  expect(summary.getBoundingClientRect().top).toBe(198);
  expect(document.activeElement).toBe(summary);
  pinHeight = 220;
  fireEvent.click(go);
  expect(summary.getBoundingClientRect().top).toBe(338);
});
