import { WorkflowSource } from './WorkflowSource';
import {
  act,
  fireEvent,
  render as baseRender,
  screen,
  waitFor,
  cleanup,
} from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ThemeProvider } from '@gravity-ui/uikit';
import type { ReactNode } from 'react';
import { MemoryRouter } from 'react-router-dom';
import type { Dashboard, Graph, Run } from '../types';
import { usePoll } from '../hooks/api';
import { MarkdownDocument } from './MarkdownDocument';
import { ContinuationPanel } from './Continuation';
import { Continuation } from './Continuation';
import { mergeTrace } from './Trace';
import { layout, WorkflowGraph } from './WorkflowGraph';
import App from '../App';
import { ImageExport } from './ImageExport';
import { RunTree } from './Tree';
import { DashboardFilters } from './DashboardFilters';
import { Dialog } from './ui';

// Те же провайдеры, что и в приложении: тестируем реальные popup/диалоги UIKit.
const render = (node: ReactNode) =>
  baseRender(node, {
    wrapper: ({ children }) => (
      <ThemeProvider theme="dark" lang="ru">
        {children}
      </ThemeProvider>
    ),
  });
async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole('combobox', { name: label }));
  fireEvent.click(await screen.findByRole('option', { name: option }));
}

// Проверяем нашу обработку выбора и данных, а DOM/геометрию React Flow — живым
// браузером. Заглушка сохраняет контракт клика по узлу, не вычисляя layout за нас.
vi.mock('@xyflow/react', () => ({
  ReactFlow: ({ nodes, onNodeClick, children }: any) => (
    <div>
      {nodes.map((node: any) => (
        <button
          key={node.id}
          data-state={node.data.state}
          data-visit={node.data.visit}
          onClick={() => onNodeClick(null, node)}
        >
          {node.id}
        </button>
      ))}
      {children}
    </div>
  ),
  Background: () => null,
  Panel: () => null,
  useReactFlow: () => ({ zoomIn() {}, zoomOut() {}, fitView() {} }),
  Handle: () => null,
  Position: { Left: 'left', Right: 'right' },
  MarkerType: { ArrowClosed: 'arrowclosed' },
}));
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
const response = (data: unknown) =>
  Promise.resolve({ ok: true, json: async () => data } as Response);
const run = {
  ID: 'run-a',
  Name: 'Workflow A',
  State: 'succeeded',
  Steps: [],
  Children: [],
  ActiveSteps: [],
  CompletedSteps: 1,
  TotalSteps: 1,
  DeleteURL: '/api/runs/run-a/stop-and-delete',
  TicketID: '',
  TicketTitle: '',
  EventsURL: '/events/run-a',
  VSCodeURL: 'vscode://file/tmp/run-a',
} as unknown as Run;
const page = {
  Title: 'Lawa',
  Refresh: '3',
  Preview: false,
  Roots: [run],
  Problems: [],
  Scheduled: [
    {
      SeriesID: 'series',
      WorkflowID: 'Следующий run',
      Remaining: '35 мин',
      Next: '15.09.2026 12:00:00',
      Schedule: 'каждый час',
      Progress: '1 из 10',
    },
  ],
  Filter: {
    Query: '',
    Period: 'all',
    Scope: 'all',
    States: 'all',
    RootID: '',
    Total: 1,
    Periods: [{ Value: 'all', Label: 'За всё время' }],
    ActiveURL: '/?period=all',
    AllURL: '/?period=all&view=all',
    AllStatesURL: '/?period=all&view=all',
    WorkingURL: '/?states=working',
    FailedURL: '/?states=failed',
  },
  Pagination: { Visible: false },
} as unknown as Dashboard;
const graph: Graph = {
  ID: 'run-a',
  Name: 'Workflow A',
  State: 'succeeded',
  StopReason: '',
  Prompt: 'workflow context',
  Nodes: [
    { ID: 'loop', Prompt: 'unstarted', Routes: [] },
    { ID: 'next', Prompt: 'next context', Routes: [] },
  ],
  Edges: [
    { From: 'loop', To: 'loop', Label: 'again' },
    { From: 'loop', To: 'next', Label: 'done' },
  ],
  Executions: [1, 2].map((n) => ({
    Key: `visit-${n}`,
    StepID: 'loop',
    State: 'succeeded',
    Result: `Итог: проход ${n}`,
    Note: '',
    Decision: '',
    Trigger: '',
    TraceURL: '',
    MemoryURL: '',
    Prompt: `context visit-${n}`,
    Visit: n,
    Attempt: 1,
  })),
};

