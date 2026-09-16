import { useState } from 'react';
import room from '../assets/office/room.png';
import boss from '../assets/office/boss.png';
import './office.css';

// Отдельная сцена без запросов к runtime: образ персонажа не выдаётся за живое
// состояние агента. Комната и персонаж — независимые изображения в одной системе
// координат; изменение ширины сохраняет положение Босса относительно мебели.
export default function Office() {
  const [selected, setSelected] = useState(false);

  return (
    <main className="office">
      <header className="office-header">
        <div className="office-brand" aria-label="Lawa — офис">
          <span className="office-brand-mark" aria-hidden="true">
            l
          </span>
          lawa <span className="office-brand-divider">/</span>
          <span className="office-brand-section">офис</span>
        </div>
        <span className="office-preview">Визуальный прототип</span>
      </header>

      <div className="office-intro">
        <div>
          <p className="office-eyebrow">ПРОСТРАНСТВО КОМАНДЫ</p>
          <h1>У каждой истории есть начало.</h1>
          <p className="office-subtitle">
            Тихий офис. Большие замыслы. Первый участник.
          </p>
        </div>
        <span className="office-room-number">
          01 <span>/ ОБЩАЯ КОМНАТА</span>
        </span>
      </div>

      <div className="office-layout">
        <section className="office-room" aria-label="Изометрический офис">
          <div className="office-scene">
            <img
              className="office-room-image"
              src={room}
              width="1536"
              height="1024"
              alt="Уютная пластилиновая комната: бежевые стены, диван, окно и зелёные растения"
            />
            <button
              className="office-boss"
              type="button"
              aria-label="Познакомиться с Боссом"
              aria-expanded={selected}
              aria-controls="office-person"
              onClick={() => setSelected(!selected)}
            >
              <img
                src={boss}
                width="1254"
                height="1254"
                alt="Босс — седой программист в очках за MacBook"
              />
              <span className="office-nameplate">
                <span aria-hidden="true" />
                Босс <span aria-hidden="true">↗</span>
              </span>
            </button>
          </div>
          <p className="office-scene-hint">
            <span aria-hidden="true">↖</span> Нажмите на Босса, чтобы
            познакомиться
          </p>
        </section>

        <aside
          className="office-sidebar"
          id="office-person"
          aria-label="Участник офиса"
        >
          <div className="office-sidebar-heading">
            <span>В комнате</span>
            <span>01</span>
          </div>
          <div className="office-person-card">
            <div className="office-person-portrait">
              <img src={boss} alt="" />
            </div>
            <span className="office-person-role">ПЕРВЫЙ УЧАСТНИК</span>
            <h2>Босс</h2>
            <p className="office-person-subtitle">Опытный программист</p>
            {selected ? (
              <div className="office-person-story">
                <p>
                  За плечами — много проектов. На столе — MacBook и кофе. В
                  привычках — сначала разобраться, потом писать код.
                </p>
                <div className="office-person-purpose">
                  <span>Его роль в команде</span>
                  <p>
                    Понять вашу цель, собрать общую картину и помочь команде
                    довести работу до результата.
                  </p>
                </div>
                <button
                  type="button"
                  className="office-person-action"
                  onClick={() => setSelected(false)}
                >
                  Свернуть историю <span aria-hidden="true">−</span>
                </button>
              </div>
            ) : (
              <>
                <p className="office-person-summary">
                  Любит ясные задачи, вдумчивые решения и когда всё работает.
                </p>
                <button
                  type="button"
                  className="office-person-action"
                  onClick={() => setSelected(true)}
                >
                  Познакомиться <span aria-hidden="true">↗</span>
                </button>
              </>
            )}
          </div>
          <p className="office-sidebar-note">
            Пока здесь только Босс.
            <br />
            Для будущей команды есть место.
          </p>
        </aside>
      </div>

      <footer className="office-footer">
        <span>Комната готова к новым историям</span>
        <span>LAWA OFFICE · 01</span>
      </footer>
    </main>
  );
}
