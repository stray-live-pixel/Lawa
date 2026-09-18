import { expect, it } from 'vitest';
import { officeLayout } from './officeLayout';

// Проверяем независимые границы открытого пола по рисунку: стол и подпись
// не должны пересекать мебель сзади или сходить с передних диагоналей.
it('размещает от одного до 50 полных спрайтов внутри пола без пересечений', () => {
  let previousWidth = Infinity;
  for (let count = 1; count <= 50; count++) {
    const seats = officeLayout(count);
    expect(seats).toHaveLength(count);
    expect(seats[0].width).toBeLessThanOrEqual(previousWidth);
    previousWidth = seats[0].width;
    for (const [index, seat] of seats.entries()) {
      const bottom = seat.top + seat.width * 1.5;
      for (const horizontal of [
        seat.left - seat.width / 2,
        seat.left + seat.width / 2,
      ]) {
        expect(seat.top + 0.001).toBeGreaterThanOrEqual(
          55 - ((horizontal - 16) * 13) / 23,
        );
        expect(seat.top + 0.001).toBeGreaterThanOrEqual(
          42 + ((horizontal - 39) * 2) / 20,
        );
        expect(seat.top + 0.001).toBeGreaterThanOrEqual(
          44 + ((horizontal - 59) * 11) / 25,
        );
        expect(horizontal).toBeGreaterThanOrEqual(16);
        expect(horizontal).toBeLessThanOrEqual(84);
        expect(bottom).toBeLessThanOrEqual(
          89 - Math.abs(horizontal - 50) + 0.001,
        );
      }
      for (const other of seats.slice(index + 1)) {
        const separateColumns = Math.abs(seat.left - other.left) >= seat.width;
        const separateRows = Math.abs(seat.top - other.top) >= seat.width * 1.5;
        expect(separateColumns || separateRows).toBe(true);
      }
    }
    // Переходы плеера и повторные опросы не должны случайно менять места.
    expect(officeLayout(count)).toEqual(seats);
  }
});

it('сохраняет крупный масштаб малой команды и уменьшает большую', () => {
  expect(officeLayout(0)).toEqual([]);
  expect(officeLayout(1)[0].width).toBe(16);
  expect(officeLayout(5)[0].width).toBeGreaterThan(9);
  expect(officeLayout(50)[0].width).toBeGreaterThanOrEqual(3.5);
  expect(officeLayout(50)[0].width).toBeLessThan(officeLayout(12)[0].width);
});