describe('Контекст и история', () => {
  it('копирует выбранную область и оставляет ручное копирование при отказе clipboard', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
    render(<Continuation cube="cube context" workflow="workflow context" />);
    await choose('Контекст продолжения', 'Весь workflow');
    fireEvent.click(screen.getByRole('button', { name: 'Скопировать промпт' }));
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith('workflow context'),
    );
    writeText.mockRejectedValueOnce(new Error('denied'));
    fireEvent.click(screen.getByRole('button', { name: 'Скопировать промпт' }));
    await screen.findByText(/Скопируйте выделенную разметку/);
    expect(
      screen.getByLabelText('Исходный Markdown: Промпт продолжения'),
    ).toHaveFocus();
  });
  it('различает turn, завершённый item заменяет delta, большие блоки ограничены', () => {
    const blocks = new Map();
    mergeTrace(blocks, [
      {
        time: 'now',
        kind: 'agent_message_delta',
        turnId: 'one',
        itemId: 'x',
        content: 'черновик',
      },
      {
        time: 'now',
        kind: 'item_completed',
        turnId: 'one',
        itemId: 'x',
        content: 'готово',
      },
      {
        time: 'now',
        kind: 'item_completed',
        turnId: 'two',
        itemId: 'x',
        content: 'другой turn',
      },
    ]);
    expect([...blocks.values()].map((item) => item.content)).toEqual([
      'готово',
      'другой turn',
    ]);
    mergeTrace(
      blocks,
      Array.from({ length: 210 }, (_, index) => ({
        time: 'now',
        kind: 'item_completed',
        itemId: String(index),
        content: 'a'.repeat(65000),
      })),
    );
    expect(blocks.size).toBe(200);
    expect(
      [...blocks.values()].every((item) => item.content.length <= 64000),
    ).toBe(true);
  });
  it('не смешивает результаты повторных посещений и показывает незапущенный узел', async () => {
    render(<WorkflowGraph runID="run-a" preview={graph} />);
    expect(screen.getByText('Итог: проход 2')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Посещение 1' }));
    expect(screen.getByText('Итог: проход 1')).toBeInTheDocument();
    expect(
      screen.queryByLabelText('Промпт продолжения'),
    ).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'next' }));
    expect(screen.getByText('Кубик ещё не запускался.')).toBeInTheDocument();
    expect(screen.queryByText('Итог: проход 1')).not.toBeInTheDocument();
  });
  it('раскладывает развилку и цикл без потери узлов', () => {
    const points = layout(graph.Nodes!, graph.Edges!);
    expect(points.size).toBe(2);
    for (const point of points.values()) {
      expect(Number.isFinite(point.x)).toBe(true);
      expect(Number.isFinite(point.y)).toBe(true);
    }
    expect(points.get('loop')).not.toEqual(points.get('next'));
  });
  it('поздний ответ прошлого URL не подменяет новый ресурс', async () => {
    let resolveOld!: (response: Response) => void;
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        url === '/old'
          ? new Promise<Response>((resolve) => {
              resolveOld = resolve;
            })
          : response({ name: 'new' }),
      ),
    );
    function Probe({ url }: { url: string }) {
      const { data } = usePoll<{ name: string }>(url, 0);
      return <p>{data?.name || 'loading'}</p>;
    }
    const view = render(<Probe url="/old" />);
    view.rerender(<Probe url="/new" />);
    await screen.findByText('new');
    await act(async () => resolveOld(await response({ name: 'old' })));
    expect(screen.queryByText('old')).not.toBeInTheDocument();
  });
});

