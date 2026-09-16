import { useState } from 'react';
import { MarkdownDocument } from './MarkdownDocument';
import type { Graph } from '../types';
import { usePoll } from '../hooks/api';
import { Choice, ErrorNotice } from './ui';

// Выбор из графа передаётся вкладке по step/visit. Самостоятельное открытие
// вкладки позволяет выбрать любой кубик, в том числе ещё не запущенный.
export function ContinuationPanel({
  runID,
  stepID,
  visitID,
  preview,
  onSelectionChange,
}: {
  runID: string;
  stepID?: string;
  visitID?: string;
  preview?: Graph;
  onSelectionChange: (step: string, visit?: string) => void;
}) {
  const { data, error } = usePoll<Graph>(
    preview ? null : `/api/graph/${encodeURIComponent(runID)}`,
  );
  const graph = preview || data;
  const selected =
    graph?.Nodes?.find((node) => node.ID === stepID) || graph?.Nodes?.[0];
  const executions = (graph?.Executions || []).filter(
    (entry) => entry.StepID === selected?.ID,
  );
  const execution =
    executions.find((entry) => entry.Key === visitID) || executions.at(-1);
  return (
    <section className="continuation-tab">
      <h2>Продолжить в новом чате Codex</h2>
      <ErrorNotice error={error} />
      {graph ? (
        <>
          <div className="actions">
            <Choice
              aria-label="Кубик для продолжения"
              value={selected?.ID || ''}
              onUpdate={(value) => onSelectionChange(value)}
              options={(graph.Nodes || []).map((node) => ({
                value: node.ID,
                content: node.ID,
              }))}
            />
            {executions.length > 1 && (
              <Choice
                aria-label="Посещение для продолжения"
                value={execution?.Key || ''}
                onUpdate={(value) => onSelectionChange(selected!.ID, value)}
                options={executions.map((entry) => ({
                  value: entry.Key,
                  content: `Посещение ${entry.Visit || 1}`,
                }))}
              />
            )}
          </div>
          <Continuation
            cube={execution?.Prompt || selected?.Prompt || graph.Prompt}
            workflow={graph.Prompt}
          />
        </>
      ) : (
        !error && <p>Загрузка контекста…</p>
      )}
    </section>
  );
}
export function Continuation({
  cube,
  workflow,
}: {
  cube: string;
  workflow: string;
}) {
  const [scope, setScope] = useState('cube');
  return (
    <section>
      <Choice
        aria-label="Контекст продолжения"
        value={scope}
        onUpdate={setScope}
        options={[
          { value: 'cube', content: 'Этот кубик' },
          { value: 'workflow', content: 'Весь workflow' },
        ]}
      />
      <MarkdownDocument
        text={scope === 'workflow' ? workflow : cube}
        label="Промпт продолжения"
        copyLabel="Скопировать промпт"
      />
    </section>
  );
}
