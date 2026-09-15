import { lazy, Suspense, useEffect, useState } from 'react';
import {
  Link,
  Route,
  Routes,
  useLocation,
  useParams,
  useSearchParams,
} from 'react-router-dom';
import * as Tabs from '@radix-ui/react-tabs';
import type { Dashboard, Run, Step } from './types';
import { usePoll } from './hooks/api';
import { Button, Dialog, ErrorNotice } from './components/ui';
import { findRun, RunTree, type Selection } from './components/Tree';
import { RunInfo } from './components/RunInfo';
import { previewGraph } from './components/previewGraph';

// Граф загружается отдельным модулем: фильтры и список доступны до загрузки layout.
const WorkflowGraph = lazy(() =>
  import('./components/WorkflowGraph').then((module) => ({
    default: module.WorkflowGraph,
  })),
);

// Состояние фильтров находится в URL: back/forward и ссылки воспроизводят вид.
// Локальный выбор и состояние React Flow не пересоздаются от каждого polling.
function DashboardPage() {
  const location = useLocation();
  const preview = location.pathname === '/preview';
  const [params, setParams] = useSearchParams();
  const [revision, setRevision] = useState(0);
  const { data, error } = usePoll<Dashboard>(
    `${preview ? '/api/preview' : '/api/dashboard'}?${params.toString()}&revision=${revision}`,
    preview ? 0 : 3000,
  );
  const [selection, setSelection] = useState<Selection>();
  const [tab, setTab] = useState('graph');
  const [scheduleOpen, setScheduleOpen] = useState(false);
  const change = (values: Record<string, string>) => {
    const next = new URLSearchParams(params);
    next.delete('page');
    for (const [key, value] of Object.entries(values)) {
      if (value) next.set(key, value);
      else next.delete(key);
    }
    setParams(next);
  };
  const select = (run: Run, step?: Step) => {
    setSelection({ runID: run.ID, stepKey: step?.Key });
    setTab(step ? 'info' : 'graph');
  };
  const filterLink = (url: string) =>
    `${location.pathname}${new URL(url || '/', window.location.origin).search}`;
  const roots = data?.Roots || [];
  const run = findRun(roots, selection?.runID) || roots[0];
  const step =
    run?.ID === selection?.runID
      ? run?.Steps?.find((item) => item.Key === selection?.stepKey)
      : undefined;
  const scheduled = data?.Scheduled || [];
  return (
    <div className="app">
      <header className="app-header">
        <Link className="brand" to={preview ? '/preview' : '/'}>
          <img src="/assets/lawa-logo.png" alt="" />
          Lawa
        </Link>
        {preview && <span className="test-label">TEST DATA</span>}
        <div className="header-right">
          {scheduled.length ? (
            <Button onClick={() => setScheduleOpen(true)}>
              {scheduled[0].WorkflowID}{' '}
              <span className="muted">{scheduled[0].Remaining}</span> ›
            </Button>
          ) : (
            <span className="muted">Нет запланированных запусков</span>
          )}
        </div>
      </header>
      <main className="dashboard">
        <ErrorNotice error={error} />
        {!data ? (
          <p className="muted">
            {error ? 'Не удалось загрузить запуски.' : 'Загрузка…'}
          </p>
        ) : (
          <>
            <div className="filters">
              <nav aria-label="Какие workflow показывать">
                <Link
                  className={data.Filter.ActiveOnly ? 'active' : ''}
                  to={filterLink(data.Filter.ActiveURL)}
                >
                  Активные
                </Link>
                <Link
                  className={!data.Filter.ActiveOnly ? 'active' : ''}
                  to={filterLink(data.Filter.AllURL)}
                >
                  Все
                </Link>
              </nav>
              <nav aria-label="Какие состояния показывать">
                {[
                  [
                    data.Filter.AllStatesURL,
                    'Все состояния',
                    !data.Filter.WorkingOnly && !data.Filter.FailedOnly,
                  ],
                  [data.Filter.WorkingURL, 'В работе', data.Filter.WorkingOnly],
                  [
                    data.Filter.FailedURL,
                    'Сломавшиеся',
                    data.Filter.FailedOnly,
                  ],
                ].map(([url, label, active]) => (
                  <Link
                    className={active ? 'active' : ''}
                    key={String(label)}
                    to={filterLink(String(url))}
                  >
                    {label}
                  </Link>
                ))}
              </nav>
              <select
                aria-label="Период"
                value={data.Filter.Period}
                onChange={(event) => change({ period: event.target.value })}
              >
                {data.Filter.Periods.map((period) => (
                  <option key={period.Value} value={period.Value}>
                    {period.Label}
                  </option>
                ))}
              </select>
              <Link className="button" to={`${location.pathname}?period=24h`}>
                Сбросить
              </Link>
              {data.Pagination.Visible && (
                <nav aria-label="Страницы" className="pagination">
                  {data.Pagination.PreviousURL && (
                    <Link to={filterLink(data.Pagination.PreviousURL)}>
                      ← Новее
                    </Link>
                  )}
                  {(data.Pagination.Items || []).map((item) => (
                    <Link
                      key={item.Label}
                      className={item.Current ? 'active' : ''}
                      to={filterLink(item.URL)}
                    >
                      {item.Label}
                    </Link>
                  ))}
                  {data.Pagination.NextURL && (
                    <Link to={filterLink(data.Pagination.NextURL)}>
                      Старее →
                    </Link>
                  )}
                </nav>
              )}
            </div>
            {!!data.Problems?.length && (
              <div className="problems">
                {data.Problems.map((problem, index) => (
                  <p className="error" key={`${problem.Name}/${index}`}>
                    <strong>{problem.Name}</strong> — {problem.Message}
                  </p>
                ))}
              </div>
            )}
            <div className="inspector">
              {data.Filter.Focused && (
                <nav className="breadcrumbs" aria-label="Закреплённая папка">
                  {(data.Filter.FocusPath || []).map((part) => (
                    <Button
                      key={part.ID}
                      onClick={() => change({ root: part.ID })}
                    >
                      {part.Name} /
                    </Button>
                  ))}
                </nav>
              )}
              <div className="inspector-body">
                <aside className="tree" aria-label="Дерево workflow и кубиков">
                  <Search
                    key={data.Filter.Query}
                    value={data.Filter.Query}
                    onChange={(value) => change({ q: value })}
                  />
                  <div className="tree-scroll">
                    {data.Filter.Focused && (
                      <Button
                        onClick={() =>
                          change({ root: data.Filter.FocusParentID })
                        }
                      >
                        ../
                      </Button>
                    )}
                    <ul>
                      {roots.map((item) => (
                        <RunTree
                          key={`${item.ID}/${data.Filter.HasActiveQuery}/${data.Filter.WorkingOnly}/${data.Filter.FailedOnly}`}
                          run={item}
                          selection={
                            run
                              ? { runID: run.ID, stepKey: step?.Key }
                              : undefined
                          }
                          onSelect={select}
                          onFocus={(id) => change({ root: id })}
                          expanded={
                            data.Filter.HasActiveQuery ||
                            data.Filter.WorkingOnly ||
                            data.Filter.FailedOnly
                          }
                        />
                      ))}
                    </ul>
                    {!roots.length && (
                      <p className="muted">{data.EmptyMessage}</p>
                    )}
                  </div>
                </aside>
                <section className="inspector-details">
                  {run ? (
                    <Tabs.Root
                      className="run-tabs"
                      value={tab}
                      onValueChange={setTab}
                    >
                      <Tabs.List className="tabs" aria-label="Вид workflow">
                        <Tabs.Trigger value="graph">Граф</Tabs.Trigger>
                        <Tabs.Trigger value="info">Информация</Tabs.Trigger>
                      </Tabs.List>
                      <Tabs.Content className="graph-tab" value="graph">
                        <Suspense
                          fallback={<p className="loading">Загрузка графа…</p>}
                        >
                          <WorkflowGraph
                            key={run.ID}
                            runID={run.ID}
                            stepID={step?.StepID}
                            visitID={step?.VisitID}
                            preview={preview ? previewGraph(run) : undefined}
                          />
                        </Suspense>
                      </Tabs.Content>
                      <Tabs.Content className="info-tab" value="info">
                        <RunInfo
                          key={`${run.ID}/${step?.Key}`}
                          run={run}
                          step={step}
                          preview={preview}
                          onDeleted={() => {
                            setSelection(undefined);
                            setRevision((value) => value + 1);
                          }}
                        />
                      </Tabs.Content>
                    </Tabs.Root>
                  ) : (
                    <p className="empty">{data.EmptyMessage}</p>
                  )}
                </section>
              </div>
            </div>
          </>
        )}
      </main>
      <Dialog
        open={scheduleOpen}
        onOpenChange={setScheduleOpen}
        title="Расписание запусков"
      >
        <div className="schedule">
          {scheduled.map((item) => (
            <article key={item.SeriesID}>
              <h3>{item.WorkflowID}</h3>
              <p className={item.Overdue ? 'tone-failed' : ''}>
                {item.Remaining}
              </p>
              <p>Запуск: {item.Next}</p>
              <small>
                {item.Schedule} · {item.Progress}
              </small>
            </article>
          ))}
        </div>
      </Dialog>
    </div>
  );
}