describe('Главная страница', () => {
  it('сохраняет действия, расписание и требует подтверждения удаления', async () => {
    const fetchMock = vi.fn((url: string, options?: RequestInit) =>
      options?.method === 'POST'
        ? response({ deleted: true })
        : response(url.startsWith('/api/graph') ? graph : page),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(
      <MemoryRouter initialEntries={['/?period=all&view=all']}>
        <App />
      </MemoryRouter>,
    );
    await screen.findByRole('tab', { name: 'Информация' });
    fireEvent.click(screen.getByRole('tab', { name: 'Информация' }));
    fireEvent.click(
      await screen.findByRole('button', { name: 'Остановить и удалить' }),
    );
    expect(
      fetchMock.mock.calls.filter(([, options]) => options?.method === 'POST'),
    ).toHaveLength(0);
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Отмена' }));
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument(),
    );
    // Управление остаётся внутри боковой панели после удаления общего header.
    const sidebar = screen.getByRole('complementary', {
      name: 'Дерево workflow и кубиков',
    });
    for (const name of ['Тема интерфейса', 'Запланированные запуски']) {
      expect(sidebar).toContainElement(screen.getByRole('button', { name }));
    }
    fireEvent.click(
      screen.getByRole('button', { name: 'Запланированные запуски' }),
    );
    expect(
      await screen.findByRole('dialog', { name: 'Расписание запусков' }),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Закрыть' }));
    fireEvent.click(
      screen.getByRole('button', { name: 'Остановить и удалить' }),
    );
    fireEvent.click(
      screen.getAllByRole('button', { name: 'Остановить и удалить' }).at(-1)!,
    );
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.some(
          ([url, options]) =>
            url === run.DeleteURL && options?.method === 'POST',
        ),
      ).toBe(true),
    );
  });
  it('ошибка API видна, разметка из данных остаётся текстом', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        response({
          ...page,
          Roots: [{ ...run, Name: '<script>alert(1)</script>' }],
        }),
      ),
    );
    const { container } = render(
      <MemoryRouter>
        <App />
      </MemoryRouter>,
    );
    await screen.findByTitle('<script>alert(1)</script>');
    expect(container.querySelector('script')).toBeNull();
  });
});

// Принудительное раскрытие результатов поиска не должно блокировать ручное
// сворачивание. Новый набор фильтров монтирует дерево заново в App.
it('позволяет свернуть автоматически раскрытое дерево', () => {
  render(<RunTree run={run} expanded onSelect={() => {}} onFocus={() => {}} />);
  const toggle = screen.getByRole('button', { name: 'Развернуть Workflow A' });
  expect(toggle).toHaveAttribute('aria-expanded', 'true');
  fireEvent.click(toggle);
  expect(toggle).toHaveAttribute('aria-expanded', 'false');
});

// Управляемый Dialog не имеет единственного Trigger: после Escape пользователь
// должен вернуться к реальной кнопке, которой открыл текущий экземпляр.
it('возвращает фокус инициатору и связывает описание диалога', async () => {
  const view = render(
    <>
      <button>Открыть</button>
      <Dialog
        open={false}
        onOpenChange={() => {}}
        title="Диалог"
        description="Описание"
      >
        Текст
      </Dialog>
    </>,
  );
  const opener = screen.getByRole('button', { name: 'Открыть' });
  opener.focus();
  view.rerender(
    <>
      <button>Открыть</button>
      <Dialog
        open
        onOpenChange={() => {}}
        title="Диалог"
        description="Описание"
      >
        Текст
      </Dialog>
    </>,
  );
  expect(screen.getByRole('dialog')).toHaveAccessibleDescription('Описание');
  view.rerender(
    <>
      <button>Открыть</button>
      <Dialog
        open={false}
        onOpenChange={() => {}}
        title="Диалог"
        description="Описание"
      >
        Текст
      </Dialog>
    </>,
  );
  await waitFor(() => expect(opener).toHaveFocus());
});

// HTML и адреса из Markdown — недоверенный текст. Проверяем безопасный render
// одновременно с точной копией исходника, включая таблицы и fenced code.
it('рендерит Markdown и копирует точный исходник без HTML и загрузки картинок', async () => {
  const text =
    '# Заголовок\n\n**Важно**\n\n| A | B |\n| - | - |\n| 1 | 2 |\n\n```go\nfmt.Println("ok")\n```\n<script>alert(1)</script>\n![secret](https://example.com/secret)\n[опасно](javascript:alert(1))';
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, 'clipboard', {
    value: { writeText },
    configurable: true,
  });
  const { container } = render(
    <MarkdownDocument text={text} label="Документ" />,
  );
  expect(
    screen.getByRole('heading', { name: 'Заголовок' }),
  ).toBeInTheDocument();
  expect(screen.getByRole('table')).toBeInTheDocument();
  expect(container.querySelector('script, img')).toBeNull();
  expect(screen.getByText('опасно')).not.toHaveAttribute(
    'href',
    'javascript:alert(1)',
  );
  fireEvent.click(screen.getByRole('button', { name: 'Скопировать Markdown' }));
  await waitFor(() => expect(writeText).toHaveBeenCalledWith(text));
});

