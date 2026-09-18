// Геометрия привязана к room-large-selected.png (1536 × 1024). Многоугольник
// целиком лежит на свободном полу: исключены мебель, стены и передний край.
const floor = [
  [9, 53],
  [28, 42],
  [59, 42],
  [87, 53],
  [91, 57],
  [49, 92],
];

// Общая рамка 42 образов: значимые пиксели (alpha >= 32) лежат по X в
// 17.6–87.6% PNG. Оставляем запас, обрезая только боковые прозрачные поля.
// Высоту сохраняем полностью: рога, уши и ножки столов не должны обрезаться.
// При добавлении внешности за пределами рамки её нужно расширить для всех.
export const officeSpriteFrame = { left: 0.16, width: 0.73 };

const floorTop = Math.min(...floor.map((point) => point[1]));
const floorHeight = Math.max(...floor.map((point) => point[1])) - floorTop;

// Проценты сцены: left — центр видимой рамки, top — верх, width — ширина рамки.
export interface OfficeSeat {
  left: number;
  top: number;
  width: number;
}

// Подбираем наибольший общий масштаб по видимым рамкам, без прозрачных полей.
// Проверяем весь прямоугольник, а не только точку под ногами. Проценты высоты
// отличаются от процентов ширины в 1.5 раза из-за пропорций фоновой картинки.
// Число рядов не ограничено: 50 участников помещаются без наложения столов.
export function officeLayout(count: number): OfficeSeat[] {
  if (count <= 0) return [];
  for (let step = 160; step > 0; step--) {
    const width = (step / 10) * officeSpriteFrame.width;
    const seats = seatsAtSize(width);
    if (seats.length < count) continue;
    // Неполная команда занимает центр пола, а не левый край последнего ряда.
    return seats
      .sort((first, second) => distance(first) - distance(second))
      .slice(0, count)
      .sort(
        (first, second) => first.top - second.top || first.left - second.left,
      );
  }
  return [];
}

// Горизонтальные ряды используют широкую середину комнаты. Отступ уменьшается
// вместе со спрайтом; подпись находится внутри его нижнего прозрачного поля.
function seatsAtSize(width: number): OfficeSeat[] {
  const height = (width / officeSpriteFrame.width) * 1.5;
  const gap = width * 0.08;
  const rows = Math.floor((floorHeight + gap) / (height + gap));
  const offset = (floorHeight - rows * height - (rows - 1) * gap) / 2;
  const seats: OfficeSeat[] = [];
  for (let row = 0; row < rows; row++) {
    const top = floorTop + offset + row * (height + gap);
    const upper = floorSpan(top);
    const lower = floorSpan(top + height);
    const left = Math.max(upper[0], lower[0]);
    const right = Math.min(upper[1], lower[1]);
    const columns = Math.floor((right - left + gap) / (width + gap));
    for (let column = 0; column < columns; column++) {
      seats.push({
        left: (left + right) / 2 + (column - (columns - 1) / 2) * (width + gap),
        top,
        width,
      });
    }
  }
  return seats;
}

// Выпуклый пол: пересечение интервалов сверху и снизу содержит весь стол.
function floorSpan(top: number): [number, number] {
  const intersections: number[] = [];
  floor.forEach(([left, upper], index) => {
    const [right, lower] = floor[(index + 1) % floor.length];
    if (top < Math.min(upper, lower) || top > Math.max(upper, lower)) return;
    if (lower === upper) {
      intersections.push(left, right);
      return;
    }
    intersections.push(
      left + ((top - upper) * (right - left)) / (lower - upper),
    );
  });
  return [Math.min(...intersections), Math.max(...intersections)];
}

// Центр свободной области смещён вниз относительно центра изображения комнаты.
function distance(seat: OfficeSeat): number {
  return (
    (seat.left - 50) ** 2 +
    (seat.top + (seat.width / officeSpriteFrame.width) * 0.75 - 64) ** 2
  );
}
