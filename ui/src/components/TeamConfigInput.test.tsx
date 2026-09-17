import { afterEach, expect, it, vi } from 'vitest';
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { TeamPhone } from './TeamPhone';
import { AppTheme } from './Theme';
import { parseTeamConfig } from './TeamConfigInput';

const characters = {
  designer: {
    name: 'Дизайнер',
    history: 'Исследователь интерфейсов',
    instructions: 'Проверяй контраст',
    avatar: 'pixel-designer',
  },
};
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState(null, '', '/office');
});

it('проверяет структуру и зарезервированные ID до отправки', () => {
  expect(parseTeamConfig(JSON.stringify({ characters }))).toEqual(characters);
  expect(parseTeamConfig('{"characters":{}}')).toEqual({});
  for (const config of [
    {},
    { characters: [] },
    { characters: { human: characters.designer } },
    { characters: { designer: { ...characters.designer, instructions: '' } } },
    { characters, extra: true },
  ])
    expect(() => parseTeamConfig(JSON.stringify(config))).toThrow();
});

// Проверяем форму целиком: выбранный JSON попадает в запрос создания, а после
// ошибки файла нельзя молча запустить обычную команду вместо выбранной.
it('загружает личности из файла в новый заказ и блокирует неверный конфиг', async () => {
  window.history.replaceState(null, '', '/office');
  const sent: unknown[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_url: string, options?: RequestInit) => {
      if (options?.method === 'POST') {
        sent.push(JSON.parse(String(options.body)));
        return new Response(JSON.stringify({ runId: 'new-order' }));
      }
      return new Response(
        JSON.stringify({ teams: [], problems: [], cwd: '/project' }),
      );
    }),
  );
  render(
    <AppTheme>
      <TeamPhone onClose={() => {}} />
    </AppTheme>,
  );
  fireEvent.change(
    await screen.findByRole('textbox', { name: 'Цель команды' }),
    { target: { value: 'Нарисовать интерфейс' } },
  );
  const input = screen.getByLabelText('JSON команды');
  fireEvent.change(input, {
    target: {
      files: [{ name: 'broken.json', size: 3, text: async () => '{' }],
    },
  });
  await screen.findByText(/SyntaxError/);
  expect(screen.getByRole('button', { name: 'Закрепить цель' })).toBeDisabled();
  fireEvent.change(input, {
    target: {
      files: [
        {
          name: 'team.json',
          size: 200,
          text: async () => JSON.stringify({ characters }),
        },
      ],
    },
  });
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Закрепить цель' }),
    ).toBeEnabled(),
  );
  fireEvent.click(screen.getByRole('button', { name: 'Закрепить цель' }));
  await waitFor(() =>
    expect(sent).toEqual([
      { goal: 'Нарисовать интерфейс', cwd: '/project', characters },
    ]),
  );
});