it('вкладка продолжения получает точное выбранное посещение', () => {
  render(
    <ContinuationPanel
      runID="run-a"
      stepID="loop"
      visitID="visit-1"
      preview={graph}
      onSelectionChange={() => {}}
    />,
  );
  expect(screen.getByLabelText('Промпт продолжения')).toHaveTextContent(
    'context visit-1',
  );
  expect(screen.getByLabelText('Посещение для продолжения')).toHaveTextContent(
    'Посещение 1',
  );
});

it('сообщения загружаются только после открытия модалки', async () => {
  const fetcher = vi.fn(() => response({ events: [], nextOffset: 0 }));
  vi.stubGlobal('fetch', fetcher);
  const sample = {
    ...graph,
    Executions: graph.Executions!.map((e) => ({
      ...e,
      TraceURL: '/api/trace/run-a?visit=' + e.Key,
    })),
  };
  render(<WorkflowGraph runID="run-a" preview={sample} />);
  expect(fetcher).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: 'Сообщения и действия' }));
  expect(screen.getByRole('dialog')).toBeInTheDocument();
  await waitFor(() => expect(fetcher).toHaveBeenCalled());
});

// Экспорт выполняется по клику и сохраняет выбранную тему при polling.
it('PNG начинает с темы UI и сохраняет ручной выбор при обновлении', async () => {
  const fetcher = vi.fn();
  vi.stubGlobal('fetch', fetcher);
  const view = render(<ImageExport runID="run-a" />);
  expect(screen.getByRole('link', { name: 'Показать PNG' })).toHaveAttribute(
    'href',
    '/graph-image/run-a?theme=dark',
  );
  await choose('Тема картинки', 'Светлая');
  view.rerender(<ImageExport runID="run-a" />);
  expect(screen.getByRole('link', { name: 'Показать PNG' })).toHaveAttribute(
    'href',
    '/graph-image/run-a?theme=light',
  );
  expect(screen.getByRole('link', { name: 'Скачать PNG' })).toHaveAttribute(
    'href',
    '/graph-image/run-a?theme=light&download=1',
  );
  expect(fetcher).not.toHaveBeenCalled();
});

// Статус меняет scope и states вместе: завершённые ошибки не должны исчезать
// из-за оставшегося active-scope. В покое сброс не занимает место.
it('компактный статус задаёт полную выборку, а период меняется независимо', async () => {
  const onChange = vi.fn(),
    onReset = vi.fn();
  const sample = {
    ...page,
    Filter: {
      ...page.Filter,
      Scope: 'active',
      States: 'all',
      Period: '24h',
      Matched: 2,
      Periods: [
        { Value: '24h', Label: 'За последние 24 часа', Selected: true },
        { Value: 'all', Label: 'За всё время', Selected: false },
      ],
    },
    Pagination: { ...page.Pagination, Current: 1 },
  };
  const view = render(
    <DashboardFilters data={sample} onChange={onChange} onReset={onReset} />,
  );
  expect(
    screen.queryByRole('button', { name: 'Сбросить фильтры' }),
  ).not.toBeInTheDocument();
  expect(screen.queryByText(/Найдено:/)).not.toBeInTheDocument();
  await choose('Статус workflow', 'С ошибками');
  expect(onChange).toHaveBeenLastCalledWith({ view: 'all', states: 'failed' });
  await choose('Статус workflow', 'Завершённые');
  expect(onChange).toHaveBeenLastCalledWith({ view: 'completed', states: '' });
  await choose('Период', 'За всё время');
  expect(onChange).toHaveBeenLastCalledWith({ period: 'all' });
  view.rerender(
    <DashboardFilters
      data={{ ...sample, Filter: { ...sample.Filter, Query: 'needle' } }}
      onChange={onChange}
      onReset={onReset}
    />,
  );
  fireEvent.click(screen.getByRole('button', { name: 'Сбросить фильтры' }));
  expect(onReset).toHaveBeenCalledOnce();
});

