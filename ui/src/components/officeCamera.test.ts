import { expect, it } from 'vitest';
import { boundCamera, initialOfficeCamera, zoomCamera } from './officeCamera';

// При зуме меняется экранный размер, но выбранная курсором точка комнаты
// остаётся на месте, пока перемещение не упирается в границы карты.
it('сохраняет точку под курсором при приближении и отдалении', () => {
  const camera = { scale: 2, x: 30, y: -20 };
  const anchor = { x: 160, y: 100 };
  const next = zoomCamera(camera, 4, anchor);
  expect((anchor.x - next.x) / next.scale).toBe(
    (anchor.x - camera.x) / camera.scale,
  );
  expect((anchor.y - next.y) / next.scale).toBe(
    (anchor.y - camera.y) / camera.scale,
  );
  expect(zoomCamera(next, 2, anchor)).toEqual(camera);
});

it('не позволяет увести увеличенную комнату за экран', () => {
  expect(
    boundCamera(
      { scale: 2, x: 900, y: -900 },
      { width: 800, height: 600, sceneWidth: 600, sceneHeight: 400 },
    ),
  ).toEqual({ scale: 2, x: 200, y: -100 });
});

it('центрирует комнату после отдаления или расширения окна', () => {
  const camera = { scale: 2, x: 200, y: -100 };
  expect(
    boundCamera(camera, {
      width: 1600,
      height: 1000,
      sceneWidth: 600,
      sceneHeight: 400,
    }),
  ).toEqual({ scale: 2, x: 0, y: 0 });
  expect(
    boundCamera(zoomCamera(camera, 1), {
      width: 800,
      height: 600,
      sceneWidth: 600,
      sceneHeight: 400,
    }),
  ).toEqual(initialOfficeCamera);
});

it('ограничивает масштаб диапазоном 100–600%', () => {
  expect(zoomCamera(initialOfficeCamera, 100).scale).toBe(6);
  expect(zoomCamera(initialOfficeCamera, 0.1).scale).toBe(1);
});
