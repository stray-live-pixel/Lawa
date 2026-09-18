import { useRef, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { TabProvider, TabList, Tab, TabPanel } from '@gravity-ui/uikit';
import { WorkflowGraph } from '../components/WorkflowGraph';
import { WorkflowSource, type Source } from '../components/WorkflowSource';
import { ThemePicker } from '../components/Theme';
import { ErrorNotice } from '../components/ui';
import { usePoll } from '../hooks/api';
import type { Graph } from '../types';

// Самостоятельная страница читает один снимок, без dashboard/office API.
// Выбор шага хранится в URL, чтобы back/forward восстанавливали читаемую инструкцию.
export default function WorkflowDefinition() {
  const { data: graph, error } = usePoll<Graph>('/api/definition', 0);
  const { data: source, error: sourceError } = usePoll<Source>(
    '/api/definition/source',
    0,
  );
  const [tab, setTab] = useState('graph');
  const [params, setParams] = useSearchParams();
  const selectedStep = useRef(params.get('step'));
  selectedStep.current = params.get('step');
  // React Flow может сообщить один выбор через click и selection change.
  // Не добавляем два одинаковых адреса, иначе back потребует лишнего нажатия.
  const select = (step: string) => {
    if (selectedStep.current === step) return;
    selectedStep.current = step;
    setParams({ step });
  };
  return (
    <div className="app">
      <header className="app-header">
        <strong>Lawa · Просмотр workflow</strong>
        <div className="header-right">
          <ThemePicker />
        </div>
      </header>
      <main className="standalone-graph">
        <div className="run-tabs">
          <ErrorNotice error={error || sourceError} />
          <TabProvider value={tab} onUpdate={setTab}>
            <TabList className="tabs" aria-label="Вид workflow">
              <Tab value="graph">Схема и инструкции</Tab>
              <Tab value="source">Исходники</Tab>
            </TabList>
            <TabPanel
              value="graph"
              className="graph-tab"
              hidden={tab !== 'graph'}
            >
              {graph ? (
                <WorkflowGraph
                  runID=""
                  definition={graph}
                  stepID={params.get('step') || undefined}
                  onSelectionChange={select}
                />
              ) : (
                !error && <p>Загрузка определения…</p>
              )}
            </TabPanel>
            <TabPanel
              value="source"
              className="info-tab"
              hidden={tab !== 'source'}
            >
              {tab === 'source' &&
                (source ? (
                  <WorkflowSource runID="" snapshot={source} />
                ) : (
                  !sourceError && <p>Загрузка исходников…</p>
                ))}
            </TabPanel>
          </TabProvider>
        </div>
      </main>
    </div>
  );
}
