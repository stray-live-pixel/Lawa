import { useState } from 'react';
import { Cube as Box, ChevronRight, NodesRight, Pin } from '@gravity-ui/icons';
import { Button, Icon, Label } from '@gravity-ui/uikit';
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
    <li className={`tree-branch ${shown ? 'tree-branch-open' : ''}`}>
      <div
        className={`tree-row ${selection?.runID === run.ID && !selection.stepKey ? 'selected' : ''}`}
      >
        <Button
          view="flat"
          size="m"
          className="icon-button"
          onClick={toggle}
          aria-label={`Развернуть ${run.Name}`}
          aria-expanded={shown}
        >
          <Icon
            data={ChevronRight}
            size={14}
            className={shown ? 'rotate' : ''}
          />
        </Button>
        <Button
          view="flat"
          className="tree-select"
          onClick={() => onSelect(run)}
          title={run.Name}
          aria-current={
            selection?.runID === run.ID && !selection.stepKey
              ? 'true'
              : undefined
          }
        >
          {/* Единый контейнер не даёт Button вынести Icon в отдельный слот:
              иконка, имя и счётчик используют одну сетку независимо от тикета. */}
          <span className="tree-entry">
            <Icon data={NodesRight} size={16} className={`tone-${run.State}`} />
            <span className="tree-name">{run.Name}</span>
            <span className={`tree-meta ${run.TicketID ? 'has-ticket' : ''}`}>
              {run.TicketID && (
                <Label
                  size="xs"
                  theme="info"
                  className="ticket"
                  title={`${run.TicketID} · ${run.TicketTitle}`}
                >
                  {run.TicketID}
                </Label>
              )}
              <small className="tree-count">
                {run.CompletedSteps}/{run.TotalSteps}
              </small>
            </span>
          </span>
        </Button>
        <Button
          view="flat"
          size="m"
          className="icon-button pin"
          onClick={() => onFocus(run.ID)}
          aria-label={`Сделать ${run.Name} корневой папкой`}
        >
          <Icon data={Pin} size={13} />
        </Button>
      </div>
      {shown && (
        <ul className="tree-children">
          {(run.Steps || []).map((step) => (
            <li key={step.Key}>
              <Button
                view="flat"
                size="s"
                className={`tree-step ${selection?.runID === run.ID && selection.stepKey === step.Key ? 'selected' : ''}`}
                onClick={() => onSelect(run, step)}
                title={step.ID}
              >
                <span className="tree-entry">
                  <Icon data={Box} size={16} className={`tone-${step.State}`} />
                  <span className="tree-name">{step.ID}</span>
                </span>
              </Button>
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
