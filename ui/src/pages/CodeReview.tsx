import { useEffect, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { Icon, TextArea, TextInput, Label, Tooltip } from '@gravity-ui/uikit';
import {
  ArrowRight,
  Bell,
  FileText,
  Plus,
  Square,
  ArrowRotateRight,
} from '@gravity-ui/icons';
import { Button, Choice, Dialog, ErrorNotice } from '../components/ui';
import { ThemePicker } from '../components/Theme';
import { ResizableRunList } from '../components/ResizableRunList';
import { usePoll } from '../hooks/api';
import { MarkdownDocument } from '../components/MarkdownDocument';
import {
  stages,
  defaultConfig,
  reviewTemplate,
  counts,
  reviewColor,
  dateTime,
  duration,
  usageSummary,
  number,
  dollars,
  mutate,
} from './review/helpers';
import { LinkLabel } from './review/Documents';
import { ReviewContext } from './review/Context';
import { ReviewExecution } from './review/Execution';
import { ReviewResults } from './review/Results';
import type { Config, Review, StageID, ViewState } from './review/types';
import './review/review.css';

// URL адресует сущность, выбранный этап живёт независимо от текущего исполнения.
// Новый этап автоматически открывается один раз; ручной возврат не сбрасывается polling.
export default function CodeReview() {
  const [params, setParams] = useSearchParams(),
    id = params.get('reviewId') || '';
  const [revision, refresh] = useState(0),
    [stage, setStage] = useState<StageID>('context');
  const history = usePoll<{
    Reviews: Review[];
    Views: Record<string, ViewState>;
  }>(`/api/reviews?revision=${revision}`, 2000);
  const detail = usePoll<Review>(
    id ? `/api/reviews/${encodeURIComponent(id)}?revision=${revision}` : null,
    1500,
  );
  const defaults = usePoll<{ Config: Config; CWD: string }>(
    '/api/reviews/defaults',
    0,
  );
  const review = detail.data;
  const [error, setError] = useState(''),
    [busy, setBusy] = useState(false);
  const current = useRef('');
  const selectReview = (next: string) => {
    setParams(next ? { reviewId: next } : {});
    setError('');
  };
  useEffect(() => {
    if (!review) return;
    const next = `${review.ID}/${review.CurrentStage}`;
    if (next !== current.current) {
      current.current = next;
      setStage(review.CurrentStage || 'context');
    }
  }, [review?.ID, review?.CurrentStage]);
  // Прочитано только показанное ревью активного окна; фоновый polling метку не снимает.
  useEffect(() => {
    if (!review) return;
    const mark = () => {
      if (
        document.visibilityState === 'visible' &&
        document.hasFocus() &&
        stage === review.CurrentStage
      )
        void mutate(`/api/reviews/${encodeURIComponent(review.ID)}/view`, {
          SeenRevision: review.Revision,
          SelectedStage: stage,
        }).catch(() => {});
    };
    mark();
    window.addEventListener('focus', mark);
    document.addEventListener('visibilitychange', mark);
    return () => {
      window.removeEventListener('focus', mark);
      document.removeEventListener('visibilitychange', mark);
    };
  }, [review?.ID, review?.Revision, review?.CurrentStage, stage]);
  const action = async (name: string) => {
    setBusy(true);
    setError('');
    try {
      await mutate(`/api/reviews/${encodeURIComponent(id)}/${name}`);
      refresh((n) => n + 1);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="code-review">
      <ResizableRunList
        preference={{
          key: 'lawa-review-sidebar-width',
          initial: 280,
          min: 240,
          max: 480,
          unit: 'px',
          label: 'Ширина истории ревью',
        }}
      >
        <aside className="cr-sidebar">
          <header className="app-header">
            <Link to="/" className="brand" aria-label="Lawa — все запуски">
              <img src="/assets/lawa-logo.png" alt="" />
              <span className="brand-name">Lawa</span>
            </Link>
            <div className="header-tools">
              <ThemePicker />
            </div>
          </header>
          <div className="cr-history-heading">
            <h2>История ревью</h2>
            {id && (
              <Tooltip content="Новое ревью">
                <Button
                  view="flat"
                  aria-label="Новое ревью"
                  onClick={() => selectReview('')}
                >
                  <Icon data={Plus} />
                </Button>
              </Tooltip>
            )}
          </div>
          <ErrorNotice error={history.error} />
          <nav className="cr-history" aria-label="История ревью">
            {(history.data?.Reviews || [])
              .slice()
              .sort((a, b) => b.UpdatedAt.localeCompare(a.UpdatedAt))
              .map((item) => (
                <HistoryItem
                  key={item.ID}
                  review={item}
                  selected={item.ID === id}
                  unread={
                    (history.data?.Views?.[item.ID]?.SeenRevision || 0) <
                    item.Revision
                  }
                  onClick={() => selectReview(item.ID)}
                />
              ))}
            {history.data && !history.data.Reviews?.length && (
              <p className="cr-empty-history">Вы ещё не запускали ревью</p>
            )}
          </nav>
        </aside>
        <main className="cr-main">
          <ErrorNotice error={error || detail.error} />
          {id ? (
            review ? (
              <>
                <ReviewHeader review={review} />
                <StageNavigation
                  review={review}
                  stage={stage}
                  setStage={setStage}
                  busy={busy}
                  action={action}
                />
                {review.Error && <ErrorNotice error={review.Error} />}
                <div className="cr-stage-content">
                  {stage === 'context' ? (
                    <ReviewContext key={review.ID} review={review} />
                  ) : stage === 'review' ? (
                    <ReviewExecution
                      key={`${review.ID}/review`}
                      review={review}
                      stageID="review"
                    />
                  ) : (
                    <ReviewResults
                      key={`${review.ID}/results`}
                      review={review}
                    />
                  )}
                </div>
              </>
            ) : (
              !detail.error && (
                <div className="cr-skeleton-group" aria-label="Загрузка ревью">
                  <div className="cr-skeleton" />
                  <div className="cr-skeleton" />
                </div>
              )
            )
          ) : (
            <StartReview
              defaults={defaults.data}
              defaultsError={defaults.error}
              onCreated={(r) => {
                selectReview(r.ID);
                refresh((n) => n + 1);
              }}
            />
          )}
        </main>
      </ResizableRunList>
    </div>
  );
}
function HistoryItem({
  review,
  selected,
  unread,
  onClick,
}: {
  review: Review;
  selected: boolean;
  unread: boolean;
  onClick: () => void;
}) {
  const { blocking, other } = counts(review),
    date = dateTime(review.UpdatedAt),
    time = date.slice(-5);
  return (
    <button
      className={`cr-history-item ${reviewColor(review)} ${selected ? 'selected' : ''}`}
      onClick={onClick}
      aria-current={selected ? 'page' : undefined}
    >
      <div className="cr-history-top">
        <span
          className={`cr-status-dot ${reviewColor(review)} ${review.State === 'running' ? 'pulse' : ''}`}
        />
        <strong>{review.Title || 'Новое ревью'}</strong>
        <div className="cr-counters">
          {!['running', 'pending'].includes(review.State) && (
            <>
              <Counter value={blocking} danger={blocking > 0} />
              <Counter value={other} />
            </>
          )}
          {unread && (
            <span
              className="cr-unread"
              title="Непросмотренные изменения"
              aria-label="Непросмотренные изменения"
            >
              <Icon data={Bell} size={12} />
            </span>
          )}
        </div>
      </div>
      <div className="cr-history-bottom">
        {review.Context.Tasks?.[0] && (
          <Label size="xs">{review.Context.Tasks[0].ID}</Label>
        )}
        <span>
          {date.slice(0, -5)}
          <b>{time}</b>
        </span>
      </div>
    </button>
  );
}
export function Counter({
  value,
  danger = false,
}: {
  value: number;
  danger?: boolean;
}) {
  return (
    <span
      className={`cr-counter ${danger ? 'danger' : ''}`}
      title={`${value} ${danger ? 'блокирующих' : 'неблокирующих'} замечаний`}
    >
      {value > 9 ? (
        <>
          9<sup>+</sup>
        </>
      ) : (
        value
      )}
    </span>
  );
}
function StartReview({
  defaults,
  defaultsError,
  onCreated,
}: {
  defaults?: { Config: Config; CWD: string };
  defaultsError?: string;
  onCreated: (r: Review) => void;
}) {
  const [prompt, setPrompt] = useState(''),
    [cwd, setCwd] = useState(''),
    [config, setConfig] = useState(defaultConfig),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const [modelRevision, reloadModels] = useState(0);
  const models = usePoll<
    {
      id: string;
      model: string;
      displayName: string;
      supportedReasoningEfforts: {
        reasoningEffort: string;
        description: string;
      }[];
    }[]
  >(`/api/reviews/models?revision=${modelRevision}`, 0);
  const [projectOpen, setProjectOpen] = useState(false),
    input = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    if (defaults) {
      setConfig(defaults.Config);
      setCwd(defaults.CWD);
    }
  }, [defaults]);
  const start = async () => {
    setBusy(true);
    setError('');
    try {
      const r = await mutate<Review>('/api/reviews', {
        Prompt: prompt,
        CWD: cwd,
        Config: config,
      });
      onCreated(r);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="cr-start">
      <h1>Code Review</h1>
      <div className="cr-composer">
        <TextArea
          controlRef={input}
          value={prompt}
          onUpdate={setPrompt}
          placeholder="Укажите ссылку на задачу и ссылку на изменения"
          minRows={5}
          controlProps={{ 'aria-label': 'Задача для Code Review' }}
        />
        <div className="cr-composer-actions">
          <Button
            onClick={() => {
              setPrompt((s) => `${s}${s ? '\n\n' : ''}${reviewTemplate}`);
              input.current?.focus();
            }}
          >
            Провести code review pull request
          </Button>
          <Button
            view="action"
            size="l"
            loading={busy}
            disabled={busy || !prompt.trim() || !cwd.trim()}
            onClick={() => void start()}
          >
            Запустить ревью
          </Button>
        </div>
      </div>
      <div className="cr-model-config">
        {stages.map((s) => (
          <section key={s.id}>
            <strong>{s.label}</strong>
            <Choice
              aria-label={`Модель: ${s.label}`}
              value={config[s.key].Model}
              options={
                models.data?.map((m) => ({
                  value: m.model,
                  content: m.displayName || m.model,
                })) || [
                  { value: config[s.key].Model, content: config[s.key].Model },
                ]
              }
              onUpdate={(Model) =>
                setConfig((c) => {
                  const efforts =
                    models.data?.find((m) => m.model === Model)
                      ?.supportedReasoningEfforts || [];
                  const Effort = efforts.some(
                    (e) => e.reasoningEffort === c[s.key].Effort,
                  )
                    ? c[s.key].Effort
                    : efforts[0]?.reasoningEffort || c[s.key].Effort;
                  return { ...c, [s.key]: { Model, Effort } };
                })
              }
            />
            <Choice
              aria-label={`Effort: ${s.label}`}
              value={config[s.key].Effort}
              options={
                models.data
                  ?.find((m) => m.model === config[s.key].Model)
                  ?.supportedReasoningEfforts.map((e) => ({
                    value: e.reasoningEffort,
                    content: `Effort: ${e.reasoningEffort}`,
                  })) || [
                  {
                    value: config[s.key].Effort,
                    content: `Effort: ${config[s.key].Effort}`,
                  },
                ]
              }
              onUpdate={(Effort) =>
                setConfig((c) => ({ ...c, [s.key]: { ...c[s.key], Effort } }))
              }
            />
          </section>
        ))}
      </div>
      <div>
        {models.error && (
          <>
            <ErrorNotice error={models.error} />
            <Button onClick={() => reloadModels((n) => n + 1)}>
              Загрузить модели повторно
            </Button>
          </>
        )}
      </div>
      <div className="cr-project">
        <Button view="flat" onClick={() => setProjectOpen(!projectOpen)}>
          Проект: {cwd || 'Выберите рабочую папку'}
        </Button>
        {projectOpen && (
          <TextInput
            value={cwd}
            onUpdate={setCwd}
            placeholder="Абсолютный путь к проекту"
            controlProps={{ 'aria-label': 'Рабочая папка проекта' }}
          />
        )}
      </div>
      <ErrorNotice error={error || defaultsError} />
    </section>
  );
}
function ReviewHeader({ review }: { review: Review }) {
  const [prompt, setPrompt] = useState(false),
    [metrics, setMetrics] = useState(false),
    usage = usageSummary(review);
  const active = ['running', 'pending'].includes(review.State);
  return (
    <header className="cr-review-header">
      <div className="cr-review-meta">
        <div className="cr-review-title">
          <Tooltip content="Исходная постановка">
            <Button
              aria-label="Исходная постановка"
              onClick={() => setPrompt(true)}
            >
              <Icon data={FileText} />
            </Button>
          </Tooltip>
          {review.Title ? (
            <h1>{review.Title}</h1>
          ) : !['running', 'pending'].includes(review.State) ? (
            <h1>Ревью изменений</h1>
          ) : (
            <div
              className="cr-skeleton cr-title-skeleton"
              aria-label="Агент формирует название"
            />
          )}
        </div>
        <p className="cr-dates">
          <span>
            <b>Создано:</b> {dateTime(review.CreatedAt)}
          </span>
          <span>·</span>
          <span>
            <b>Обновлено:</b> {dateTime(review.UpdatedAt)}
          </span>
        </p>
        <div className="cr-links">
          {(review.Context.Tasks || []).map((t) => (
            <LinkLabel key={t.ID} label={t.ID} url={t.URL} />
          ))}
          {(review.Context.Links || []).map((l, i) => (
            <LinkLabel key={`${l.URL}-${i}`} label={l.Label} url={l.URL} />
          ))}
        </div>
      </div>
      <button
        className="cr-spend"
        onClick={() => setMetrics(true)}
        aria-label="Затраты ревью по этапам"
      >
        <div>
          <strong>Затраты ревью</strong>
          <span>
            {duration(review.CreatedAt, active ? undefined : review.UpdatedAt)}
          </span>
        </div>
        {usage.rows.map((r) => (
          <div key={r.label}>
            <span>{r.label}</span>
            <span>{number(r.tokens)}</span>
            <span>{dollars(r.cost)}</span>
          </div>
        ))}
        <div>
          <strong>{usage.complete ? 'API-итого' : 'API · частично'}</strong>
          <strong>
            {usage.total == null
              ? '—'
              : `≈ ${dollars(usage.total)} / ${(usage.total * review.Config.RublesPerDollar).toLocaleString('ru-RU', { maximumFractionDigits: 2 })} ₽`}
          </strong>
        </div>
      </button>
      <Dialog
        open={prompt}
        onOpenChange={setPrompt}
        title="Исходная постановка"
      >
        <MarkdownDocument
          text={review.Prompt}
          label="Запрос пользователя"
          reader
        />
      </Dialog>
      <Dialog
        open={metrics}
        onOpenChange={setMetrics}
        title="Затраты по этапам"
      >
        {stages.map((s) => {
          const st = review.Stages?.find((v) => v.ID === s.id);
          return (
            <section key={s.id} className="cr-metric-stage">
              <h3>
                {s.label} · {review.Config[s.key].Model} ·{' '}
                {review.Config[s.key].Effort}
              </h3>
              {(st?.Attempts || []).map((a) => (
                <div key={a.Number} className="cr-metric-attempt">
                  <p>
                    Попытка {a.Number} · {duration(a.StartedAt, a.FinishedAt)} ·{' '}
                    {a.Usage.Complete
                      ? dollars(a.Usage.CostUSD)
                      : 'Стоимость неполная'}
                  </p>
                  {(a.ThreadUsage?.length
                    ? a.ThreadUsage
                    : [
                        {
                          ThreadID: a.ThreadID,
                          Model: review.Config[s.key].Model,
                          Effort: review.Config[s.key].Effort,
                          Usage: a.Usage,
                        },
                      ]
                  ).map((t) => (
                    <p className="cr-muted" key={`${t.ThreadID}/${t.Model}`}>
                      {t.Model} · {t.Effort} · Input:{' '}
                      {number(
                        t.Usage.InputTokens == null ||
                          t.Usage.CachedInputTokens == null
                          ? null
                          : Math.max(
                              0,
                              t.Usage.InputTokens - t.Usage.CachedInputTokens,
                            ),
                      )}{' '}
                      · Cache: {number(t.Usage.CachedInputTokens)} · Output:{' '}
                      {number(t.Usage.OutputTokens)} ·{' '}
                      {dollars(t.Usage.CostUSD)}
                    </p>
                  ))}
                </div>
              ))}
            </section>
          );
        })}
        <p className="cr-muted">
          API-эквивалент, не списание с подписки. $1 ={' '}
          {review.Config.RublesPerDollar} ₽. Неизвестные метрики и тарифы
          обозначены «—».{' '}
          {!usage.complete &&
            'Часть расхода не предоставлена; показанные затраты не являются полной суммой.'}
        </p>
        {Object.entries(review.Config.Prices || {}).map(([model, p]) => (
          <section key={model} className="cr-metric-stage">
            <strong>{model}</strong>
            <p>
              {p.AsOf} · {p.Basis}
            </p>
            <LinkLabel label="Источник тарифа" url={p.Source} />
          </section>
        ))}
      </Dialog>
    </header>
  );
}
function StageNavigation({
  review,
  stage,
  setStage,
  busy,
  action,
}: {
  review: Review;
  stage: StageID;
  setStage: (s: StageID) => void;
  busy: boolean;
  action: (s: string) => Promise<void>;
}) {
  const { blocking, other } = counts(review);
  return (
    <div className="cr-stage-bar">
      <div className="cr-stage-tabs" role="tablist" aria-label="Этапы ревью">
        {stages.map((s, i) => {
          const state =
            review.Stages?.find((v) => v.ID === s.id)?.State || 'pending';
          const color =
            state === 'running'
              ? 'info'
              : state === 'failed' ||
                  (s.id === 'presentation' && state === 'succeeded' && blocking)
                ? 'danger'
                : state === 'succeeded'
                  ? 'success'
                  : 'neutral';
          return (
            <div className="cr-stage-entry" key={s.id}>
              {i > 0 && (
                <Icon className="cr-step-arrow" data={ArrowRight} size={18} />
              )}
              <div className="cr-stage-with-model">
                <button
                  type="button"
                  role="tab"
                  aria-selected={stage === s.id}
                  tabIndex={stage === s.id ? 0 : -1}
                  onKeyDown={(event) => {
                    const next =
                      event.key === 'ArrowRight'
                        ? (i + 1) % stages.length
                        : event.key === 'ArrowLeft'
                          ? (i + stages.length - 1) % stages.length
                          : event.key === 'Home'
                            ? 0
                            : event.key === 'End'
                              ? stages.length - 1
                              : -1;
                    if (next < 0) return;
                    event.preventDefault();
                    setStage(stages[next].id);
                    event.currentTarget
                      .closest('[role="tablist"]')
                      ?.querySelectorAll<HTMLButtonElement>('[role="tab"]')
                      [next]?.focus();
                  }}
                  className={`cr-stage-button ${color} ${stage === s.id ? 'selected' : ''}`}
                  onClick={() => setStage(s.id)}
                >
                  <span
                    className={`cr-status-dot ${color} ${state === 'running' ? 'pulse' : ''}`}
                  />
                  {s.label}
                </button>
                <small>
                  {review.Config[s.key].Model} · {review.Config[s.key].Effort}
                </small>
              </div>
              {s.id === 'presentation' && state === 'succeeded' && (
                <div className="cr-stage-counters">
                  <Counter value={blocking} danger={blocking > 0} />
                  <Counter value={other} />
                </div>
              )}
            </div>
          );
        })}
      </div>
      <div className="cr-process-actions">
        {['running', 'pending'].includes(review.State) ? (
          <Button
            loading={busy}
            disabled={busy || review.StopRequested}
            onClick={() => void action('stop')}
          >
            <Icon data={Square} size={16} />
            {review.StopRequested ? 'Останавливаем…' : 'Остановить'}
          </Button>
        ) : (
          ['failed', 'interrupted', 'stopped'].includes(review.State) && (
            <Button
              loading={busy}
              disabled={busy}
              onClick={() => void action('retry')}
            >
              <Icon data={ArrowRotateRight} size={16} />
              Повторить
            </Button>
          )
        )}
      </div>
    </div>
  );
}
