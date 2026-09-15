import { useState } from 'react';
import { Box, ChevronRight, Folder, Pin } from 'lucide-react';
import type { Run, Step } from '../types';

export interface Selection {
  runID: string;
  stepKey?: string;
}
// Ключи — durable run/visit ID. localStorage хранит только раскрытие дерева;
// отказ хранилища (например private mode) не препятствует работе интерфейса.
export function RunTree({
  run,
  selection,
  onSelect,
  onFocus,
  expanded,
}: {
  run: Run;
  selection?: Selection;
  onSelect: (run: Run, step?: Step) => void;
  onFocus: (id: string) => void;
  expanded: boolean;
}) {
  const [open, setOpen] = useState(() => {
    try {
      const saved = localStorage.getItem(`lawa-tree:${run.ID}`);
      return expanded || (saved === null ? run.Open : saved === 'true');
    } catch {
      return expanded || run.Open;
    }
  });
  const shown = open;
  const toggle = () => {
    setOpen(!shown);
    try {
      localStorage.setItem(`lawa-tree:${run.ID}`, String(!shown));
    } catch {
      /* Раскрытие продолжает работать без persistence. */
    }
  };
  return (
    <li>
      <div
        className={`tree-row ${selection?.runID === run.ID && !selection.stepKey ? 'selected' : ''}`}
      >
        <button
          className="icon-button"
          onClick={toggle}
          aria-label={`Развернуть ${run.Name}`}
          aria-expanded={shown}
        >
          <ChevronRight size={14} className={shown ? 'rotate' : ''} />
        </button>
        <button
          className="tree-select"
          onClick={() => onSelect(run)}
          title={run.Name}
          aria-current={
            selection?.runID === run.ID && !selection.stepKey
              ? 'true'
              : undefined
          }
        >
          <Folder size={16} className={`tone-${run.State}`} />
          <span>{run.Name}</span>
          {run.TicketID && (
            <small className="ticket" title={run.TicketTitle}>
              {run.TicketID}
            </small>
          )}
          <small>
            {run.CompletedSteps}/{run.TotalSteps}
          </small>
        </button>
        <button
          className="icon-button pin"
          onClick={() => onFocus(run.ID)}
          aria-label={`Сделать ${run.Name} корневой папкой`}
        >
          <Pin size={13} />
        </button>
      </div>
      {shown && (
        <ul className="tree-children">
          {(run.Steps || []).map((step) => (
            <li key={step.Key}>
              <button
                className={`tree-step ${selection?.runID === run.ID && selection.stepKey === step.Key ? 'selected' : ''}`}
                onClick={() => onSelect(run, step)}
                title={step.ID}
              >
                <Box size={15} className={`tone-${step.State}`} />
                <span>{step.ID}</span>
              </button>
            </li>
          ))}
          {(run.Children || []).map((child) => (
            <RunTree
              key={child.ID}
              run={child}
              selection={selection}
              onSelect={onSelect}
              onFocus={onFocus}
              expanded={expanded}
            />
          ))}
        </ul>
      )}
    </li>
  );
}
// Поиск ограничен текущим отфильтрованным деревом: скрытый/удалённый запуск
// не удерживается выбранным после обновления списка или смены временного окна.
export function findRun(roots: Run[], id?: string): Run | undefined {
  for (const run of roots) {
    if (run.ID === id) return run;
    const child = findRun(run.Children || [], id);
    if (child) return child;
  }
}
