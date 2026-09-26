import { afterEach, expect, it, vi } from 'vitest';
import {
  cleanup,
  render,
  screen,
  fireEvent,
  waitFor,
} from '@testing-library/react';
import { AppTheme } from '../../components/Theme';
import { ReviewResults } from './Results';
import { ReviewContext, DesignImage } from './Context';
import { artifactURL, counts, safeURL, usageSummary } from './helpers';
import { fixture } from './testFixture';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
const savedCode =
  'const [error, setError] = useState(null);\n\nasync function submit() {\n  setError(null);\n  await signIn(email, password);\n}\n';
function mockArtifacts() {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async (url: string, init?: RequestInit) =>
        new Response(
          init?.method === 'POST'
            ? '{}'
            : url.includes('code.txt')
              ? savedCode
              : url.includes('fix.md')
                ? 'Исправь повторную отправку.'
                : 'Одно нажатие создаёт один запрос.',
        ),
    ),
  );
}
// Кэш уже входит во входные токены; повторное прибавление завышает стоимость.
it('считает расходы раздельно и не подменяет неизвестное нулём', () => {
  const r = fixture();
  const usage = usageSummary(r);
  expect(usage.rows.map((v) => v.tokens)).toEqual([800, 200, 100]);
  expect(usage.total).toBeCloseTo(0.00264);
  r.Stages![0].Attempts![0].Usage.CachedInputTokens = null;
  expect(usageSummary(r).rows[0].tokens).toBeNull();
  expect(usageSummary(r).rows[1].cost).toBeNull();
});
it('различает приоритеты и блокирует исполняемые ссылки', () => {
  expect(counts(fixture())).toEqual({ blocking: 1, other: 0 });
  expect(safeURL('javascript:alert(1)')).toBeUndefined();
  expect(artifactURL('review/a', 'artifacts/code x.md')).toContain(
    'review%2Fa/artifact?path=artifacts%2Fcode%20x.md',
  );
});
// Проверяем реальную навигацию к доказательству, а не только наличие карточки.
it('открывает экскурсию и сохраняет зелёный diff внутри места ошибки', async () => {
  mockArtifacts();
  render(
    <AppTheme>
      <ReviewResults review={fixture()} />
    </AppTheme>,
  );
  fireEvent.click(screen.getByRole('button', { name: 'Разобрать замечание' }));
  expect(
    await screen.findByRole('heading', { name: 'Повторная отправка' }),
  ).toBeInTheDocument();
  const code = await screen.findByText('async function submit() {');
  expect(code.closest('.cr-code-line')).toHaveClass('added');
  expect(screen.getByText('Место ошибки')).toBeInTheDocument();
  expect(
    screen
      .getByText('Пользователь может нажать Войти повторно.')
      .closest('.cr-annotation'),
  ).toHaveClass('error');
  fireEvent.click(screen.getByRole('button', { name: 'Закрыть обзор' }));
  await waitFor(() =>
    expect(
      screen.queryByRole('heading', { name: 'Повторная отправка' }),
    ).not.toBeInTheDocument(),
  );
});
it('даёт пройти обзор и прочитать полный снимок файла', async () => {
  mockArtifacts();
  render(
    <AppTheme>
      <ReviewResults review={fixture()} />
    </AppTheme>,
  );
  fireEvent.click(screen.getByRole('button', { name: 'Обзор изменений' }));
  fireEvent.click(
    await screen.findByRole('button', { name: 'Открыть файл целиком' }),
  );
  expect(
    await screen.findByRole('heading', { name: 'src/LoginForm.tsx' }),
  ).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Закрыть' }));
  fireEvent.click(screen.getByRole('button', { name: 'Следующий шаг' }));
  expect(
    await screen.findByText('Показываем результат входа.'),
  ).toBeInTheDocument();
});
it('открывает сохранённые требования и комментарии', async () => {
  mockArtifacts();
  render(
    <AppTheme>
      <ReviewContext review={fixture()} />
    </AppTheme>,
  );
  expect(
    await screen.findByText('Одно нажатие создаёт один запрос.'),
  ).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Комментарии · 0' }));
  expect(await screen.findByText('Комментариев нет')).toBeInTheDocument();
});

