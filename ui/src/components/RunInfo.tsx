import { useState } from 'react';
import type { Run, Step } from '../types';
import { Button, Dialog, ErrorNotice, Facts, Status } from './ui';
import { Trace } from './Trace';
import { MarkdownDocument, MemoryDialog } from './MarkdownDocument';

// Raw memory/events остаются экспортируемыми текстовыми ресурсами. Диалоги
// действий реализованы React/Radix; destructive POST требует отдельного клика.
export function RunInfo({
  run,
  step,
  preview,
  onDeleted,
}: {
  run: Run;
  step?: Step;
  preview: boolean;
  onDeleted: () => void;
}) {
  const [confirm, setConfirm] = useState(false),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const [memoryOpen, setMemoryOpen] = useState(false);
  const [trace, setTrace] = useState<{ url: string; title: string } | null>(
    null,
  );
  const remove = async () => {
    setBusy(true);
    setError('');
    try {
      const response = await fetch(run.DeleteURL, {
        method: 'POST',
        headers: { Accept: 'application/json' },
      });
      if (!response.ok) throw new Error((await response.text()).trim());
      setConfirm(false);
      onDeleted();
    } catch (error) {
      setError(String(error));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="run-info">
      <h2>{step?.ID || run.Name}</h2>
      <Status state={step?.State || run.State} />
      <Facts
        items={[
          ['Workflow', run.Name],
          ['Run', run.ID],
          ['Завершено', `${run.CompletedSteps} из ${run.TotalSteps}`],
          ['Обновлён', step?.Updated || run.Updated],
          ['Тикет', `${run.TicketID} ${run.TicketTitle}`.trim()],
          ['Причина остановки', run.StopReason],
          ['Остановившее посещение', run.StopVisit],
          ['Достигнут лимит', run.StopLimit],
          ...(step
            ? ([
                ['Шаг', step.StepID],
                ['Visit', step.VisitID],
                ['Проход', step.Visit || ''],
                ['Итерация', step.Iteration || ''],
                ['Попытка', step.Attempt || ''],
                ['Причина запуска', step.Trigger],
                ['Решение', step.Decision],
                ['Объяснение', step.Explanation],
                ['Переход', step.Transition],
                ['Пропущенные routes', step.Skipped],
                ['Ограничение', step.Limit],
                ['Техническая ошибка', step.TechnicalError],
                ['Ошибка решения', step.DecisionError],
                ['Процесс', step.Runtime],
                ['Действие', step.Action],
              ] as [string, string | number][])
            : []),
        ]}
      />
      {step?.Result && (
        <MarkdownDocument
          text={step.Result}
          label="Результат работы"
          copyLabel="Скопировать результат"
        />
      )}
      <div className="actions">
        <a
          className="button"
          href={step?.EventsURL || run.EventsURL}
          aria-disabled={preview}
          onClick={(event) => {
            if (preview) event.preventDefault();
          }}
        >
          События
        </a>
        {!step && (
          <a
            className="button"
            href={run.VSCodeURL}
            aria-disabled={preview}
            onClick={(event) => {
              if (preview) event.preventDefault();
            }}
          >
            Папка
          </a>
        )}
        {run.TicketURL && (
          <a
            className="button"
            href={run.TicketURL}
            target="_blank"
            rel="noreferrer"
          >
            Тикет · {run.TicketID}
          </a>
        )}
        {step && (
          <>
            <Button
              disabled={preview}
              onClick={() => setTrace({ url: step.TraceURL, title: step.ID })}
            >
              Live-вывод
            </Button>
            {step.HasMemory && (
              <Button disabled={preview} onClick={() => setMemoryOpen(true)}>
                Память кубика
              </Button>
            )}
          </>
        )}
        {!step && (
          <Button
            className="danger"
            disabled={preview}
            onClick={() => setConfirm(true)}
          >
            Остановить и удалить
          </Button>
        )}
      </div>
      {!step && !!run.ActiveSteps?.length && (
        <section>
          <h3>Сейчас выполняются</h3>
          {run.ActiveSteps.map((item) => (
            <Button
              key={item.Key}
              disabled={preview}
              onClick={() => setTrace({ url: item.TraceURL, title: item.ID })}
            >
              {item.ID} {item.Action}
            </Button>
          ))}
        </section>
      )}
      <MemoryDialog
        url={step?.MemoryURL}
        open={memoryOpen}
        onOpenChange={setMemoryOpen}
      />
      <Dialog
        open={confirm}
        onOpenChange={(open) => {
          if (!busy) setConfirm(open);
        }}
        title="Остановить и удалить workflow?"
        description="Активные агенты будут остановлены. Файлы этого запуска будут удалены без возможности восстановления."
      >
        <ErrorNotice error={error} />
        <p>{run.Name}</p>
        <div className="actions">
          <Button disabled={busy} onClick={() => setConfirm(false)}>
            Отмена
          </Button>
          <Button
            className="danger"
            disabled={busy}
            onClick={() => void remove()}
          >
            {busy ? 'Останавливаю…' : 'Остановить и удалить'}
          </Button>
        </div>
      </Dialog>
      <Dialog
        open={!!trace}
        onOpenChange={(open) => {
          if (!open) setTrace(null);
        }}
        title={trace?.title || 'Работа агента'}
        description="Приватный вывод: сообщения и команды могут содержать секреты."
      >
        {trace && <Trace url={trace.url} />}
      </Dialog>
    </section>
  );
}
