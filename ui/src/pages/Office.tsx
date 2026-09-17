import { useState } from 'react';
import { Button, Icon, Text } from '@gravity-ui/uikit';
import { Smartphone } from '@gravity-ui/icons';
import {
  TeamPhone,
  type TeamChat,
  type TeamActor,
} from '../components/TeamPhone';
import { ThemePicker } from '../components/Theme';
import { ErrorNotice } from '../components/ui';
import { usePoll } from '../hooks/api';
import room from '../assets/office/room-transparent.png';
import boss from '../assets/office/boss.png';
import developer from '../assets/office/developer.png';
import './office.css';

const states = {
  idle: 'Ждёт',
  working: 'Работает',
  monitoring: 'Мониторит',
  blocked: 'Нужна помощь',
};

// Сцена читает тот же заказ, что телефон. Стол появляется только после durable
// приглашения Босса; нажатие label открывает общий чат, не симулирует работу.
export default function Office() {
  const [run, setRun] = useState(
    () => new URLSearchParams(window.location.search).get('run') || '',
  );
  const [phoneOpen, setPhoneOpen] = useState(false);
  const { data: chat, error } = usePoll<TeamChat>(
    run ? `/api/teams/${encodeURIComponent(run)}` : null,
  );
  const hasDeveloper = Boolean(chat?.room?.actors.developer);
  return (
    <main className="office" aria-label="Офис агентов">
      <div className="office-toolbar">
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
          className={`office-scene ${hasDeveloper ? 'office-scene-team' : ''}`}
        >
          <img
            className="office-room-image"
            src={room}
            width="1536"
            height="1024"
            alt="Изометрическая комната с диваном, окном и растениями"
          />
          <Employee
            id="boss"
            name="Босс"
            sprite={boss}
            actor={chat?.room?.actors.boss}
            onClick={() => setPhoneOpen(true)}
          />
          {hasDeveloper && (
            <Employee
              id="developer"
              name="Разработчик"
              sprite={developer}
              actor={chat?.room?.actors.developer}
              onClick={() => setPhoneOpen(true)}
            />
          )}
        </div>
      </div>
      {phoneOpen && (
        <TeamPhone onClose={() => setPhoneOpen(false)} onRunChange={setRun} />
      )}
    </main>
  );
}

// Облачко показывает короткое публичное действие, а не внутренние рассуждения.
// Текст статуса доступен скринридеру: цвет точки не единственный сигнал.
function Employee({
  id,
  name,
  sprite,
  actor,
  onClick,
}: {
  id: string;
  name: string;
  sprite: string;
  actor?: TeamActor;
  onClick: () => void;
}) {
  const status = actor?.status || 'idle';
  const label = states[status] || states.idle;
  const message =
    status === 'working' || status === 'blocked' ? actor?.summary : '';
  return (
    <div className={`office-employee office-${id}`}>
      <img src={sprite} width="1254" height="1254" alt={`${name} за MacBook`} />
      <div className="office-speech" role="status" aria-atomic="true">
        {message && (
          <Text className="office-speech-bubble" variant="body-1">
            {message}
          </Text>
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
              className={`office-status-dot office-status-dot_${status === 'blocked' ? 'idle' : status}`}
              aria-hidden="true"
            />
            {name}
          </span>
        </Button>
      </div>
    </div>
  );
}
