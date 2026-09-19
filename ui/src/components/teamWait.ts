import type { TeamWait } from './TeamPhone';

const labels: Record<string, string> = {
  no_messages: 'Нет сообщений',
  queue: 'В очереди запуска',
  capacity: 'Ждёт свободный слот',
  result: 'Ждёт результат',
  acceptance: 'Ждёт приёмку',
  error: 'Ошибка',
  permission: 'Ждёт разрешение',
};

// Возраст считается относительно курсора плеера: история не стареет вместе
// с живым офисом. Старые заказы не получают выдуманное время начала ожидания.
export function waitLabel(wait: TeamWait, at: number): string {
  const elapsed = Math.max(0, Math.floor((at - Date.parse(wait.since)) / 1000));
  const duration = !Number.isFinite(elapsed)
    ? ''
    : elapsed < 60
      ? 'меньше минуты'
      : elapsed < 3600
        ? `${Math.floor(elapsed / 60)} мин`
        : `${Math.floor(elapsed / 3600)} ч ${Math.floor((elapsed % 3600) / 60)} мин`;
  return [labels[wait.kind] || 'Ожидание', duration]
    .filter(Boolean)
    .join(' · ');
}

// Полная причина доступна рядом с короткой подписью, включая автора заявления
// и ссылку на сообщение. Runtime не выдаётся за источник смыслового вывода.
export function waitDetails(wait: TeamWait): string {
  return [
    wait.source === 'actor' ? 'Сообщил сотрудник' : 'Определено Lawa',
    wait.actorId && `Участник: @${wait.actorId}`,
    wait.messageId && `Сообщение: ${wait.messageId}`,
    wait.text,
  ]
    .filter(Boolean)
    .join('. ');
}
