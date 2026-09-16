import type { Execution } from '../types';

// Вход — visits одного кубика в порядке истории API. Реальная активность имеет
// приоритет над поздним skipped; среди остальных выбираем последнее нескipped.
// Явный выбор в истории сохраняется, пока visit существует, даже при polling.
// Отсутствие visits возвращает undefined, а не выдуманное pending-посещение.
export function displayVisit(
  visits: Execution[],
  chosen?: string,
): Execution | undefined {
  const explicit = chosen && visits.find((visit) => visit.Key === chosen);
  if (explicit) return explicit;
  const ordered = [...visits].sort((a, b) => (b.Visit || 1) - (a.Visit || 1));
  return (
    ordered.find((visit) =>
      ['starting', 'running', 'waiting_for_approval'].includes(visit.State),
    ) ||
    ordered.find((visit) => visit.State !== 'skipped') ||
    ordered[0]
  );
}
