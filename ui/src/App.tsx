import { ResizableRunList } from './components/ResizableRunList';
import { lazy, Suspense, useEffect, useState } from 'react';
import {
  Link as RouterLink,
  Route,
  Routes,
  useLocation,
  useParams,
  useSearchParams,
} from 'react-router-dom';
import {
  TabProvider,
  TabList,
  Tab,
  TabPanel,
  TextInput,
  Label,
  Button as NavigationButton,
  Icon,
  Loader,
  Tooltip,
} from '@gravity-ui/uikit';
import { Magnifier, Clock, ArrowLeft, ArrowRight } from '@gravity-ui/icons';
import {
  DashboardFilters,
  filtersChanged,
} from './components/DashboardFilters';
import { ThemePicker } from './components/Theme';
import type { LinkProps } from 'react-router-dom';

// RouterLink сохраняет SPA-навигацию и историю, Gravity задаёт стиль ссылок.
function Link(props: LinkProps) {
  return <NavigationButton view="flat" component={RouterLink} {...props} />;
}
import type { Dashboard, Run, Step } from './types';
import { usePoll } from './hooks/api';
import { Button, Dialog, ErrorNotice } from './components/ui';
import { findRun, RunTree, type Selection } from './components/Tree';
import { ContinuationPanel } from './components/Continuation';
import { RunInfo } from './components/RunInfo';
import { WorkflowSource } from './components/WorkflowSource';
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
  const [graphChoice, setGraphChoice] = useState<{
    runID: string;
    step: string;
    visit?: string;
  }>();
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
    setGraphChoice({
      runID: run.ID,
      step: step?.StepID || '',
      visit: step?.VisitID,
    });
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
      <main className="dashboard">
        <ErrorNotice error={error} />
        {!data ? (
          <p className="muted">
            {error ? 'Не удалось загрузить запуски.' : 'Загрузка…'}
          </p>
        ) : (
          <>
            {!!data.Problems?.length && (
              <div className="problems">
                {data.Problems.map((problem, index) => (
                  <ErrorNotice
                    key={`${problem.Name}/${index}`}
                    error={`${problem.Name} — ${problem.Message}`}
                  />
                ))}
              </div>
            )}
            <div className="inspector">
              <ResizableRunList>
                <aside className="tree" aria-label="Дерево workflow и кубиков">
                  <header className="app-header">
                    <div className="brand">
                      <img src="/assets/lawa-logo.png" alt="" />
                      Lawa
                    </div>
                    {preview && <Label size="xs">TEST DATA</Label>}
                    <div className="header-tools">
                      <ThemePicker />
                      <Tooltip
                        content={`Запланированные запуски: ${scheduled.length}`}
                      >
                        <Button
                          view="flat"
                          className="schedule-button"
                          aria-label="Запланированные запуски"
                          onClick={() => setScheduleOpen(true)}
                        >
                          <span className="schedule-icon">
                            <Icon data={Clock} />
                            {scheduled.length > 0 && (
                              <span
                                className="schedule-dot"
                                aria-hidden="true"
                              />
                            )}
                          </span>
                        </Button>
                      </Tooltip>
                    </div>
                  </header>
                  {data.Filter.Focused && (
                    <nav
                      className="breadcrumbs"
                      aria-label="Закреплённая папка"
                    >
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

                  <div className="filters">
                    <DashboardFilters
                      data={data}
                      onChange={change}
                      onReset={() => setParams({ period: '24h' })}
                    />
                  </div>
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
                      <div className="empty-search">
                        <p className="muted">{data.EmptyMessage}</p>
                        {filtersChanged(data) && (
                          <Button
                            view="flat"
                            onClick={() => setParams({ period: '24h' })}
                          >
                            Сбросить фильтры
                          </Button>
                        )}
                      </div>
                    )}
                  </div>
                  {data.Pagination.Visible && (
                    <footer className="sidebar-footer">
                      <nav aria-label="Страницы" className="pagination">
                        {data.Pagination.PreviousURL && (
                          <NavigationButton
                            view="flat"
                            component={RouterLink}
                            to={filterLink(data.Pagination.PreviousURL)}
                          >
                            <Icon data={ArrowLeft} /> Новее
                          </NavigationButton>
                        )}
                        {(data.Pagination.Items || []).map((item) => (
                          <NavigationButton
                            view="flat"
                            component={RouterLink}
                            key={item.Label}
                            selected={item.Current}
                            to={filterLink(item.URL)}
                          >
                            {item.Label}
                          </NavigationButton>
                        ))}
                        {data.Pagination.NextURL && (
                          <NavigationButton
                            view="flat"
                            component={RouterLink}
                            to={filterLink(data.Pagination.NextURL)}
                          >
                            Старее <Icon data={ArrowRight} />
                          </NavigationButton>
                        )}
                      </nav>
                    </footer>
                  )}
                </aside>
                <section className="inspector-details">
                  {run ? (
                    <div className="run-tabs">
                      <TabProvider value={tab} onUpdate={setTab}>
                        <TabList
                          contentOverflow="scroll"
                          className="tabs"
                          aria-label="Вид workflow"
                        >
                          <Tab value="graph">Граф</Tab>
                          <Tab value="info">Информация</Tab>
                          <Tab value="source">JSON и Markdown</Tab>
                          <Tab value="continue">
                            Продолжить в новом чате Codex
                          </Tab>
                        </TabList>
                        <TabPanel
                          className="info-tab"
                          value="source"
                          hidden={tab !== 'source'}
                        >
                          {tab === 'source' && (
                            <WorkflowSource
                              key={run.ID}
                              runID={run.ID}
                              preview={preview ? previewGraph(run) : undefined}
                            />
                          )}
                        </TabPanel>
                        <TabPanel
                          className="graph-tab"
                          value="graph"
                          hidden={tab !== 'graph'}
                        >
                          {tab === 'graph' && (
                            <>
                              <Suspense
                                fallback={
                                  <div className="loading">
                                    <Loader size="m" />
                                  </div>
                                }
                              >
                                <WorkflowGraph
                                  key={run.ID}
                                  runID={run.ID}
                                  stepID={
                                    graphChoice?.runID === run.ID
                                      ? graphChoice.step
                                      : step?.StepID
                                  }
                                  visitID={
                                    graphChoice?.runID === run.ID
                                      ? graphChoice.visit
                                      : step?.VisitID
                                  }
                                  onSelectionChange={(step, visit) =>
                                    setGraphChoice({
                                      runID: run.ID,
                                      step,
                                      visit,
                                    })
                                  }
                                  preview={
                                    preview ? previewGraph(run) : undefined
                                  }
                                />
                              </Suspense>
                            </>
                          )}
                        </TabPanel>
                        <TabPanel
                          className="info-tab"
                          value="continue"
                          hidden={tab !== 'continue'}
                        >
                          {tab === 'continue' && (
                            <>
                              <ContinuationPanel
                                key={run.ID}
                                runID={run.ID}
                                stepID={
                                  graphChoice?.runID === run.ID
                                    ? graphChoice.step
                                    : step?.StepID
                                }
                                visitID={
                                  graphChoice?.runID === run.ID
                                    ? graphChoice.visit
                                    : step?.VisitID
                                }
                                preview={
                                  preview ? previewGraph(run) : undefined
                                }
                                onSelectionChange={(step, visit) =>
                                  setGraphChoice({ runID: run.ID, step, visit })
                                }
                              />
                            </>
                          )}
                        </TabPanel>
                        <TabPanel
                          className="info-tab"
                          value="info"
                          hidden={tab !== 'info'}
                        >
                          {tab === 'info' && (
                            <>
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
                            </>
                          )}
                        </TabPanel>
                      </TabProvider>
                    </div>
                  ) : (
                    <p className="empty">{data.EmptyMessage}</p>
                  )}
                </section>
              </ResizableRunList>
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
          {!scheduled.length && (
            <p className="muted">Нет запланированных запусков</p>
          )}
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
      <TextInput
        type="search"
        startContent={
          <span className="search-icon">
            <Icon data={Magnifier} size={16} />
          </span>
        }
        controlProps={{ 'aria-label': 'Поиск', maxLength: 300 }}
        placeholder="Поиск…"
        value={text}
        onChange={(event) => setText(event.target.value)}
      />
    </form>
  );
}
function GraphPage() {
  const [tab, setTab] = useState('graph');
  const { run = '' } = useParams();
  const [params, setParams] = useSearchParams();
  const select = (step: string, visit?: string) => {
    const next = new URLSearchParams(params);
    next.set('step', step);
    if (visit) next.set('visit', visit);
    else next.delete('visit');
    setParams(next, { replace: true });
  };
  const props = {
    runID: run,
    stepID: params.get('step') || undefined,
    visitID: params.get('visit') || undefined,
    onSelectionChange: select,
  };
  return (
    <div className="app">
      <header className="app-header">
        <Link className="brand" to="/">
          <img src="/assets/lawa-logo.png" alt="" />
          Lawa
        </Link>
        <Link to="/">Все запуски</Link>
        <div className="header-right">
          <ThemePicker />
        </div>
      </header>
      <main className="standalone-graph">
        <div className="run-tabs">
          <TabProvider value={tab} onUpdate={setTab}>
            <TabList
              contentOverflow="scroll"
              className="tabs"
              aria-label="Вид workflow"
            >
              <Tab value="graph">Граф</Tab>
              <Tab value="continue">Продолжить в новом чате Codex</Tab>
            </TabList>
            <TabPanel
              className="graph-tab"
              value="graph"
              hidden={tab !== 'graph'}
            >
              {tab === 'graph' && (
                <>
                  <Suspense fallback={<p>Загрузка графа…</p>}>
                    <WorkflowGraph key={run} {...props} />
                  </Suspense>
                </>
              )}
            </TabPanel>
            <TabPanel
              className="info-tab"
              value="continue"
              hidden={tab !== 'continue'}
            >
              {tab === 'continue' && (
                <>
                  <ContinuationPanel key={run} {...props} />
                </>
              )}
            </TabPanel>
          </TabProvider>
        </div>
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
