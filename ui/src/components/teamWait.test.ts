import { expect, it } from 'vitest';
import { waitLabel, waitDetails } from './teamWait';

// Длительность зависит от курсора, происхождение не теряется в подробностях.
it('показывает источник, ссылку и длительность ожидания', () => {
  const wait = {
    kind: 'result',
    source: 'actor' as const,
    since: '2026-09-19T12:00:00Z',
    actorId: 'reviewer',
    messageId: 'question',
    text: 'Нужен отчёт',
  };
  expect(waitLabel(wait, Date.parse('2026-09-19T12:05:00Z'))).toBe(
    'Ждёт результат · 5 мин',
  );
  expect(waitDetails(wait)).toContain(
    'Сообщил сотрудник. Участник: @reviewer. Сообщение: question. Нужен отчёт',
  );
  expect(waitLabel({ ...wait, kind: 'capacity' }, Date.parse(wait.since))).toBe(
    'Ждёт свободный слот · меньше минуты',
  );
});
