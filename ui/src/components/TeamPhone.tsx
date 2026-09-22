import { TeamTaskDetails, type TeamTask } from './TeamTaskDetails';
import {
  TeamConversation,
  revealTeamMessage,
  conversationBoundary,
} from './TeamConversation';
import { waitLabel, waitDetails } from './teamWait';
import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import {
  Avatar,
  ClipboardButton,
  Label,
  Button,
  Icon,
  Text,
} from '@gravity-ui/uikit';
import {
  Pin,
  Xmark,
  CircleCheckFill,
  ChevronsRight,
  ChevronDown,
} from '@gravity-ui/icons';
import { usePoll } from '../hooks/api';
import { ErrorNotice } from './ui';
import { type TeamPlayerState, type TeamHistory } from './TeamPlayer';
import { useEmployeeSprite } from './appearances';
import './team-phone.css';
import { TeamMarkdown } from './TeamMarkdown';
import { TeamMessageInput } from './TeamMessageInput';
import { usePhoneWindow } from './usePhoneWindow';
import { toaster } from '@gravity-ui/uikit/toaster-singleton';

export interface TeamMessage {
  taskId?: string;
  taskRevision?: number;
  taskSnapshot?: TeamTask;
  summary?: {
    requestId: string;
    through: number;
    text: string;
    sourceIds: string[];
  };
  id: string;
  authorId: string;
  date: string;
  text: string;
  to?: string;
  kind?: string;
  replyTo?: string;
}
export interface TeamWait {
  kind: string;
  source: 'runtime' | 'actor';
  since: string;
  actorId?: string;
  messageId?: string;
  text?: string;
}
export interface TeamActor {
  wait?: TeamWait;
  status: 'idle' | 'working' | 'monitoring' | 'blocked' | 'unknown';
  summary?: string;
  error?: string;
  nextCheck: string;
  delivery?: { attempted: boolean };
}
export interface TeamChat {
  compaction?: { pending?: { status: string; error?: string } };
  history?: TeamHistory;
  runId: string;
  goal: string;
  members: Record<string, { name: string; avatar?: string }>;
  messages: TeamMessage[];
  room?: {
    tasks?: Record<string, TeamTask>;
    actors: Record<string, TeamActor>;
    achievedAt?: string;
  };
}
interface Teams {
  teams: { id: string; goal: string }[];
  problems: string[];
}

// Совпадает со strings.Fields в Go для обычных пробельных разделителей.
// Сервер повторно проверяет ограничение: клиентский счётчик не является защитой.
export function wordCount(text: string) {
  return text.match(/[^\p{White_Space}]+/gu)?.length || 0;
}

// POST не повторяется автоматически. ID сообщения сохраняется при ошибке сети,
// поэтому явный повтор подтверждает прежнюю запись вместо создания дубля.
async function post<T>(url: string, input: unknown): Promise<T> {
  const response = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  });
  if (!response.ok) throw new Error((await response.text()).trim());
  return response.json() as Promise<T>;
}

