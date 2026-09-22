import { useEffect, useState } from 'react';
import { Button, Text } from '@gravity-ui/uikit';
import { Dialog, ErrorNotice } from './ui';
import { TeamMarkdown } from './TeamMarkdown';
import { revealTeamMessage } from './TeamConversation';
import type { TeamMessage } from './TeamPhone';

// Карточка общая с backend; неизвестные legacy-поля не получают выдуманных статусов.
export interface TeamTask {
  id: string;
  text: string;
  assignee: string;
  acceptedAt?: string;
  card?: {
    title: string;
    expected: string;
    criteria: string[];
    revision: number;
    version: number;
    status: string;
    blocker?: string;
    cancellation?: string;
    staleReason?: string;
    dependencies?: string[];
  };
}
const names: Record<string, string> = {
  todo: 'К выполнению',
  in_progress: 'В работе',
  in_review: 'На проверке',
  done: 'Готово',
  cancel_requested: 'Запрошена отмена',
  cancelled: 'Отменена',
};

// В истории берём лишь снимок из видимых сообщений. Live API никогда не
// вызывается из прошлого кадра, даже если карточка ещё не была записана тогда.
export function TeamTaskDetails({
  id,
  run,
  snapshot,
  historical,
  messages,
  onClose,
}: {
  id: string;
  run: string;
  snapshot?: TeamTask;
  historical: boolean;
  messages: TeamMessage[];
  onClose: () => void;
}) {
  const [task, setTask] = useState(snapshot);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(!historical);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    if (historical) return;
    const controller = new AbortController();
    setLoading(true);
    setError('');
    fetch(
      `/api/teams/${encodeURIComponent(run)}/tasks?taskId=${encodeURIComponent(id)}`,
      { signal: controller.signal },
    )
      .then(async (response) => {
        if (!response.ok) throw new Error(await response.text());
        return response.json() as Promise<{ task: TeamTask }>;
      })
      .then((page) => setTask(page.task))
      .catch((cause) => {
        if (!controller.signal.aborted) setError(String(cause));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [id, run, historical, attempt]);
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      title={task?.card?.title || `Задача ${id}`}
    >
      <div className="team-task-details">
        <ErrorNotice error={error} />
        {error && (
          <Button onClick={() => setAttempt((value) => value + 1)}>
            Повторить загрузку
          </Button>
        )}
        {loading && <Text color="secondary">Загрузка карточки…</Text>}
        <Text color="secondary">
          {id}
          {task?.card &&
            ` · Версия условий ${task.card.revision} · ${names[task.card.cancellation || task.card.status] || task.card.status}`}
        </Text>
        {task ? (
          <>
            {task.card?.blocker && <Text>Блокер: {task.card.blocker}</Text>}
            {task.card?.staleReason && (
              <ErrorNotice
                error={`Нужна повторная проверка: ${task.card.staleReason}`}
              />
            )}
            <TeamMarkdown text={task.text} />
            {task.card && (
              <>
                <Text variant="subheader-1">Ожидаемый результат</Text>
                <TeamMarkdown text={task.card.expected} />
                <Text variant="subheader-1">Критерии приёмки</Text>
                <ul>
                  {task.card.criteria?.map((item, index) => (
                    <li key={index}>{item}</li>
                  ))}
                </ul>
                {!!task.card.dependencies?.length && (
                  <Text>Зависимости: {task.card.dependencies.join(', ')}</Text>
                )}
              </>
            )}
          </>
        ) : (
          !loading && <Text>В этом кадре описание задачи ещё недоступно.</Text>
        )}
        <Text variant="subheader-1">Обсуждение</Text>
        {messages
          .filter((message) => message.taskId === id)
          .map((message) => (
            <Button
              key={message.id}
              view="flat"
              className="team-task-comment"
              onClick={() => {
                onClose();
                requestAnimationFrame(() => revealTeamMessage(message.id));
              }}
            >
              {message.text.slice(0, 180)}
              {message.text.length > 180 ? '…' : ''}
            </Button>
          ))}
      </div>
    </Dialog>
  );
}
