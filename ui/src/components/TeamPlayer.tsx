import { useEffect, useState } from 'react';
import { Button, Icon, Slider, Text } from '@gravity-ui/uikit';
import {
  ChevronLeft,
  ChevronRight,
  ChevronsRight,
  PlayFill,
  PauseFill,
} from '@gravity-ui/icons';
import { Choice, ErrorNotice } from './ui';
import { type TeamActor, type TeamChat } from './TeamPhone';
import './team-player.css';

export interface TeamFrame {
  achievedAt?: string;
  at: string;
  messageCount: number;
  actors: Record<string, TeamActor>;
}
export interface TeamHistory {
  recordedFrom: string;
  recovered: boolean;
  frames: TeamFrame[];
}

// Старые заказы до восстановления имеют только достоверную переписку. Не
// переносим сегодняшний статус в прошлое: отсутствие данных видно как unknown.
export function historyFrames(chat: TeamChat): TeamFrame[] {
  if (chat.history?.frames.length) return chat.history.frames;
  let actors: Record<string, TeamActor> = {
    boss: { status: 'unknown', nextCheck: '' },
  };
  let previous = 0;
  return chat.messages.map((message, index) => {
    previous = Math.max(previous, Date.parse(message.date));
    actors = { ...actors };
    if (message.kind === 'system' && message.id === 'summon-developer')
      actors.developer = { status: 'unknown', nextCheck: '' };
    if (actors[message.authorId])
      actors[message.authorId] = {
        ...actors[message.authorId],
        summary: message.text
          .replace(/^@\S+\s+/, '')
          .split(/\s+/)
          .slice(0, 7)
          .join(' '),
      };
    return {
      at: new Date(previous).toISOString(),
      messageCount: index + 1,
      actors,
    };
  });
}

// Выбираем последний опубликованный кадр не позже курсора, включая несколько
// событий одной миллисекунды. Переписка и состав комнаты берутся из одного кадра.
export function teamAt(
  chat: TeamChat,
  frames: TeamFrame[],
  at: number,
): TeamChat {
  let low = 0,
    high = frames.length;
  while (low < high) {
    const mid = (low + high) >>> 1;
    if (Date.parse(frames[mid].at) <= at) low = mid + 1;
    else high = mid;
  }
  const frame = frames[low - 1];
  return {
    ...chat,
    messages: frame ? chat.messages.slice(0, frame.messageCount) : [],
    room: { actors: frame?.actors || {}, achievedAt: frame?.achievedAt },
  };
}

type Selection = { run: string; at: number; end: number; playing: boolean };

// Воспроизведение полностью локально: не продолжает turn и не меняет очередь.
// Диапазон фиксируется при входе в прошлое, поэтому polling не двигает курсор.
export function useTeamPlayer(chat?: TeamChat) {
  const [selection, setSelection] = useState<Selection>();
  const [speed, setSpeed] = useState(20);
  const frames = chat ? historyFrames(chat) : [];
  const current = selection?.run === chat?.runId ? selection : undefined;
  const start = frames.length ? Date.parse(frames[0].at) : Date.now();
  const end =
    current?.end ??
    Math.max(
      start,
      chat?.room?.achievedAt ? Date.parse(chat.room.achievedAt) : Date.now(),
      Date.parse(frames.at(-1)?.at || '') || 0,
    );
  const at = current?.at ?? end;
  const historical = Boolean(current);
  useEffect(() => {
    setSelection(undefined);
  }, [chat?.runId]);
  useEffect(() => {
    if (!current?.playing) return;
    let previous = performance.now();
    const timer = setInterval(() => {
      const now = performance.now(),
        delta = (now - previous) * speed;
      previous = now;
      setSelection((old) => {
        if (!old || old.run !== chat?.runId || !old.playing) return old;
        const next = Math.min(old.end, old.at + delta);
        return { ...old, at: next, playing: next < old.end };
      });
    }, 100);
    return () => clearInterval(timer);
  }, [current?.playing, chat?.runId, speed]);
  const seek = (value: number) =>
    chat &&
    setSelection({
      run: chat.runId,
      at: Math.max(start, Math.min(end, value)),
      end,
      playing: false,
    });
  const toggle = () => {
    if (!chat) return;
    if (current?.playing) {
      setSelection({ ...current, playing: false });
      return;
    }
    setSelection({
      run: chat.runId,
      at: !current || at >= end ? start : at,
      end,
      playing: true,
    });
  };
  const step = (direction: -1 | 1) => {
    const times = frames.map((frame) => Date.parse(frame.at));
    const target =
      direction < 0
        ? (times.filter((time) => time < at).at(-1) ?? start)
        : (times.find((time) => time > at) ?? end);
    seek(target);
  };
  return {
    chat,
    frames,
    start,
    end,
    at,
    historical,
    playing: Boolean(current?.playing),
    speed,
    setSpeed,
    seek,
    toggle,
    step,
    live: () => setSelection(undefined),
    view: chat && current ? teamAt(chat, frames, at) : chat,
  };
}
export type TeamPlayerState = ReturnType<typeof useTeamPlayer>;
const timeLabel = (value: number) =>
  new Date(value).toLocaleTimeString('ru-RU');