// Один телефон показывает одну команду; выбранный run остаётся в адресе страницы.
// Новая цель автоматически становится первым поручением Боссу.
export function TeamPhone({
  onClose,
  onRunChange,
  player,
}: {
  onClose: () => void;
  onRunChange?: (run: string) => void;
  player?: TeamPlayerState;
}) {
  const phoneWindow = usePhoneWindow();
  const [run, setRun] = useState(
    () => new URLSearchParams(window.location.search).get('run') || '',
  );
  const { data: teams, error } = usePoll<Teams>('/api/teams');
  const historical = player?.historical && player.view?.runId === run;
  function selectRun(id: string) {
    setRun(id);
    onRunChange?.(id);
    const url = new URL(window.location.href);
    url.searchParams.set('run', id);
    window.history.replaceState(null, '', url);
  }
  return (
    <section
      ref={phoneWindow.panel}
      className="team-phone"
      role="dialog"
      aria-label="Чат команды"
      tabIndex={-1}
      style={phoneWindow.bounds}
      onKeyDown={(event) => {
        if (event.key === 'Escape' && !event.defaultPrevented) {
          event.stopPropagation();
          onClose();
        }
      }}
    >
      <div className="team-phone-screen">
        <header className="team-phone-header" {...phoneWindow.move}>
          <span
            className="team-phone-move-grip"
            role="button"
            tabIndex={0}
            aria-label="Переместить телефон"
            title="Перетащите окно или используйте стрелки"
            onKeyDown={(event) => phoneWindow.keyboard('move', event)}
          />
          <span className="team-phone-camera-island" aria-hidden="true">
            <span className="team-phone-camera-lens" />
          </span>
          <Text variant="subheader-2" className="team-phone-title">
            Чат команды
          </Text>
          {historical && (
            <>
              <Label theme="info" size="xs">
                История
              </Label>
              <Button
                view="flat"
                size="s"
                aria-label="К текущему чату"
                title="К текущему чату"
                onClick={player?.live}
              >
                <Icon data={ChevronsRight} size={16} />
              </Button>
            </>
          )}
          <Button
            view="flat"
            size="s"
            aria-label="Закрыть чат"
            onClick={onClose}
          >
            <Icon data={Xmark} size={16} />
          </Button>
        </header>
        <ErrorNotice error={error || teams?.problems.join('\n')} />
        {!run ? (
          <div className="team-new">
            <Text color="secondary">
              {teams?.teams.length
                ? 'Выберите существующий заказ'
                : 'Нет запущенных команд. Создайте заказ через CLI Lawa.'}
            </Text>
            {teams?.teams.map((team) => (
              <Button
                key={team.id}
                view="outlined"
                onClick={() => selectRun(team.id)}
              >
                {team.goal}
              </Button>
            ))}
          </div>
        ) : (
          <>
            <TeamThread
              key={run}
              run={run}
              historyView={
                player?.historical && player.view?.runId === run
                  ? player.view
                  : undefined
              }
              at={player?.view?.runId === run ? player.at : undefined}
              onLive={player?.live}
            />
          </>
        )}
      </div>
      <button
        className="team-phone-resize"
        type="button"
        aria-label="Изменить размер телефона"
        title="Потяните за угол или используйте стрелки"
        {...phoneWindow.resize}
        onKeyDown={(event) => phoneWindow.keyboard('resize', event)}
      >
        <svg viewBox="0 0 64 64" aria-hidden="true">
          <path d="M33.344 54.138A54 54 0 0 0 55.028 31.683" />
        </svg>
      </button>
    </section>
  );
}

// Карточка занимает две строки до раскрытия; полная цель доступна с клавиатуры.
// Собственное раскрытие не меняет курсор истории или состояние заказа.
function TeamGoal({ goal, achieved }: { goal: string; achieved: boolean }) {
  const [expanded, setExpanded] = useState(false);
  return (
    <section
      className={`team-pin ${achieved ? 'team-pin-achieved' : ''} ${expanded ? 'team-pin-expanded' : ''}`}
    >
      <Button
        view="flat"
        className="team-pin-toggle"
        aria-label={expanded ? 'Свернуть цель' : 'Развернуть цель'}
        aria-expanded={expanded}
        onClick={() => setExpanded(!expanded)}
      >
        <span className="team-pin-heading">
          <Icon data={achieved ? CircleCheckFill : Pin} size={16} />
          <Text
            as="span"
            variant="caption-2"
            color={achieved ? 'positive' : 'secondary'}
          >
            Цель команды
          </Text>
          <Icon className="team-pin-chevron" data={ChevronDown} size={14} />
        </span>
        <span className={expanded ? 'team-pin-details' : 'team-pin-preview'}>
          {goal}
        </span>
      </Button>
      <ClipboardButton
        className="team-pin-clipboard"
        view="flat"
        size="s"
        text={goal}
        aria-label="Скопировать цель"
        tooltipInitialText="Скопировать цель"
        tooltipSuccessText="Цель скопирована"
        onCopy={(_, copied) => {
          if (!copied)
            toaster.add({
              name: 'copy-team-goal',
              title: 'Не удалось скопировать цель',
              theme: 'danger',
              autoHiding: 2500,
            });
        }}
      />
    </section>
  );
}

// Выбранная внешность едина для сцены и чата. Если выбора нет, используем
// исходный образ из реестра команды либо стабильные инициалы автора.
function MemberAvatar({ id, chat }: { id: string; chat: TeamChat }) {
  const member = chat.members[id];
  const sprite = useEmployeeSprite(id, member?.avatar || '');
  const hash = Array.from(id).reduce(
    (value, letter) => (value * 31 + letter.codePointAt(0)!) >>> 0,
    0,
  );
  return (
    <Avatar
      size="s"
      aria-label={`Аватар: ${member?.name || id}`}
      text={member?.name || '?'}
      imgUrl={sprite}
      className={sprite ? 'team-employee-avatar' : undefined}
      theme="normal"
      style={{ backgroundColor: `hsl(${hash % 360} 25% 78%)` }}
    />
  );
}

