import { useId, type ReactNode, type CSSProperties } from 'react';
import { Button, Icon } from '@gravity-ui/uikit';
import { Minus, Plus, ArrowsExpand } from '@gravity-ui/icons';
import { useOfficeCamera } from './useOfficeCamera';
import { maxOfficeZoom } from './officeCamera';
import './office-map.css';

// Управление остаётся над неподвижным окном; только комната с сотрудниками
// получает transform. Проигрыватель и телефон не масштабируются вместе с картой.
export function OfficeMap({ children }: { children: ReactNode }) {
  const view = useOfficeCamera();
  const hint = useId();
  return (
    <div
      ref={view.viewport}
      className={`office-map ${view.dragging ? 'office-map-dragging' : ''}`}
      role="region"
      aria-label="Карта офиса"
      aria-describedby={hint}
      tabIndex={0}
      onPointerDown={view.pointerDown}
      onPointerMove={view.pointerMove}
      onPointerUp={view.pointerEnd}
      onPointerCancel={view.pointerEnd}
      onLostPointerCapture={view.pointerEnd}
      onClickCapture={view.clickCapture}
      onKeyDown={view.keyDown}
      onFocusCapture={view.focusCapture}
      onDragStart={(event) => event.preventDefault()}
    >
      <div
        ref={view.scene}
        className="office-map-scene"
        style={
          {
            '--office-camera-scale': view.camera.scale,
            transform: `translate(${view.camera.x}px, ${view.camera.y}px) scale(${view.camera.scale})`,
          } as CSSProperties
        }
      >
        {children}
      </div>
      <div
        className="office-map-controls"
        role="toolbar"
        aria-label="Масштаб карты"
      >
        <Button
          view="flat"
          aria-label="Отдалить карту"
          title="Отдалить (−)"
          disabled={view.camera.scale <= 1}
          onClick={() => view.zoom(1 / 1.25)}
        >
          <Icon data={Minus} />
        </Button>
        <span className="office-map-scale" aria-label="Текущий масштаб">
          {Math.round(view.camera.scale * 100)}%
        </span>
        <Button
          view="flat"
          aria-label="Приблизить карту"
          title="Приблизить (+)"
          disabled={view.camera.scale >= maxOfficeZoom}
          onClick={() => view.zoom(1.25)}
        >
          <Icon data={Plus} />
        </Button>
        <Button
          view="flat"
          aria-label="Показать весь офис"
          title="Показать весь офис (0)"
          onClick={view.reset}
        >
          <Icon data={ArrowsExpand} />
        </Button>
      </div>
      <span id={hint} className="office-map-hint">
        Колесо или два пальца — масштаб; перетаскивание — обзор. Клавиши: + / −,
        стрелки, 0 — весь офис.
      </span>
    </div>
  );
}