// Плеер живёт только под сценой. Телефон отображает выбранный кадр без
// второго набора контролов; доступная подпись сохраняет смысл кнопок-иконок.
export function TeamPlayer({ player }: { player: TeamPlayerState }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const chat = player.chat;
  if (!chat?.room) return null;
  const needsRecovery =
    !chat.history?.recovered &&
    (!chat.history ||
      Date.parse(chat.messages[0]?.date || '') <
        Date.parse(chat.history.recordedFrom) - 1000);
  async function recover() {
    setBusy(true);
    setError('');
    try {
      const result = await fetch(
        `/api/teams/${encodeURIComponent(chat!.runId)}/history/recover`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: '{}',
        },
      );
      if (!result.ok) throw new Error(await result.text());
    } catch (cause) {
      setError(String(cause));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="team-player" aria-label="Плеер команды">
      <div className="team-player-controls">
        <Button
          view="outlined"
          size="s"
          aria-label="Предыдущее событие"
          onClick={() => player.step(-1)}
          disabled={!player.frames.length}
        >
          <Icon data={ChevronLeft} size={16} />
        </Button>
        <Button
          view="outlined"
          size="s"
          aria-label={player.playing ? 'Пауза' : 'Воспроизвести историю'}
          onClick={player.toggle}
          disabled={!player.frames.length}
        >
          <Icon data={player.playing ? PauseFill : PlayFill} size={16} />
        </Button>
        <Button
          view="outlined"
          size="s"
          aria-label="Следующее событие"
          onClick={() => player.step(1)}
          disabled={!player.frames.length}
        >
          <Icon data={ChevronRight} size={16} />
        </Button>
        <Text variant="caption-2" className="team-player-time">
          {player.historical ? timeLabel(player.at) : 'Сейчас'}
        </Text>
        <Choice
          aria-label="Скорость воспроизведения"
          value={String(player.speed)}
          onUpdate={(value) => player.setSpeed(Number(value))}
          options={[1, 5, 20, 60].map((value) => ({
            value: String(value),
            content: `${value}×`,
          }))}
        />
        <Button
          size="s"
          view="outlined"
          aria-label="К текущему"
          title="К текущему"
          onClick={player.live}
        >
          <Icon data={ChevronsRight} size={16} />
        </Button>
      </div>
      <Slider<number>
        aria-label="Момент истории"
        marks={0}
        min={0}
        max={Math.max(1, player.end - player.start)}
        step={1}
        value={Math.max(0, player.at - player.start)}
        onUpdate={(value) => player.seek(player.start + value)}
        tooltipDisplay="off"
        disabled={!player.frames.length}
      />
      <div className="team-player-range">
        <Text variant="caption-1" color="secondary">
          {timeLabel(player.start)}
        </Text>
        <Text variant="caption-1" color="secondary">
          {timeLabel(player.end)}
        </Text>
      </div>
      {needsRecovery && (
        <div className="team-player-recovery">
          <Text variant="caption-2" color="secondary">
            Ранние статусы ещё не восстановлены.
          </Text>
          <Button
            size="s"
            view="flat-info"
            loading={busy}
            onClick={() => void recover()}
          >
            Восстановить из Codex
          </Button>
        </div>
      )}
      <ErrorNotice error={error} />
    </section>
  );
}
