import { useState, type CSSProperties } from 'react';
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
import {
  AppearanceProvider,
  useEmployeeSprite,
} from '../components/appearances';
import { AppearancePicker } from '../components/AppearancePicker';
import { CharacterGallery } from '../components/CharacterGallery';
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
  const actors = chat?.room?.actors || { boss: {} };
  const actorIds = [
    ...new Set(['boss', ...invited, ...Object.keys(actors)]),
  ].filter((id) => Object.hasOwn(actors, id));
  return (
    <AppearanceProvider scope={run}>
      <main
        className={`office ${chat?.room ? 'office-with-player' : ''}`}
        aria-label="Офис агентов"
      >
        <div className="office-toolbar">
          <CharacterGallery />
          <AppearancePicker
            members={Object.fromEntries(
              Object.keys(currentChat?.room?.actors || { boss: {} }).map(
                (id) => [
                  id,
                  {
                    name:
                      currentChat?.members[id]?.name ||
                      (id === 'boss'
                        ? 'Босс'
                        : id === 'developer'
                          ? 'Разработчик'
                          : id),
                  },
                ],
              ),
            )}
          />
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
            className={`office-scene ${actorIds.length > 2 ? 'office-scene-many' : ''}`}
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
                placement={employeePlacement(index, actorIds.length)}
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
    </AppearanceProvider>
  );
}

// Облачко показывает короткое публичное действие, а не внутренние рассуждения.
// Текст статуса доступен скринридеру: цвет точки не единственный сигнал.
function Employee({
  id,
  name,
  avatar,
  placement,
  actor,
  achieved = false,
  onClick,
}: {
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
      <img src={sprite} width="1254" height="1254" alt={`${name} за MacBook`} />
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
          view="raised"
          size="s"
          aria-label={`${name}: ${label}`}
          title={`${label}. Открыть общий чат`}
          onClick={onClick}
        >
          <span className="office-nameplate-content">
            <span
              className={`office-status-dot office-status-dot_${status === 'blocked' || status === 'unknown' ? 'idle' : status}`}
              aria-hidden="true"
            />
            {name}
          </span>
        </Button>
      </div>
    </div>
  );
}

// До двух сотрудников сохраняем привычную крупную композицию. Большие команды
// располагаются на изометрической сетке свободного пола: порядок приглашений
// стабилен внутри снимка, z-index следует глубине, мебель задней стены свободна.
export function employeePlacement(index: number, count: number): CSSProperties {
  if (count === 1) return { left: '50%', top: '46%', width: '16%' };
  if (count === 2)
    return {
      left: index === 0 ? '38%' : '65%',
      top: index === 0 ? '44%' : '53%',
      width: '16%',
    };
  const columns = count <= 6 ? 3 : count <= 12 ? 4 : 5;
  const rows = Math.ceil(count / columns);
  const u = (index % columns) / Math.max(1, columns - 1);
  const v = Math.floor(index / columns) / Math.max(1, rows - 1);
  return {
    left: `${48 + 32 * u - 30 * v}%`,
    top: `${33 + 20 * u + 22 * v}%`,
    width: count <= 6 ? '13%' : count <= 12 ? '10%' : '8%',
    zIndex: Math.round((u + v) * 100),
  };
}
