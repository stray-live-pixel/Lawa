import type { Config, Review, StageID, Usage } from './types';

export const stages: {
  id: StageID;
  label: string;
  key: 'Context' | 'Review' | 'Presentation';
}[] = [
  { id: 'context', label: 'Контекст', key: 'Context' },
  { id: 'review', label: 'Проверка', key: 'Review' },
  { id: 'presentation', label: 'Результат', key: 'Presentation' },
];
export const defaultConfig: Config = {
  Context: { Model: 'gpt-6-sol', Effort: 'high' },
  Review: { Model: 'gpt-6-astra', Effort: 'high' },
  Presentation: { Model: 'gpt-6-sol', Effort: 'high' },
  Prices: {},
  RublesPerDollar: 100,
};
export const reviewTemplate =
  'Проведи code review изменений по требованиям задачи.\n\nЗадача: {{ссылка на задачу}}\nИзменения (PR / MR или аналог): {{ссылка на изменения}}\nДизайн (для frontend): {{ссылка на дизайн}}';
// Только HTTP(S): произвольные URI из результата агента не исполняются браузером.
export function safeURL(url: string) {
  try {
    const value = new URL(url);
    return ['https:', 'http:'].includes(value.protocol)
      ? value.href
      : undefined;
  } catch {
    return undefined;
  }
}
export function artifactURL(id: string, path: string) {
  return `/api/reviews/${encodeURIComponent(id)}/artifact?path=${encodeURIComponent(path)}`;
}
export function counts(review: Review) {
  const all = review.Findings || [];
  const blocking = all.filter((f) => ['P0', 'P1'].includes(f.Priority)).length;
  return { blocking, other: all.length - blocking };
}
export function reviewColor(review: Review) {
  if (['running', 'pending'].includes(review.State)) return 'info';
  if (
    ['failed', 'interrupted'].includes(review.State) ||
    review.Verdict === 'request_changes'
  )
    return 'danger';
  return review.State === 'succeeded' ? 'success' : 'neutral';
}
export function dateTime(value: string) {
  const d = new Date(value);
  return Number.isNaN(d.valueOf())
    ? '—'
    : d
        .toLocaleString('ru-RU', {
          day: '2-digit',
          month: '2-digit',
          year: 'numeric',
          hour: '2-digit',
          minute: '2-digit',
        })
        .replace(',', '');
}
export function duration(start: string, end?: string | null) {
  const s = Math.max(
    0,
    Math.floor(
      ((end ? new Date(end) : new Date()).valueOf() -
        new Date(start).valueOf()) /
        1000,
    ),
  );
  return Number.isFinite(s)
    ? `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
    : '—';
}
export function number(value: number | null | undefined) {
  return value == null ? '—' : value.toLocaleString('ru-RU');
}
export function dollars(value: number | null | undefined) {
  return value == null
    ? '—'
    : `$${value.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 6 })}`;
}
// Расход суммируется по всем попыткам; Input — вход без кэшированной части.
// Неизвестное значение не превращается в ноль и не скрывает частичную метрику.
export function usageSummary(review: Review) {
  const entries = (review.Stages || []).flatMap((stage) =>
    (stage.Attempts || []).flatMap((attempt) => {
      const model =
        review.Config[stages.find((s) => s.id === stage.ID)!.key].Model;
      return attempt.ThreadUsage?.length
        ? attempt.ThreadUsage.map((t) => ({
            ...t.Usage,
            Complete: attempt.Usage.Complete && t.Usage.Complete,
            price: review.Config.Prices?.[t.Model],
          }))
        : [{ ...attempt.Usage, price: review.Config.Prices?.[model] }];
    }),
  );
  const sum = (key: keyof Usage) =>
    entries.length && entries.every((e) => typeof e[key] === 'number')
      ? entries.reduce((n, e) => n + (e[key] as number), 0)
      : null;
  const inputTotal = sum('InputTokens'),
    cache = sum('CachedInputTokens'),
    output = sum('OutputTokens');
  const input =
    inputTotal == null || cache == null
      ? null
      : Math.max(0, inputTotal - cache);
  const cost = (kind: 'Input' | 'CachedInput' | 'Output') =>
    entries.length &&
    entries.every(
      (e) =>
        e.price &&
        (kind === 'Input'
          ? e.InputTokens != null && e.CachedInputTokens != null
          : kind === 'CachedInput'
            ? e.CachedInputTokens != null
            : e.OutputTokens != null),
    )
      ? entries.reduce(
          (n, e) =>
            n +
            ((kind === 'Input'
              ? Math.max(0, e.InputTokens! - e.CachedInputTokens!)
              : kind === 'CachedInput'
                ? e.CachedInputTokens!
                : e.OutputTokens!) *
              e.price[kind]) /
              1e6,
          0,
        )
      : null;
  const complete = entries.length > 0 && entries.every((e) => e.Complete);
  const costs = [cost('Input'), cost('CachedInput'), cost('Output')];
  const total = !complete
    ? null
    : costs.every((n) => n != null)
      ? costs.reduce<number>((n, v) => n + v!, 0)
      : sum('CostUSD');
  return {
    rows: [
      { label: 'Input', tokens: input, cost: costs[0] },
      { label: 'Cache', tokens: cache, cost: costs[1] },
      { label: 'Output', tokens: output, cost: costs[2] },
    ],
    total,
    complete,
  };
}
export async function mutate<T>(url: string, body: unknown = {}): Promise<T> {
  const response = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!response.ok)
    throw new Error(
      (await response.text()).trim() || `HTTP ${response.status}`,
    );
  const text = await response.text();
  return text ? (JSON.parse(text) as T) : (undefined as T);
}

// Склонение коротких итогов не зависит от приоритета отдельных карточек.
export function findingCount(value: number, blocking: boolean) {
  const rule = new Intl.PluralRules('ru').select(value);
  const forms = blocking
    ? {
        one: 'блокирующее замечание',
        few: 'блокирующих замечания',
        many: 'блокирующих замечаний',
        other: 'блокирующего замечания',
      }
    : {
        one: 'рекомендация',
        few: 'рекомендации',
        many: 'рекомендаций',
        other: 'рекомендации',
      };
  return `${value} ${forms[rule as keyof typeof forms] || forms.other}`;
}