// Анонимизированная топология: параллельные маршруты внутри цикла раньше
// обрушали страницу исключением Dagre. Исходные связи должны сохраняться.
it('раскладывает цикл с параллельными маршрутами', () => {
  const pairs = [
    ['node-13', 'node-0'],
    ['node-0', 'node-14'],
    ['node-0', 'node-1'],
    ['node-0', 'node-1'],
    ['node-0', 'node-2'],
    ['node-0', 'node-1'],
    ['node-0', 'node-2'],
    ['node-0', 'node-3'],
    ['node-0', 'node-1'],
    ['node-0', 'node-2'],
    ['node-0', 'node-3'],
    ['node-0', 'node-4'],
    ['node-0', 'node-1'],
    ['node-0', 'node-2'],
    ['node-0', 'node-3'],
    ['node-0', 'node-4'],
    ['node-0', 'node-5'],
    ['node-1', 'node-6'],
    ['node-2', 'node-6'],
    ['node-3', 'node-6'],
    ['node-4', 'node-6'],
    ['node-5', 'node-6'],
    ['node-6', 'node-7'],
    ['node-7', 'node-8'],
    ['node-8', 'node-9'],
    ['node-8', 'node-13'],
    ['node-9', 'node-10'],
    ['node-10', 'node-11'],
    ['node-11', 'node-12'],
    ['node-12', 'node-13'],
    ['node-14', 'node-15'],
  ];
  const nodes = Array.from({ length: 16 }, (_, i) => ({
    ID: `node-${i}`,
    Prompt: '',
    Routes: [],
  }));
  const edges = pairs.map(([From, To]) => ({ From, To, Label: '' }));
  const original = structuredClone(edges);
  const positions = layout(nodes, edges);
  expect(positions.size).toBe(nodes.length);
  for (const point of positions.values()) {
    expect(Number.isFinite(point.x)).toBe(true);
    expect(Number.isFinite(point.y)).toBe(true);
  }
  expect(edges).toEqual(original);
});

// Вкладка показывает сохранённый JSON, затем тот же текст инструкции в двух
// представлениях. Открытие не обращается к путям исходных файлов пользователя.
it('показывает JSON и сохранённый Markdown запуска', async () => {
  const fetcher = vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({
      JSON: { id: 'saved' },
      Note: 'Сохранённый снимок',
      Documents: [{ Name: 'Инструкция · a', Content: '# Saved heading' }],
    }),
  });
  vi.stubGlobal('fetch', fetcher);
  render(<WorkflowSource runID="saved-run" />);
  expect(await screen.findByLabelText('JSON workflow')).toHaveValue(
    JSON.stringify({ id: 'saved' }, null, 2),
  );
  expect(fetcher.mock.calls[0][0]).toBe('/api/source/saved-run');
  await choose('Файл workflow', 'Инструкция · a');
  expect(
    screen.getByRole('heading', { name: 'Saved heading' }),
  ).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Исходный текст' }));
  expect(screen.getByLabelText('Исходный Markdown')).toHaveValue(
    '# Saved heading',
  );
});

// Один и тот же выбранный visit определяет цвет узла и результат панели.
// Polling новой партии обновляет auto, но не сбрасывает явный просмотр истории.
it('согласует состояние кубика и детали при skipped и новой партии', async () => {
  const base = graph.Executions![0];
  const make = (Key: string, State: string, Visit: number) => ({
    ...base,
    Key,
    StepID: 'loop',
    State,
    Visit,
    Result: `Результат ${Key}`,
    Trigger: '',
  });
  const value: Graph = {
    ...graph,
    Executions: [make('first', 'succeeded', 1), make('skip', 'skipped', 2)],
  };
  const { rerender } = render(<WorkflowGraph runID="run-a" preview={value} />);
  expect(screen.getByRole('button', { name: 'loop' })).toHaveAttribute(
    'data-state',
    'succeeded',
  );
  expect(screen.getByText('Результат first')).toBeInTheDocument();
  const next = {
    ...value,
    Executions: [...value.Executions!, make('next', 'running', 3)],
  };
  rerender(
    <ThemeProvider theme="dark" lang="ru">
      <WorkflowGraph runID="run-a" preview={next} />
    </ThemeProvider>,
  );
  expect(screen.getByRole('button', { name: 'loop' })).toHaveAttribute(
    'data-state',
    'running',
  );
  expect(screen.getByRole('button', { name: 'loop' })).toHaveAttribute(
    'data-visit',
    '3',
  );
  expect(screen.getByText('Результат next')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Посещение 2' }));
  expect(screen.getByRole('button', { name: 'Посещение 2' })).toHaveAttribute(
    'aria-current',
    'page',
  );
  expect(screen.getByRole('button', { name: 'loop' })).toHaveAttribute(
    'data-state',
    'skipped',
  );
  expect(screen.getByText('Результат skip')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Посещение 3' }));
  expect(screen.getByRole('button', { name: 'loop' })).toHaveAttribute(
    'data-state',
    'running',
  );
});