// Debounce относится только к поиску, а не к остальным фильтрам. Enter позволяет
// отправить запрос сразу; cleanup отменяет отложенную смену уже покинутого вида.
function Search({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}) {
  const [text, setText] = useState(value);
  useEffect(() => {
    if (text === value) return;
    const timer = setTimeout(() => onChange(text), 700);
    return () => clearTimeout(timer);
  }, [text, value, onChange]);
  return (
    <form
      className="search"
      onSubmit={(event) => {
        event.preventDefault();
        onChange(text);
      }}
    >
      <input
        type="search"
        aria-label="Поиск"
        placeholder="Поиск по workflow, кубикам и тикетам…"
        maxLength={300}
        value={text}
        onChange={(event) => setText(event.target.value)}
      />
    </form>
  );
}
function GraphPage() {
  const { run = '' } = useParams();
  const [params, setParams] = useSearchParams();
  return (
    <div className="app">
      <header className="app-header">
        <Link className="brand" to="/">
          <img src="/assets/lawa-logo.png" alt="" />
          Lawa
        </Link>
        <Link to="/">← Все запуски</Link>
      </header>
      <main className="standalone-graph">
        <Suspense fallback={<p className="loading">Загрузка графа…</p>}>
          <WorkflowGraph
            key={run}
            runID={run}
            stepID={params.get('step') || undefined}
            visitID={params.get('visit') || undefined}
            onSelectionChange={(step, visit) => {
              const next = new URLSearchParams(params);
              next.set('step', step);
              if (visit) next.set('visit', visit);
              else next.delete('visit');
              setParams(next, { replace: true });
            }}
          />
        </Suspense>
      </main>
    </div>
  );
}
export default function App() {
  return (
    <Routes>
      <Route path="/" element={<DashboardPage />} />
      <Route path="/preview" element={<DashboardPage />} />
      <Route path="/graph/:run" element={<GraphPage />} />
      <Route
        path="*"
        element={
          <main>
            <h1>Страница не найдена</h1>
            <Link to="/">Все запуски</Link>
          </main>
        }
      />
    </Routes>
  );
}
