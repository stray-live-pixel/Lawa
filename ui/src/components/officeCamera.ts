// Камера работает в экранных пикселях относительно центра области просмотра.
// 100% — вся комната; верхний предел позволяет разглядеть большую команду.
export const maxOfficeZoom = 6;
export interface OfficeCamera {
  scale: number;
  x: number;
  y: number;
}
export interface CameraBounds {
  width: number;
  height: number;
  sceneWidth: number;
  sceneHeight: number;
}
export const initialOfficeCamera: OfficeCamera = { scale: 1, x: 0, y: 0 };

// Комнату нельзя утащить за экран. Если она меньше окна по одной из осей,
// оставляем её по центру этой оси, в том числе после изменения размера окна.
export function boundCamera(
  camera: OfficeCamera,
  bounds: CameraBounds,
): OfficeCamera {
  const scale = Math.max(1, Math.min(maxOfficeZoom, camera.scale));
  const limitX = Math.max(0, (bounds.sceneWidth * scale - bounds.width) / 2);
  const limitY = Math.max(0, (bounds.sceneHeight * scale - bounds.height) / 2);
  return {
    scale,
    x: limitX ? Math.max(-limitX, Math.min(limitX, camera.x)) : 0,
    y: limitY ? Math.max(-limitY, Math.min(limitY, camera.y)) : 0,
  };
}

// Сохраняем точку комнаты под курсором/серединой двух пальцев. Ограничения
// применяются после вычисления; у края окна они имеют приоритет над якорем.
export function zoomCamera(
  camera: OfficeCamera,
  scale: number,
  anchor = { x: 0, y: 0 },
): OfficeCamera {
  const next = Math.max(1, Math.min(maxOfficeZoom, scale));
  const ratio = next / camera.scale;
  return {
    scale: next,
    x: anchor.x - (anchor.x - camera.x) * ratio,
    y: anchor.y - (anchor.y - camera.y) * ratio,
  };
}