// Polling получает новые реплики, локально подтверждённые записи защищены от
// запоздавшего GET. Порядок задаёт серверная запись, а не часы браузера.
// Скролл следует за новыми сообщениями только у нижнего края.
function TeamThread({
  run,
  historyView,
  at,
  onLive,
}: {
  run: string;
  historyView?: TeamChat;
  at?: number;
  onLive?: () => void;
}) {
  const url = `/api/teams/${encodeURIComponent(run)}`;
  const { data: liveChat, error: readError } = usePoll<TeamChat>(url);
  const chat = historyView || liveChat;
  const [text, setText] = useState('');
  const [reply, setReply] = useState<TeamMessage>();
  const [taskId, setTaskId] = useState('');
  const [confirmed, setConfirmed] = useState<TeamMessage[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const pending = useRef<{
    id: string;
    text: string;
    taskId?: string;
    replyTo?: string;
  } | null>(null);
  const list = useRef<HTMLDivElement>(null);
  const follows = useRef(true);
  const readingAnchor = useRef<{ id: string; top: number } | undefined>(
    undefined,
  );
  const messages = [
    ...new Map(
      [...(chat?.messages || []), ...(historyView ? [] : confirmed)].map(
        (message) => [message.id, message],
      ),
    ).values(),
  ];
  const boundary = conversationBoundary(messages);
  // После переноса сообщения в архив сохраняем его положение до отрисовки.
  useLayoutEffect(() => {
    const anchor = readingAnchor.current;
    if (!follows.current && anchor && list.current) {
      const element = document.getElementById(anchor.id);
      if (element)
        list.current.scrollTop +=
          element.getBoundingClientRect().top - anchor.top;
    }
  }, [boundary]);
  useEffect(() => {
    if (follows.current && list.current)
      list.current.scrollTop = list.current.scrollHeight;
  }, [messages.length, historyView]);
  async function send() {
    if (busy || !text.trim() || wordCount(text) > 50) return;
    setBusy(true);
    setError('');
    if (
      !pending.current ||
      pending.current.text !== text ||
      pending.current.replyTo !== reply?.id
    )
      pending.current = {
        id: crypto.randomUUID(),
        text,
        taskId: reply?.taskId,
        replyTo: reply?.id,
      };
    try {
      const message = await post<TeamMessage>(
        `${url}/messages`,
        pending.current,
      );
      setConfirmed((previous) => [
        ...previous.filter((item) => item.id !== message.id),
        message,
      ]);
      pending.current = null;
      follows.current = true;
      setText('');
      setReply(undefined);
      onLive?.();
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }
  const messagesById = new Map(
    messages.map((message) => [message.id, message]),
  );
  const taskSnapshots = Object.fromEntries(
    messages
      .filter((message) => message.taskSnapshot)
      .map((message) => [message.taskId, message.taskSnapshot!]),
  );
  const tasks = historyView
    ? taskSnapshots
    : { ...taskSnapshots, ...chat?.room?.tasks };
  function messageLinks(message: TeamMessage) {
    const source = message.replyTo
      ? messagesById.get(message.replyTo)
      : undefined;
    return (
      <div className="team-message-links">
        {message.taskId && (
          <Button
            view="flat"
            size="s"
            className="team-task-link"
            onClick={() => setTaskId(message.taskId!)}
          >
            Задача {message.taskId}
            {tasks[message.taskId]?.card?.title
              ? ` · ${tasks[message.taskId].card!.title}`
              : ''}
          </Button>
        )}
        {message.replyTo &&
          (source ? (
            <Button
              view="flat"
              size="s"
              className="team-reply-link"
              onClick={() => revealTeamMessage(source.id)}
            >
              Ответ на: {source.text.slice(0, 100)}
              {source.text.length > 100 ? '…' : ''}
            </Button>
          ) : (
            <Text color="secondary">
              Исходное сообщение недоступно в этом кадре
            </Text>
          ))}
        {!historyView && (
          <Button
            view="flat"
            size="s"
            disabled={busy}
            onClick={() => {
              setReply(message);
              if (chat?.room?.actors[message.authorId])
                setText(
                  (current) =>
                    `@${message.authorId} ${current.replace(/^@\S*\s*/, '')}`,
                );
              requestAnimationFrame(() =>
                document
                  .querySelector<HTMLTextAreaElement>(
                    '[aria-label="Сообщение команде"]',
                  )
                  ?.focus(),
              );
            }}
          >
            Ответить
          </Button>
        )}
      </div>
    );
  }
  return (
    <>
      <ErrorNotice error={readError} />
      {taskId && (
        <TeamTaskDetails
          key={`${taskId}-${historyView ? messages.length : 'live'}`}
          id={taskId}
          run={run}
          snapshot={tasks[taskId]}
          historical={Boolean(historyView)}
          messages={messages}
          onClose={() => setTaskId('')}
        />
      )}
      {!historyView && <ErrorNotice error={chat?.compaction?.pending?.error} />}
      {chat ? (
        <>
          <div
            className="team-messages"
            ref={list}
            role="log"
            aria-label="Сообщения команды"
            onScroll={() => {
              const el = list.current!;
              follows.current =
                el.scrollHeight - el.scrollTop - el.clientHeight < 48;
              const visible = [
                ...el.querySelectorAll<HTMLElement>('[id^="team-message-"]'),
              ].find(
                (node) =>
                  node.getBoundingClientRect().bottom >
                  el.getBoundingClientRect().top + 90,
              );
              if (visible)
                readingAnchor.current = {
                  id: visible.id,
                  top: visible.getBoundingClientRect().top,
                };
            }}
          >
            <div className="team-pin-layer">
              <TeamGoal
                key={chat.goal}
                goal={chat.goal}
                achieved={Boolean(chat.room?.achievedAt)}
              />
            </div>
            {!messages.length && (
              <Text className="team-empty" color="secondary">
                Здесь — самое важное для всей команды.
              </Text>
            )}
            <TeamConversation
              messages={messages}
              preserveReading={!follows.current}
              renderMessage={(message) =>
                message.authorId === 'system' ||
                message.kind === 'achievement' ||
                message.kind === 'goal_updated' ? (
                  <div
                    key={message.id}
                    id={`team-message-${message.id}`}
                    tabIndex={-1}
                    className="team-system-message"
                  >
                    <time dateTime={message.date}>
                      {new Date(message.date).toLocaleTimeString('ru-RU', {
                        hour: '2-digit',
                        minute: '2-digit',
                      })}
                    </time>
                    <span>
                      {message.kind === 'achievement' && (
                        <Icon
                          data={CircleCheckFill}
                          size={14}
                          className="team-achievement-icon"
                        />
                      )}{' '}
                      {message.text}
                    </span>
                    {messageLinks(message)}
                  </div>
                ) : (
                  <article
                    id={`team-message-${message.id}`}
                    key={message.id}
                    tabIndex={-1}
                    className={`team-message ${message.authorId === 'human' ? 'team-message-own' : ''}`}
                  >
                    <MemberAvatar id={message.authorId} chat={chat} />
                    <div className="team-message-body">
                      <div className="team-message-meta">
                        <Text variant="caption-2">
                          {chat.members[message.authorId]?.name ||
                            message.authorId}
                        </Text>
                        <time
                          dateTime={message.date}
                          title={new Date(message.date).toLocaleString('ru-RU')}
                        >
                          {new Date(message.date).toLocaleString('ru-RU', {
                            day: '2-digit',
                            month: '2-digit',
                            hour: '2-digit',
                            minute: '2-digit',
                          })}
                        </time>
                      </div>
                      <div className="team-message-bubble">
                        <TeamMarkdown text={message.text} to={message.to} />
                        {messageLinks(message)}
                      </div>
                    </div>
                  </article>
                )
              }
            />
            {chat.room &&
              Object.entries(chat.room.actors)
                .filter(([, actor]) => actor.wait)
                .map(([id, actor]) => (
                  <div key={id} className="team-wait">
                    <Text variant="body-1">
                      {chat.members[id]?.name || id}:{' '}
                      {waitLabel(actor.wait!, at ?? Date.now())}
                    </Text>
                    <p>{waitDetails(actor.wait!)}</p>
                    {actor.wait?.messageId && (
                      <Button
                        size="s"
                        view="flat"
                        onClick={() =>
                          revealTeamMessage(actor.wait!.messageId!)
                        }
                      >
                        К сообщению
                      </Button>
                    )}
                  </div>
                ))}
          </div>
          {!historyView &&
            chat.room &&
            Object.entries(chat.room.actors)
              .filter(([, actor]) => actor.error)
              .map(([id, actor]) => (
                <div key={id} className="team-actor-error">
                  <ErrorNotice
                    error={`${chat.members[id]?.name || id}: ${actor.error}`}
                  />
                  {actor.delivery && !actor.delivery.attempted && (
                    <Button
                      size="s"
                      disabled={busy}
                      onClick={async () => {
                        setBusy(true);
                        try {
                          await post(`${url}/actors/${id}/retry`, {});
                          setError('');
                        } catch (cause) {
                          setError(String(cause));
                        } finally {
                          setBusy(false);
                        }
                      }}
                    >
                      Повторить запуск
                    </Button>
                  )}
                </div>
              ))}
          <form
            className="team-compose"
            onSubmit={(event) => {
              event.preventDefault();
              void send();
            }}
          >
            <ErrorNotice error={error} />
            {reply && (
              <div className="team-compose-reply">
                <Text>Ответ на: {reply.text.slice(0, 100)}</Text>
                <Button
                  view="flat"
                  size="s"
                  aria-label="Сбросить ответ"
                  disabled={busy}
                  onClick={() => setReply(undefined)}
                >
                  <Icon data={Xmark} size={14} />
                </Button>
              </div>
            )}
            {(liveChat || chat).room && (
              <TeamRecipients
                actors={(liveChat || chat).room!.actors}
                achieved={Boolean((liveChat || chat).room?.achievedAt)}
                busy={busy}
                onSelect={(id) =>
                  setText(`@${id} ${text.replace(/^@[a-z][a-z0-9_-]*\s*/, '')}`)
                }
              />
            )}
            <TeamMessageInput
              value={text}
              onUpdate={setText}
              employees={Object.keys((liveChat || chat).room?.actors || {}).map(
                (id) => ({
                  id,
                  name: (liveChat || chat).members[id]?.name || id,
                }),
              )}
              renderAvatar={(id) => (
                <MemberAvatar id={id} chat={liveChat || chat} />
              )}
              busy={busy}
              canSend={Boolean(text.trim()) && wordCount(text) <= 50}
              placeholder={chat.room ? '@boss Самое важное…' : 'Самое важное…'}
            />
            <div className="team-compose-meta">
              <Text
                className="team-compose-count"
                variant="caption-2"
                color={wordCount(text) > 50 ? 'danger' : 'secondary'}
              >
                Чел · {wordCount(text)}/50 слов
              </Text>
              {(liveChat || chat).room && (
                <Text
                  className="team-compose-hint"
                  variant="caption-1"
                  color="secondary"
                >
                  Агенты отвечают только на явный @тег
                </Text>
              )}
            </div>
          </form>
        </>
      ) : (
        !readError && <p className="team-empty">Загрузка чата…</p>
      )}
    </>
  );
}

// Личный таймер виден без активной модели: ожидание пяти минут не выглядит
// зависшим интерфейсом. Сервер остаётся источником времени следующей проверки.
function TeamRecipients({
  actors,
  achieved,
  busy,
  onSelect,
}: {
  actors: Record<string, TeamActor>;
  achieved?: boolean;
  busy: boolean;
  onSelect: (id: string) => void;
}) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  return (
    <div className="team-recipients" aria-label="Адресовать сотруднику">
      {Object.entries(actors).map(([id, actor]) => {
        const seconds = Math.max(
          0,
          Math.ceil((Date.parse(actor.nextCheck) - now) / 1000),
        );
        const state = achieved ? 'idle' : actor.status;
        const status = {
          idle: 'Ждёт обращения',
          working: 'Работает',
          monitoring: 'Мониторит',
          blocked: 'Нужна помощь',
          unknown: 'Статус не записан',
        }[state];
        // nextCheck занятого/остановленного сотрудника не является отсчётом.
        // После достижения таймер скрыт даже при устаревшем серверном времени.
        const countdown =
          !achieved &&
          (state === 'idle' || state === 'monitoring') &&
          seconds > 0
            ? `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
            : undefined;
        const description = countdown
          ? `${status}. Проверка через ${countdown}`
          : status;
        return (
          <div key={id} className="team-recipient">
            <Button
              size="m"
              view="outlined"
              disabled={busy}
              title={description}
              aria-description={description}
              onClick={() => onSelect(id)}
            >
              <span className="team-recipient-content">
                <span
                  className={`team-recipient-dot team-recipient-dot_${state}`}
                  aria-hidden="true"
                />
                @{id}
              </span>
            </Button>
            {countdown && (
              <Label
                className="team-recipient-timer"
                size="xs"
                theme="normal"
                aria-hidden="true"
              >
                {countdown}
              </Label>
            )}
          </div>
        );
      })}
    </div>
  );
}
