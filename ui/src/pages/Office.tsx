import { useMemo, useState, type CSSProperties } from 'react';
import { Button, Icon, Text } from '@gravity-ui/uikit';
import { Smartphone, CircleCheckFill } from '@gravity-ui/icons';
import {
  TeamPhone,
  type TeamChat,
  type TeamActor,
} from '../components/TeamPhone';
import { ThemePicker } from '../components/Theme';
import { ErrorNotice } from '../components/ui';
import { usePoll } from '../hooks/api';
import { TeamPlayer, useTeamPlayer } from '../components/TeamPlayer';
import room from '../assets/office/room-large-selected.png';
import { useEmployeeSprite } from '../components/appearances';
import { CharacterGallery } from '../components/CharacterGallery';
import { officeLayout, officeSpriteFrame } from './officeLayout';
import './office.css';

const states = {
  idle: 'Ждёт',
  working: 'Работает',
  monitoring: 'Мониторит',
  blocked: 'Нужна помощь',
  unknown: 'Статус не записан',
};

// Сцена читает тот же заказ, что телефон. Стол появляется только после durable
// приглашения Босса; нажатие label открывает общий чат, не симулирует работу.
export default function Office() {
  const [run, setRun] = useState(
    () => new URLSearchParams(window.location.search).get('run') || '',
  );
  const [phoneOpen, setPhoneOpen] = useState(false);
  const { data: currentChat, error } = usePoll<TeamChat>(
    run ? `/api/teams/${encodeURIComponent(run)}` : null,
  );
  const player = useTeamPlayer(currentChat);
  const chat = player.view;
  // Порядок событий стабилен при появлении новых сотрудников и перемотке.
  const invited = (chat?.messages || [])
    .filter(
      (message) =>
        message.kind === 'system' && message.id.startsWith('summon-'),
    )
    .map((message) => message.id.slice('summon-'.length));
  const actors = chat?.room?.actors || {};
  const actorIds = [
    ...new Set(['boss', ...invited, ...Object.keys(actors)]),
  ].filter((id) => Object.hasOwn(actors, id));
  const seats = useMemo(() => officeLayout(actorIds.length), [actorIds.length]);
  return (
    <main
      className={`office ${chat?.room ? 'office-with-player' : ''}`}
      aria-label="Офис агентов"
    >
      <div className="office-toolbar">
        <CharacterGallery />
        <Button
          view="flat"
          onClick={() => setPhoneOpen(true)}
          aria-label="Открыть чат команды"
          title="Чат команды"
        >
          <Icon data={Smartphone} />
        </Button>
        <ThemePicker />
      </div>
      <ErrorNotice error={error} />
      <div className="office-space">
        <div
          style={
            {
              '--office-sprite-width': officeSpriteFrame.width,
              '--office-sprite-left': officeSpriteFrame.left,
            } as CSSProperties
          }
          className={`office-scene ${actorIds.length > 12 ? 'office-scene-dense' : ''}`}
        >
          <img
            className="office-room-image"
            src={room}
            width="1536"
            height="1024"
            alt="Просторный изометрический офис с зоной отдыха, стеллажами и кофейным уголком"
          />
          {actorIds.map((id, index) => (
            <Employee
              key={id}
              compact={actorIds.length > 12}
              id={id}
              name={
                chat?.members[id]?.name ||
                (id === 'boss'
                  ? 'Босс'
                  : id === 'developer'
                    ? 'Разработчик'
                    : id)
              }
              avatar={chat?.members[id]?.avatar}
              actor={chat?.room?.actors[id]}
              placement={{
                left: `${seats[index].left}%`,
                top: `${seats[index].top}%`,
                width: `${seats[index].width}%`,
                zIndex: Math.round(seats[index].top * 100),
              }}
              achieved={id === 'boss' && Boolean(chat?.room?.achievedAt)}
              onClick={() => setPhoneOpen(true)}
            />
          ))}
        </div>
      </div>
      <TeamPlayer player={player} />
      {phoneOpen && (
        <TeamPhone
          onClose={() => setPhoneOpen(false)}
          onRunChange={setRun}
          player={player}
        />
      )}
    </main>
  );
}

// Облачко показывает короткое публичное действие, а не внутренние рассуждения.
// Текст статуса доступен скринридеру: цвет точки не единственный сигнал.
function Employee({
  compact,
  id,
  name,
  avatar,
  placement,
  actor,
  achieved = false,
  onClick,
}: {
  compact: boolean;
  id: string;
  name: string;
  avatar?: string;
  placement: CSSProperties;
  actor?: TeamActor;
  achieved?: boolean;
  onClick: () => void;
}) {
  const sprite = useEmployeeSprite(id, avatar || id);
  const status = actor?.status || 'idle';
  const label = states[status] || states.idle;
  const message =
    status === 'working' || status === 'blocked' || status === 'unknown'
      ? actor?.summary
      : '';
  return (
    <div className={`office-employee office-${id}`} style={placement}>
      <div className="office-sprite">
        <img
          src={sprite}
          width="1254"
          height="1254"
          alt={`${name} за MacBook`}
        />
      </div>
      <div className="office-speech" role="status" aria-atomic="true">
        {achieved ? (
          <Text
            className="office-speech-bubble office-speech-achieved"
            variant="body-1"
          >
            <Icon data={CircleCheckFill} size={16} /> Цель достигнута
          </Text>
        ) : (
          message && (
            <Text className="office-speech-bubble" variant="body-1">
              {message}
            </Text>
          )
        )}
      </div>
      <div className="office-nameplate">
        <Button
          view={compact ? 'flat' : 'raised'}
          size="s"
          aria-label={`${name}: ${label}`}
          title={`${name}: ${label}. Открыть общий чат`}
          onClick={onClick}
        >
          <span className="office-nameplate-content">
            <span
              className={`office-status-dot office-status-dot_${status === 'blocked' || status === 'unknown' ? 'idle' : status}`}
              aria-hidden="true"
            />
            <span className="office-nameplate-text">{name}</span>
          </span>
        </Button>
      </div>
    </div>
  );
}
