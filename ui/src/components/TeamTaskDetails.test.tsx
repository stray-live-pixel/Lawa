import { AppTheme } from './Theme';
import type { ReactNode } from 'react';
import {
  render as baseRender,
  cleanup,
  screen,
  fireEvent,
  waitFor,
} from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TeamTaskDetails, type TeamTask } from './TeamTaskDetails';

const task: TeamTask = {
  id: 'T1',
  text: 'Исходные условия',
  assignee: 'developer',
  card: {
    title: 'Проверить файл',
    expected: 'Файл готов',
    criteria: ['Прочитать файл'],
    revision: 1,
    version: 1,
    status: 'todo',
  },
};
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
const render = (node: ReactNode) => baseRender(<AppTheme>{node}</AppTheme>);

describe('Карточка задачи', () => {
  it('в истории не загружает будущую версию через API', () => {
    const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);
    render(
      <TeamTaskDetails
        id="T1"
        run="run"
        historical
        snapshot={task}
        messages={[]}
        onClose={() => {}}
      />,
    );
    expect(screen.getByText('Исходные условия')).toBeTruthy();
    expect(screen.getByText(/Версия условий 1/)).toBeTruthy();
    expect(fetch).not.toHaveBeenCalled();
  });
  it('ошибка загрузки позволяет повторить чтение, не создавая задачу', async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce({
        ok: false,
        text: async () => 'Временная ошибка',
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ task }) });
    vi.stubGlobal('fetch', fetch);
    render(
      <TeamTaskDetails
        id="T1"
        run="run"
        historical={false}
        messages={[]}
        onClose={() => {}}
      />,
    );
    fireEvent.click(
      await screen.findByRole('button', { name: 'Повторить загрузку' }),
    );
    await waitFor(() =>
      expect(screen.getByText('Исходные условия')).toBeTruthy(),
    );
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(fetch.mock.calls[0][0]).toContain('/tasks?taskId=T1');
  });
});
