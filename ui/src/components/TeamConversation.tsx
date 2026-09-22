import { useLayoutEffect, useState, type ReactNode } from 'react';
import { Button, ClipboardButton, Text } from '@gravity-ui/uikit';
import { TeamMarkdown } from './TeamMarkdown';
import { type TeamMessage } from './TeamPhone';

// Версия определяется только видимыми событиями: исторический кадр не может
// получить будущую границу. Каждый оригинал присутствует ровно в одном участке.
export function conversationBoundary(messages: TeamMessage[]) {
  return (
    [...messages].reverse().find((message) => message.summary)?.summary
      ?.through || 0
  );
}

// Архив один, без вложенных cut при повторном сжатии. Старые системные события
// и версии сводки остаются в исходном порядке и доступны при раскрытии.
export function TeamConversation({
  messages,
  renderMessage,
  preserveReading,
}: {
  messages: TeamMessage[];
  renderMessage: (message: TeamMessage) => ReactNode;
  preserveReading: boolean;
}) {
  const boundary = conversationBoundary(messages);
  const [archive, setArchive] = useState({ boundary, expanded: false });
  const [source, setSource] = useState('');
  // Обновляем состояние до commit DOM. Иначе исходник на один commit исчезает
  // из закрытого архива и родитель не может восстановить ориентир прокрутки.
  if (archive.boundary !== boundary) {
    setArchive({ boundary, expanded: archive.expanded || preserveReading });
  }
  const expanded = archive.expanded;
  function setExpanded(value: boolean) {
    setArchive({ boundary, expanded: value });
  }
  useLayoutEffect(() => {
    if (!source) return;
    const element = document.getElementById(`team-message-${source}`);
    element?.scrollIntoView({ block: 'center' });
    element?.focus({ preventScroll: true });
    setSource('');
  }, [source, expanded]);
  function goToSource(id: string) {
    setExpanded(true);
    setSource(id);
  }
  function render(message: TeamMessage) {
    if (!message.summary) return renderMessage(message);
    return (
      <section
        key={message.id}
        id={`team-message-${message.id}`}
        tabIndex={-1}
        className="team-compaction"
      >
        <Text variant="subheader-1">Босс · Переписка сжата</Text>
        <Text color="secondary">
          До позиции {message.summary.through} ·{' '}
          {new Date(message.date).toLocaleString('ru-RU')}
        </Text>
        <TeamMarkdown text={message.summary.text} />
        <div className="team-summary-sources">
          <ClipboardButton
            text={message.summary.text}
            title="Копировать сводку"
          />
          <Text color="secondary">Источники:</Text>
          {message.summary.sourceIds.map((id) => (
            <Button
              key={id}
              view="flat"
              size="s"
              title={id}
              aria-label={id}
              onClick={() => goToSource(id)}
            >
              {/* Полный ID доступен в подписи и подсказке; длинный ключ не ломает строку. */}
              {id.length > 30 ? `${id.slice(0, 14)}…${id.slice(-8)}` : id}
            </Button>
          ))}
        </div>
      </section>
    );
  }
  return (
    <div className="team-conversation">
      {boundary > 0 && (
        <>
          <Button
            view="flat"
            className="team-archive-toggle"
            aria-expanded={expanded}
            aria-controls="team-chat-archive"
            onClick={() => setExpanded(!expanded)}
          >
            {expanded ? 'Скрыть' : 'Показать'} прошлую переписку · {boundary}{' '}
            сообщений
          </Button>
          <div id="team-chat-archive" hidden={!expanded}>
            {expanded && messages.slice(0, boundary).map(render)}
          </div>
          <div className="team-archive-boundary">
            <Button
              view="flat"
              size="s"
              onClick={() => {
                const event = [...messages].reverse().find((m) => m.summary);
                if (event) {
                  setSource(event.id);
                }
              }}
            >
              К сводке Босса
            </Button>
          </div>
        </>
      )}
      {messages.slice(boundary).map(render)}
    </div>
  );
}

// Переходы к причинам ожидания также раскрывают архив через его кнопку.
export function revealTeamMessage(id: string) {
  const toggle = document.querySelector<HTMLButtonElement>(
    '.team-archive-toggle[aria-expanded="false"]',
  );
  if (!document.getElementById(`team-message-${id}`) && toggle) toggle.click();
  requestAnimationFrame(() =>
    document
      .getElementById(`team-message-${id}`)
      ?.scrollIntoView({ block: 'center' }),
  );
}