// Неполная телеметрия субагентов запрещает общий итог, даже при известных ценах.
it('не выдаёт стоимость основного агента за полную при пропущенном usage субагента', () => {
  const r = fixture();
  r.Stages![0].Attempts![0].Usage.Complete = false;
  r.Stages![0].Attempts![0].Usage.CostUSD = null;
  expect(usageSummary(r).total).toBeNull();
  expect(usageSummary(r).complete).toBe(false);
});
// У разных субагентов разные тарифы: агрегат попытки не суммируется второй раз.
it('оценивает ThreadUsage по модели каждого агента без двойного счёта', () => {
  const r = fixture(),
    a = r.Stages![0].Attempts![0];
  r.Config.Prices['gpt-6-astra'] = {
    Input: 10,
    CachedInput: 1,
    Output: 50,
    Source: 'https://example.test/pricing',
    AsOf: '2026-09-26',
  };
  a.ThreadUsage = [
    { ThreadID: 'parent', Model: 'gpt-6-sol', Effort: 'high', Usage: a.Usage },
    {
      ThreadID: 'child',
      Model: 'gpt-6-astra',
      Effort: 'high',
      Usage: {
        InputTokens: 100,
        CachedInputTokens: 0,
        OutputTokens: 10,
        CostUSD: 0.0015,
        Complete: true,
      },
    },
  ];
  expect(usageSummary(r).rows.map((row) => row.tokens)).toEqual([
    900, 200, 110,
  ]);
  expect(usageSummary(r).total).toBeCloseTo(0.00414);
});
it('объясняет замечание без искусственной привязки и экскурсии', async () => {
  mockArtifacts();
  const r = fixture();
  r.Presentation.FindingTours = [];
  r.Findings![0].Anchors = [
    {
      FileID: '',
      StartLine: 0,
      EndLine: 0,
      TaskID: 'TASK-42',
      DesignID: '',
      NoAnchorReason:
        'Требуемый обработчик отсутствует, привязки к строкам нет.',
    },
  ];
  render(
    <AppTheme>
      <ReviewResults review={r} />
    </AppTheme>,
  );
  fireEvent.click(screen.getByRole('button', { name: 'Разобрать замечание' }));
  expect(
    await screen.findByText(
      'Требуемый обработчик отсутствует, привязки к строкам нет.',
    ),
  ).toBeInTheDocument();
  expect(screen.getByText('Нажмите Войти дважды.')).toBeInTheDocument();
});
it('не скрывает сохранённый результат из-за ошибки публикации', () => {
  const r = fixture();
  r.State = 'failed';
  r.Stages![2].State = 'failed';
  r.Error = 'Публикация недоступна';
  render(
    <AppTheme>
      <ReviewResults review={r} />
    </AppTheme>,
  );
  expect(
    screen.getByRole('button', { name: 'Обзор изменений' }),
  ).toBeInTheDocument();
  expect(
    screen.getByRole('button', { name: 'Разобрать замечание' }),
  ).toBeEnabled();
});

it('открывает сохранённый PNG кликом на изображение', async () => {
  render(
    <AppTheme>
      <DesignImage
        id="review-fixture"
        design={{
          ID: 'design-1',
          Title: 'Форма входа',
          Path: 'artifacts/design.png',
          SourceURL: 'https://example.test/design',
          Version: '1',
          CollectedAt: '2026-09-26T00:00:00Z',
        }}
      />
    </AppTheme>,
  );
  const preview = screen.getByRole('img', { name: 'Форма входа' });
  expect(preview).toHaveAttribute(
    'src',
    artifactURL('review-fixture', 'artifacts/design.png'),
  );
  fireEvent.click(
    screen.getByRole('button', { name: 'Увеличить: Форма входа' }),
  );
  expect(
    await screen.findByRole('dialog', { name: 'Форма входа' }),
  ).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Закрыть' }));
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument(),
  );
});
