import { expect, it } from 'vitest';
import { officeLayout, officeSpriteFrame } from './officeLayout';

// Проверяем независимые границы открытого пола по рисунку: стол и подпись
// не должны пересекать мебель сзади или сходить с передних диагоналей.
it('размещает от одного до 50 видимых рамок внутри пола без пересечений', () => {
  let previousWidth = Infinity;
  for (let count = 1; count <= 50; count++) {
    const seats = officeLayout(count);
    expect(seats).toHaveLength(count);
    expect(seats[0].width).toBeLessThanOrEqual(previousWidth);
    previousWidth = seats[0].width;
    for (const [index, seat] of seats.entries()) {
      const bottom = seat.top + (seat.width / officeSpriteFrame.width) * 1.5;
      for (const horizontal of [
        seat.left - seat.width / 2,
        seat.left + seat.width / 2,
      ]) {
        expect(seat.top + 0.001).toBeGreaterThanOrEqual(
          53 - ((horizontal - 9) * 11) / 19,
        );
        expect(seat.top + 0.001).toBeGreaterThanOrEqual(42);
        expect(seat.top + 0.001).toBeGreaterThanOrEqual(
          42 + ((horizontal - 59) * 11) / 28,
        );
        expect(seat.top + 0.001).toBeGreaterThanOrEqual(horizontal - 34);
        expect(horizontal).toBeGreaterThanOrEqual(9);
        expect(horizontal).toBeLessThanOrEqual(91);
        expect(bottom).toBeLessThanOrEqual(
          92 - ((horizontal - 49) * 35) / 42 + 0.001,
        );
        expect(bottom).toBeLessThanOrEqual(
          92 + ((horizontal - 49) * 39) / 40 + 0.001,
        );
      }
      for (const other of seats.slice(index + 1)) {
        const separateColumns = Math.abs(seat.left - other.left) >= seat.width;
        const separateRows =
          Math.abs(seat.top - other.top) >=
          (seat.width / officeSpriteFrame.width) * 1.5;
        expect(separateColumns || separateRows).toBe(true);
      }
    }
    // Переходы плеера и повторные опросы не должны случайно менять места.
    expect(officeLayout(count)).toEqual(seats);
  }
});

it('сохраняет крупный масштаб малой команды и уменьшает большую', () => {
  expect(officeLayout(0)).toEqual([]);
  expect(officeLayout(1)[0].width).toBe(16 * officeSpriteFrame.width);
  expect(officeLayout(5)[0].width).toBeGreaterThan(
    13 * officeSpriteFrame.width,
  );
  expect(officeLayout(50)[0].width).toBeGreaterThanOrEqual(
    5 * officeSpriteFrame.width,
  );
  expect(officeLayout(50)[0].width).toBeLessThan(officeLayout(12)[0].width);
});

// Регрессия: прежний запас оставлял пустыми боковые участки, а 50 спрайтов
// сжимались до 3.8% ширины комнаты. Проверяем и масштаб, и занятую ширину.
it('использует боковой пол и увеличивает персонажей в большой команде', () => {
  const seats = officeLayout(50);
  const left = Math.min(...seats.map((seat) => seat.left - seat.width / 2));
  const right = Math.max(...seats.map((seat) => seat.left + seat.width / 2));
  expect(right - left).toBeGreaterThan(65);
  expect(seats[0].width / officeSpriteFrame.width).toBeGreaterThan(3.8 * 1.3);
});
