import { useState } from 'react';
import { Icon } from '@gravity-ui/uikit';
import { ChevronRight } from '@gravity-ui/icons';
import { Button, Dialog, ErrorNotice } from '../../components/ui';
import { usePoll } from '../../hooks/api';
import { SavedText, ReviewMarkdown } from './Documents';
import { dateTime } from './helpers';
import type { Event, Review, StageID } from './types';

// Список отражает наблюдённые события, а не предполагаемые возможности агента.
export function ReviewExecution({
  review,
  stageID,
}: {
  review: Review;
  stageID: StageID;
}) {
  const stage = review.Stages?.find((s) => s.ID === stageID),
    attempts = stage?.Attempts || [];
  const [attemptIndex, setAttemptIndex] = useState<number>();
  const attempt = attempts[attemptIndex ?? attempts.length - 1];
  const [selected, setSelected] = useState('prompt'),
    [journal, setJournal] = useState(false);
  const activities = (review.Activities || []).filter(
      (a) => a.Stage === stageID && a.Attempt === attempt?.Number,
    ),
    chosen = activities.find((a) => a.ID === selected);
  const title =
    stageID === 'presentation'
      ? 'Агент готовит результат'
      : 'Агент выполняет ревью';
  return (
    <section className="cr-execution">
      <div className="cr-agent-banner">
        <span
          className={`cr-status-dot ${stage?.State === 'running' ? 'info pulse' : stage?.State === 'succeeded' ? 'success' : 'neutral'}`}
        />
        <span>
          {stage?.State === 'running'
            ? title
            : stage?.State === 'succeeded'
              ? 'Работа агента завершена'
              : stage?.State === 'pending'
                ? 'Этап ожидает запуска'
                : 'Работа агента остановлена'}
        </span>
        <Button onClick={() => setJournal(true)}>Журнал</Button>
      </div>
      {attempts.length > 1 && (
        <div className="cr-actions">
          {attempts.map((a, i) => (
            <Button
              key={a.Number}
              size="s"
              selected={(attemptIndex ?? attempts.length - 1) === i}
              onClick={() => {
                setAttemptIndex(i);
                setSelected('prompt');
              }}
            >
              Попытка {a.Number}
            </Button>
          ))}
        </div>
      )}
      <div className="cr-instructions">
        <nav aria-label="Инструкции и действия агента">
          <button
            className={selected === 'prompt' ? 'selected' : ''}
            onClick={() => setSelected('prompt')}
          >
            Agent Reviewer<small>Исходный промпт</small>
          </button>
          {['skill', 'subagent'].map((kind) => {
            const items = activities.filter((a) => a.Kind === kind);
            return (
              <section key={kind}>
                <p>
                  {kind === 'skill' ? 'Скиллы' : 'Субагенты'} · {items.length}
                </p>
                {items.map((a) => (
                  <button
                    key={a.ID}
                    className={selected === a.ID ? 'selected' : ''}
                    onClick={() => setSelected(a.ID)}
                  >
                    {a.Title}
                    <small>
                      {(
                        {
                          read: 'Прочитан',
                          called: 'Вызван',
                          running: 'В работе',
                          completed: 'Готово',
                          failed: 'Ошибка',
                          pendingInit: 'Запускается',
                        } as Record<string, string>
                      )[a.State] || a.State}{' '}
                      · {new Date(a.At).toLocaleTimeString('ru-RU')}
                    </small>
                  </button>
                ))}
              </section>
            );
          })}
        </nav>
        <article>
          <h3>{chosen?.Title || 'Промпт агента'}</h3>
          {chosen?.Model && (
            <p className="cr-muted">
              {chosen.Model} · {chosen.Effort}
            </p>
          )}
          {chosen ? (
            chosen.DocumentPath ? (
              <SavedText id={review.ID} path={chosen.DocumentPath} />
            ) : (
              <p className="cr-muted">
                Текст документа не был предоставлен источником события
              </p>
            )
          ) : attempt?.PromptPath ? (
            <SavedText id={review.ID} path={attempt.PromptPath} />
          ) : (
            <p className="cr-muted">Промпт появится при запуске этапа</p>
          )}
        </article>
      </div>
      {journal && (
        <Dialog open onOpenChange={setJournal} title="Журнал агента">
          <Journal
            id={review.ID}
            stage={stageID}
            active={stage?.State === 'running' || stage?.State === 'pending'}
          />
        </Dialog>
      )}
    </section>
  );
}
function Journal({
  id,
  stage,
  active,
}: {
  id: string;
  stage: StageID;
  active: boolean;
}) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const { data, error } = usePoll<Event[]>(
    `/api/reviews/${encodeURIComponent(id)}/events`,
    active ? 1500 : 0,
  );
  return (
    <>
      <ErrorNotice error={error} />
      <div className="cr-journal">
        {(data || [])
          .filter((e) => e.Stage === stage)
          .map((e) => (
            <article key={e.ID}>
              <small>
                {dateTime(e.At)} · {e.Kind}
              </small>
              {e.Message && <ReviewMarkdown text={e.Message} />}
              <details
                onToggle={(event) => {
                  const open = event.currentTarget.open;
                  setExpanded((previous) => ({ ...previous, [e.ID]: open }));
                }}
              >
                <summary>
                  Данные события
                  <Icon data={ChevronRight} size={12} />
                </summary>
                {expanded[e.ID] && <pre>{JSON.stringify(e.Data, null, 2)}</pre>}
              </details>
            </article>
          ))}
        {!data?.length && <p className="cr-muted">События ещё не получены</p>}
      </div>
    </>
  );
}
