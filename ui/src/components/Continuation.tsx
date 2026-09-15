import { useState } from 'react';
import { MarkdownDocument } from './MarkdownDocument';
import type { Graph } from '../types';
import { usePoll } from '../hooks/api';
import { ErrorNotice } from './ui';

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
            <select
              aria-label="Кубик для продолжения"
              value={selected?.ID || ''}
              onChange={(event) => onSelectionChange(event.target.value)}
            >
              {graph.Nodes?.map((node) => (
                <option key={node.ID}>{node.ID}</option>
              ))}
            </select>
            {executions.length > 1 && (
              <select
                aria-label="Посещение для продолжения"
                value={execution?.Key || ''}
                onChange={(event) =>
                  onSelectionChange(selected!.ID, event.target.value)
                }
              >
                {executions.map((entry) => (
                  <option key={entry.Key} value={entry.Key}>
                    Посещение {entry.Visit || 1}
                  </option>
                ))}
              </select>
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
      <select
        aria-label="Контекст продолжения"
        value={scope}
        onChange={(event) => setScope(event.target.value)}
      >
        <option value="cube">Этот кубик</option>
        <option value="workflow">Весь workflow</option>
      </select>
      <MarkdownDocument
        text={scope === 'workflow' ? workflow : cube}
        label="Промпт продолжения"
        copyLabel="Скопировать промпт"
      />
    </section>
  );
}
