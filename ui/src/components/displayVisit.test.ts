import { expect, it } from 'vitest';
import type { Execution } from '../types';
import { displayVisit } from './displayVisit';

const visit = (State: string, index: number): Execution => ({
  State,
  Visit: index,
  Key: String(index),
  StepID: 'a',
  Result: '',
  Note: '',
  Decision: '',
  Trigger: '',
  TraceURL: '',
  MemoryURL: '',
  Prompt: '',
  Attempt: 1,
});
// Skipped остаётся в истории, но не скрывает последнее реальное выполнение.
it.each([
  ['succeeded', 'skipped', 'succeeded', 1],
  ['running', 'skipped', 'running', 1],
  ['succeeded', 'failed', 'failed', 2],
  ['cancelled', 'skipped', 'cancelled', 1],
  ['skipped', 'skipped', 'skipped', 2],
  ['succeeded', 'pending', 'pending', 2],
])('%s + %s → %s #%s', (a, b, state, number) => {
  const visits = [visit(a, 1), visit(b, 2)];
  expect(displayVisit(visits)).toMatchObject({ State: state, Visit: number });
  expect(visits).toHaveLength(2);
});
it('отличает отсутствие посещений от созданного pending', () => {
  expect(displayVisit([])).toBeUndefined();
  expect(displayVisit([visit('pending', 1)])?.State).toBe('pending');
});
it('переходит к новой партии автоматически и сохраняет ручной выбор истории', () => {
  const visits = [visit('succeeded', 1), visit('skipped', 2)];
  expect(displayVisit(visits)?.Visit).toBe(1);
  visits.push(visit('running', 3));
  expect(displayVisit(visits)?.Visit).toBe(3);
  expect(displayVisit(visits, '2')?.State).toBe('skipped');
  expect(displayVisit(visits, 'missing')?.Visit).toBe(3);
});
