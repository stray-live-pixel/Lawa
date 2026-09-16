import { Button } from '@gravity-ui/uikit';
import { ThemePicker } from '../components/Theme';
import room from '../assets/office/room-transparent.png';
import boss from '../assets/office/boss.png';
import './office.css';

// Та же тема и компоненты, что у dashboard. Сцена пока не связана с runtime:
// кнопка обозначает персонажа, но не запускает агента и не открывает карточку.
export default function Office() {
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
            <div className="office-nameplate">
              <Button view="raised" size="s">
                Босс
              </Button>
            </div>
          </div>
        </div>
      </div>
    </main>
  );
}
