import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { AppTheme } from '../components/Theme';
import Office from './Office';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState(null, '', '/office');
});

// Пустой офис не создаёт заказ: настройки задаются только запуском CLI.
it('показывает пустой офис без настроек и создания заказа', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({ teams: [], cwd: '/project', problems: [] }),
        ),
    ),
  );
  render(
    <AppTheme>
      <Office />
    </AppTheme>,
  );
  expect(
    screen.queryByAltText('Разработчик за MacBook'),
  ).not.toBeInTheDocument();
  expect(screen.queryByAltText('Босс за MacBook')).not.toBeInTheDocument();
  expect(
    screen.queryByRole('button', { name: 'Внешность сотрудников' }),
  ).not.toBeInTheDocument();
  await userEvent
    .setup()
    .click(screen.getByRole('button', { name: 'Открыть чат команды' }));
  expect(
    await screen.findByText(
      'Нет запущенных команд. Создайте заказ через CLI Lawa.',
    ),
  ).toBeVisible();
});

// Стол, точка и реплика выводятся из durable-состояния выбранного заказа.
it('показывает приглашённого Разработчика и настоящую активность', async () => {
  window.history.replaceState(null, '', '/office?run=order');
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            runId: 'order',
            goal: 'Игра',
            members: {},
            messages: [],
            room: {
              actors: {
                boss: {
                  status: 'monitoring',
                  nextCheck: '2026-09-17T12:00:00Z',
                },
                developer: {
                  status: 'working',
                  summary: 'Проверяю прыжок',
                  nextCheck: '2026-09-17T12:00:00Z',
                },
              },
            },
          }),
        ),
    ),
  );
  render(
    <AppTheme>
      <Office />
    </AppTheme>,
  );
  expect(
    await screen.findByRole('button', { name: 'Разработчик: Работает' }),
  ).toBeVisible();
  expect(screen.getByRole('button', { name: 'Босс: Мониторит' })).toBeVisible();
  expect(screen.getByText('Проверяю прыжок')).toBeVisible();
});

// Новый ID не требует отдельного компонента. Образ и подпись берутся из конфига,
// а неприглашённая личность каталога не занимает рабочее место.
it('показывает произвольного сотрудника с выбранной в конфиге внешностью', async () => {
  window.history.replaceState(null, '', '/office?run=design');
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            runId: 'design',
            goal: 'Дизайн',
            messages: [],
            members: {
              boss: { name: 'Босс' },
              designer: { name: 'Лена', avatar: 'pixel-designer' },
            },
            room: {
              catalog: { qa: { name: 'QA' } },
              actors: {
                boss: { status: 'idle', nextCheck: '' },
                designer: {
                  status: 'working',
                  summary: 'Подбирает цвета',
                  nextCheck: '',
                },
              },
            },
          }),
        ),
    ),
  );
  render(
    <AppTheme>
      <Office />
    </AppTheme>,
  );
  expect(
    await screen.findByRole('button', { name: 'Лена: Работает' }),
  ).toBeVisible();
  expect(screen.getByAltText('Лена за MacBook')).toHaveAttribute(
    'src',
    expect.stringContaining('pixel-designer.png'),
  );
  expect(screen.queryByAltText('QA за MacBook')).not.toBeInTheDocument();
});

// Реальный состав из 50 участников не теряет столы или доступные имена
// в компактном режиме, в том числе при длинных именах из конфига команды.
it('показывает все 50 рабочих мест с полными доступными именами', async () => {
  window.history.replaceState(null, '', '/office?run=large-team');
  const ids = [
    'boss',
    ...Array.from({ length: 49 }, (_, index) => `member-${index}`),
  ];
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            runId: 'large-team',
            goal: 'Большая команда',
            messages: [],
            members: Object.fromEntries(
              ids.map((id) => [
                id,
                { name: `${id} — специалист большой команды` },
              ]),
            ),
            room: {
              actors: Object.fromEntries(
                ids.map((id) => [id, { status: 'idle', nextCheck: '' }]),
              ),
            },
          }),
        ),
    ),
  );
  const { container } = render(
    <AppTheme>
      <Office />
    </AppTheme>,
  );
  expect(
    await screen.findByRole('button', {
      name: 'member-48 — специалист большой команды: Ждёт',
    }),
  ).toBeVisible();
  expect(container.querySelectorAll('.office-employee')).toHaveLength(50);
  expect(container.querySelector('.office-scene-dense')).not.toBeNull();
  expect(
    screen.getByRole('button', {
      name: 'member-48 — специалист большой команды: Ждёт',
    }),
  ).toHaveAttribute(
    'title',
    expect.stringContaining('member-48 — специалист большой команды'),
  );
});
