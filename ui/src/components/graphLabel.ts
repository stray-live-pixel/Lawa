// Размер подписи участвует в раскладке до маршрутизации. Измеритель передаётся
// отдельно: браузер использует шрифт Gravity, тесты — воспроизводимые метрики.
export type TextMeasure = (text: string, font?: 'label' | 'title') => number;
export interface LabelSize {
  width: number;
  height: number;
  lines: string[];
}
export function graphLabel(
  text: string,
  measure: TextMeasure = (text) => [...text].length * 7,
): LabelSize {
  if (!text) return { width: 0, height: 0, lines: [] };
  const lines: string[] = [];
  let line = '';
  for (const word of text.trim().split(/\s+/)) {
    if (line && measure(`${line} ${word}`) <= 120) {
      line += ` ${word}`;
      continue;
    }
    if (line) lines.push(line);
    line = '';
    for (const character of word) {
      if (line && measure(line + character) > 120) {
        lines.push(line);
        line = '';
      }
      line += character;
    }
  }
  if (line) lines.push(line);
  if (lines.length > 3) {
    lines.length = 3;
    while (lines[2] && measure(lines[2] + '…') > 120)
      lines[2] = [...lines[2]].slice(0, -1).join('');
    lines[2] += '…';
  }
  return {
    width: Math.min(
      120,
      Math.ceil(Math.max(...lines.map((line) => measure(line)))),
    ),
    height: lines.length * 18,
    lines,
  };
}
