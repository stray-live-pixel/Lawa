import { expect, it } from 'vitest';
import { fitPhone } from './usePhoneWindow';

// При уменьшении viewport окно целиком остаётся доступным, даже если
// экран меньше обычного минимального размера телефона.
it('возвращает окно в границы небольшого экрана', () => {
  expect(
    fitPhone({ left: 1000, top: 1000, width: 800, height: 900 }, 300, 400),
  ).toEqual({ left: 12, top: 12, width: 276, height: 376 });
});
it('ограничивает минимальный размер и отрицательное смещение', () => {
  expect(
    fitPhone({ left: -50, top: -50, width: 10, height: 10 }, 1024, 800),
  ).toEqual({ left: 12, top: 12, width: 320, height: 420 });
});
