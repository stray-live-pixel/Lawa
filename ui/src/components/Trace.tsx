import { useEffect, useRef, useState } from 'react';
import type { Trace as TraceResponse, TraceEvent } from '../types';
import { request } from '../hooks/api';
import { ErrorNotice } from './ui';

export interface TraceBlock extends TraceEvent {
  key: string;
  content: string;
}
// Delta накапливается внутри item и turn; завершённый item заменяет накопленный
// текст. Ограничение касается отображения, полный журнал остаётся в хранилище.
export function mergeTrace(
  blocks: Map<string, TraceBlock>,
  events: TraceEvent[],
) {
  for (const event of events) {
    if (!event.content && !event.message) continue;
    const key = JSON.stringify([
      event.turnId,
      event.itemId || event.kind,
      event.itemType,
    ]);
    const previous = blocks.get(key);
    const content = event.kind.endsWith('_delta')
      ? (previous?.content || '') + (event.content || '')
      : event.content || event.message || '';
    blocks.set(key, { ...event, key, content: content.slice(-64000) });
    if (blocks.size > 200) blocks.delete(blocks.keys().next().value!);
  }
}

// Состояние привязано к полному URL, включая visit. Выделение текста не теряется:
// новые блоки собираются отдельно и публикуются после снятия выделения человеком.
export function Trace({ url }: { url?: string }) {
  const container = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<{
    url?: string;
    blocks: TraceBlock[];
    error?: string;
    loaded: boolean;
  }>({ url, blocks: [], loaded: false });
  useEffect(() => {
    if (!url || url === '#preview') return;
    const controller = new AbortController();
    let disposed = false,
      cursor = 0;
    let timer: ReturnType<typeof setTimeout>;
    const blocks = new Map<string, TraceBlock>();
    const load = async () => {
      try {
        const target = new URL(url, location.origin);
        target.searchParams.set('after', String(cursor));
        const payload = await request<TraceResponse>(
          target.pathname + target.search,
          controller.signal,
        );
        if (disposed) return;
        mergeTrace(blocks, payload.events || []);
        cursor = payload.next;
        const selection = window.getSelection();
        if (
          selection?.isCollapsed !== false ||
          !container.current?.contains(selection.anchorNode)
        ) {
          setState({
            url,
            blocks: [...blocks.values()].reverse(),
            loaded: true,
          });
        }
      } catch (error) {
        if (!disposed)
          setState((previous) => ({
            url,
            blocks: previous.url === url ? previous.blocks : [],
            loaded: true,
            error: String(error),
          }));
      } finally {
        if (!disposed) timer = setTimeout(load, 3000);
      }
    };
    void load();
    return () => {
      disposed = true;
      controller.abort();
      clearTimeout(timer);
    };
  }, [url]);
  const visible =
    state.url === url ? state : { blocks: [], loaded: false, error: undefined };
  return (
    <div ref={container} className="messages">
      <ErrorNotice error={visible.error} />
      {!visible.blocks.length && (
        <p className="muted">
          {!url || url === '#preview'
            ? 'Сообщений пока нет.'
            : visible.loaded
              ? 'Агент ещё не прислал сообщения.'
              : 'Загрузка…'}
        </p>
      )}
      {visible.blocks.map((event) => (
        <article className="message" key={event.key}>
          <small>
            {event.itemType === 'agentMessage'
              ? 'Сообщение агента'
              : event.itemType || event.kind}{' '}
            · {new Date(event.time).toLocaleTimeString()}
          </small>
          <pre>{event.content}</pre>
        </article>
      ))}
    </div>
  );
}
