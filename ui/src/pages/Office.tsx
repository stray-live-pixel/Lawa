import { useState } from 'react';
import { Button, Text } from '@gravity-ui/uikit';
import { ThemePicker } from '../components/Theme';
import room from '../assets/office/room-transparent.png';
import boss from '../assets/office/boss.png';
import './office.css';

// Демонстрационные реплики описывают внешнее действие, а не внутренние мысли.
// Нажатие на label меняет только локальную сцену; реальный агент не запускается.
const demoStates = [
  { status: 'idle', label: 'Ждёт', message: '' },
  { status: 'working', label: 'Работает', message: 'Изучаю задачу' },
  { status: 'monitoring', label: 'Мониторит', message: 'Слежу за командой' },
] as const;

// Общие тема и компоненты dashboard. Статус и реплика пока приходят из демо,
// впоследствии их должен определять источник событий конкретного заказа.
export default function Office() {
  const [demoIndex, setDemoIndex] = useState(0);
  const activity = demoStates[demoIndex];
  return (
    <main className="office" aria-label="Офис агентов">
      <div className="office-toolbar">
        <ThemePicker />
      </div>
      <div className="office-space">
        <div className="office-scene">
          <img
            className="office-room-image"
            src={room}
            width="1536"
            height="1024"
            alt="Изометрическая комната с диваном, окном и растениями"
          />
          <div className="office-boss">
            <img src={boss} width="1254" height="1254" alt="Босс за MacBook" />
            <div className="office-speech" role="status" aria-atomic="true">
              {activity.message && (
                <Text className="office-speech-bubble" variant="body-1">
                  {activity.message}
                </Text>
              )}
            </div>
            <div className="office-nameplate">
              <Button
                view="raised"
                size="s"
                aria-label={`Босс: ${activity.label}`}
                title={`${activity.label}. Демо: нажмите для смены состояния`}
                onClick={() =>
                  setDemoIndex((index) => (index + 1) % demoStates.length)
                }
              >
                <span className="office-nameplate-content">
                  <span
                    className={`office-status-dot office-status-dot_${activity.status}`}
                    aria-hidden="true"
                  />
                  Босс
                </span>
              </Button>
            </div>
          </div>
        </div>
      </div>
    </main>
  );
}
