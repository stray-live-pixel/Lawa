import { useEffect, useRef, useState } from 'react';
import {
  Avatar,
  Button,
  Icon,
  Modal,
  Text,
  TextArea,
  TextInput,
} from '@gravity-ui/uikit';
import { ArrowUp, Pin, Xmark } from '@gravity-ui/icons';
import { usePoll } from '../hooks/api';
import { Choice, ErrorNotice } from './ui';
import bossImage from '../assets/office/boss.png';
import developerImage from '../assets/office/developer.png';
import './team-phone.css';

export interface TeamMessage {
  id: string;
  authorId: string;
  date: string;
  text: string;
  to?: string;
  kind?: string;
  replyTo?: string;
}
export interface TeamActor {
  status: 'idle' | 'working' | 'monitoring' | 'blocked';
  summary?: string;
  error?: string;
  nextCheck: string;
  delivery?: { attempted: boolean };
}
export interface TeamChat {
  runId: string;
  goal: string;
  members: Record<string, { name: string; avatar?: string }>;
  messages: TeamMessage[];
  room?: { actors: Record<string, TeamActor> };
}
interface Teams {
  teams: { id: string; goal: string }[];
  cwd: string;
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
}: {
  onClose: () => void;
  onRunChange?: (run: string) => void;
}) {
  const [run, setRun] = useState(
    () => new URLSearchParams(window.location.search).get('run') || '',
  );
  const { data: teams, error } = usePoll<Teams>('/api/teams');
  const [creating, setCreating] = useState(!run);
  function selectRun(id: string) {
    setRun(id);
    onRunChange?.(id);
    setCreating(false);
    const url = new URL(window.location.href);
    url.searchParams.set('run', id);
    window.history.replaceState(null, '', url);
  }
  return (
    <Modal
      open
      onClose={onClose}
      aria-label="Чат команды"
      contentClassName="team-phone-modal"
    >
      <section className="team-phone" aria-label="Смартфон команды">
        <div className="team-phone-hardware" aria-hidden="true">
          <span />
        </div>
        <header className="team-phone-header">
          <div>
            <Text variant="subheader-2">Команда</Text>
            <Text as="div" variant="caption-2" color="secondary">
              Общая база знаний
            </Text>
          </div>
          <Button view="flat" aria-label="Закрыть чат" onClick={onClose}>
            <Icon data={Xmark} />
          </Button>
        </header>
        <div className="team-phone-picker">
          <Choice
            aria-label="Заказ команды"
            value={creating ? 'new' : run}
            onUpdate={(id) =>
              id === 'new' ? setCreating(true) : selectRun(id)
            }
            options={[
              { value: 'new', content: 'Новая цель' },
              ...(teams?.teams || []).map((team) => ({
                value: team.id,
                content: team.goal.slice(0, 70),
              })),
              ...(!creating &&
              run &&
              !teams?.teams.some((team) => team.id === run)
                ? [{ value: run, content: 'Текущий заказ' }]
                : []),
            ]}
          />
        </div>
        <ErrorNotice error={error || teams?.problems.join('\n')} />
        {creating ? (
          <NewTeam cwd={teams?.cwd || ''} onCreated={selectRun} />
        ) : (
          <TeamThread key={run} run={run} />
        )}
        <div className="team-phone-home" aria-hidden="true">
          <span />
        </div>
      </section>
    </Modal>
  );
}

// Цель закрепляется целиком; лимит сообщения к постановке не применяется.
function NewTeam({
  cwd,
  onCreated,
}: {
  cwd: string;
  onCreated: (id: string) => void;
}) {
  const [goal, setGoal] = useState('');
  const [directory, setDirectory] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  async function create() {
    setBusy(true);
    setError('');
    try {
      const result = await post<{ runId: string }>('/api/teams', {
        goal,
        cwd: directory ?? cwd,
      });
      onCreated(result.runId);
    } catch (cause) {
      setError(
        `${String(cause)}. Перед повтором проверьте список заказов: сохранение могло завершиться.`,
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <form
      className="team-new"
      onSubmit={(event) => {
        event.preventDefault();
        void create();
      }}
    >
      <Text variant="header-1">С чего начнём?</Text>
      <Text color="secondary">
        Закрепите цель, ради которой собирается команда.
      </Text>
      <TextArea
        controlProps={{ 'aria-label': 'Цель команды' }}
        placeholder="Какого результата хотим достичь?"
        value={goal}
        onUpdate={setGoal}
        minRows={5}
        disabled={busy}
      />
      <TextInput
        label="Папка проекта"
        aria-label="Папка проекта"
        value={directory ?? cwd}
        onUpdate={setDirectory}
        disabled={busy}
      />
      <ErrorNotice error={error} />
      <Button
        type="submit"
        view="action"
        size="l"
        loading={busy}
        disabled={!goal.trim() || !(directory ?? cwd).trim()}
      >
        Закрепить цель
      </Button>
      <Text variant="caption-2" color="secondary">
        Босс сразу начнёт работу в выбранной папке.
      </Text>
    </form>
  );
}

// ID автора разрешается через реестр команды. Образ Босса берётся из сцены,
// Разработчик использует свой образ. Прочие авторы получают стабильные инициалы.
function MemberAvatar({ id, chat }: { id: string; chat: TeamChat }) {
  const member = chat.members[id];
  const sprite =
    member?.avatar === 'boss'
      ? bossImage
      : member?.avatar === 'developer'
        ? developerImage
        : undefined;
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
      className={sprite ? 'team-boss-avatar' : undefined}
      theme="normal"
      style={{ backgroundColor: `hsl(${hash % 360} 25% 78%)` }}
    />
  );
}

// Polling получает новые реплики, локально подтверждённые записи защищены от
// запоздавшего GET. Порядок задаёт серверная запись, а не часы браузера.
// Скролл следует за новыми сообщениями только у нижнего края.
function TeamThread({ run }: { run: string }) {
  const url = `/api/teams/${encodeURIComponent(run)}`;
  const { data: chat, error: readError } = usePoll<TeamChat>(url);
  const [text, setText] = useState('');
  const [confirmed, setConfirmed] = useState<TeamMessage[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const pending = useRef<{ id: string; text: string } | null>(null);
  const list = useRef<HTMLDivElement>(null);
  const follows = useRef(true);
  const messages = [
    ...new Map(
      [...(chat?.messages || []), ...confirmed].map((message) => [
        message.id,
        message,
      ]),
    ).values(),
  ];
  useEffect(() => {
    if (follows.current && list.current)
      list.current.scrollTop = list.current.scrollHeight;
  }, [messages.length]);
  async function send() {
    if (busy || !text.trim() || wordCount(text) > 50) return;
    setBusy(true);
    setError('');
    if (!pending.current || pending.current.text !== text)
      pending.current = { id: crypto.randomUUID(), text };
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
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <ErrorNotice error={readError} />
      {chat ? (
        <>
          <div className="team-pin" tabIndex={0}>
            <Icon data={Pin} size={16} />
            <div>
              <Text variant="caption-2" color="secondary">
                Цель команды · закреплено
              </Text>
              <p>{chat.goal}</p>
            </div>
          </div>
          <div
            className="team-messages"
            ref={list}
            role="log"
            aria-label="Сообщения команды"
            onScroll={() => {
              const el = list.current!;
              follows.current =
                el.scrollHeight - el.scrollTop - el.clientHeight < 48;
            }}
          >
            {!messages.length && (
              <Text className="team-empty" color="secondary">
                Здесь — самое важное для всей команды.
              </Text>
            )}
            {messages.map((message) =>
              message.kind === 'system' ? (
                <div key={message.id} className="team-system-message">
                  <time dateTime={message.date}>
                    {new Date(message.date).toLocaleTimeString('ru-RU', {
                      hour: '2-digit',
                      minute: '2-digit',
                    })}
                  </time>
                  <span>{message.text}</span>
                </div>
              ) : (
                <article
                  key={message.id}
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
                      {message.to &&
                      message.text.startsWith(`@${message.to} `) ? (
                        <>
                          <strong className="team-mention">
                            @{message.to}
                          </strong>
                          {message.text.slice(message.to.length + 1)}
                        </>
                      ) : (
                        message.text
                      )}
                    </div>
                  </div>
                </article>
              ),
            )}
          </div>
          {chat.room &&
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
            {chat.room && (
              <TeamRecipients
                actors={chat.room.actors}
                busy={busy}
                onSelect={(id) =>
                  setText(
                    `@${id} ${text.replace(/^@(boss|developer|human)\s*/, '')}`,
                  )
                }
              />
            )}
            <div className="team-compose-row">
              <TextArea
                controlProps={{ 'aria-label': 'Сообщение команде' }}
                placeholder={
                  chat.room ? '@boss Самое важное…' : 'Самое важное…'
                }
                value={text}
                onUpdate={setText}
                minRows={2}
                maxRows={4}
                disabled={busy}
              />
              <Button
                type="submit"
                view="action"
                size="l"
                aria-label="Отправить сообщение"
                loading={busy}
                disabled={!text.trim() || wordCount(text) > 50}
              >
                <Icon data={ArrowUp} />
              </Button>
            </div>
            <Text
              variant="caption-2"
              color={wordCount(text) > 50 ? 'danger' : 'secondary'}
            >
              Чел · {wordCount(text)}/50 слов
            </Text>
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
  busy,
  onSelect,
}: {
  actors: Record<string, TeamActor>;
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
        const status =
          actor.status === 'working'
            ? 'Работает'
            : actor.status === 'blocked'
              ? 'Нужна помощь'
              : seconds > 0
                ? `Проверка через ${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
                : 'Ожидает проверки';
        return (
          <div key={id} className="team-recipient">
            <Button
              size="s"
              view="flat-secondary"
              disabled={busy}
              onClick={() => onSelect(id)}
            >
              @{id}
            </Button>
            <Text variant="caption-1" color="secondary">
              {status}
            </Text>
          </div>
        );
      })}
      <Text variant="caption-1" color="secondary">
        Агенты отвечают только на явный @тег
      </Text>
    </div>
  );
}
