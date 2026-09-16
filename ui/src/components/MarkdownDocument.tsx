import { useEffect, useRef, useState } from 'react';
import Markdown from 'react-markdown';
import { TextArea, Link } from '@gravity-ui/uikit';
import remarkGfm from 'remark-gfm';
import { Button, Dialog, ErrorNotice } from './ui';
import { usePoll } from '../hooks/api';

// Копируется исходная строка, а не innerText от отрендеренного Markdown. HTML
// и опасные URL не исполняются. Изображения выводятся ссылками: открытие частной
// памяти само по себе не должно отправлять запросы на адреса из текста агента.
export function MarkdownDocument({
  text,
  label,
  copyLabel = 'Скопировать Markdown',
}: {
  text: string;
  label: string;
  copyLabel?: string;
}) {
  const [copyState, setCopyState] = useState('');
  const [manual, setManual] = useState(false);
  const raw = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    if (manual) {
      raw.current?.focus();
      raw.current?.select();
    }
  }, [manual]);
  useEffect(() => {
    setCopyState('');
    setManual(false);
  }, [text]);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopyState('Скопировано.');
    } catch {
      setManual(true);
      setCopyState('Скопируйте выделенную разметку: Ctrl+C или ⌘C.');
    }
  };
  return (
    <section className="markdown-document" aria-label={label}>
      <Button onClick={() => void copy()} disabled={!text}>
        {copyLabel}
      </Button>
      {copyState && (
        <p role="status" className="muted">
          {copyState}
        </p>
      )}
      {manual && (
        <TextArea
          controlRef={raw}
          readOnly
          controlProps={{ 'aria-label': `Исходный Markdown: ${label}` }}
          value={text}
        />
      )}
      <div className="markdown">
        <Markdown
          remarkPlugins={[remarkGfm]}
          skipHtml
          components={{
            a: ({ node: _node, ...props }) => (
              <Link
                {...props}
                href={props.href || ''}
                target="_blank"
                rel="noopener noreferrer"
              />
            ),
            img: ({ src, alt }) => (
              <Link href={src || ''} target="_blank" rel="noopener noreferrer">
                {alt || 'Изображение'}
              </Link>
            ),
          }}
        >
          {text}
        </Markdown>
      </div>
    </section>
  );
}

// Текст читается только при открытии модалки; при смене cube URL отменяет старый
// запрос. Закрытие размонтирует reader и прекращает чтение приватной памяти.
export function MemoryDialog({
  url,
  open,
  onOpenChange,
}: {
  url?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange} title="Память кубика">
      {open && url && <MemoryContent url={url} />}
    </Dialog>
  );
}
function MemoryContent({ url }: { url: string }) {
  const { data, error } = usePoll<string>(url, 0, 'text');
  return (
    <>
      <ErrorNotice error={error} />
      {data !== undefined ? (
        <MarkdownDocument
          text={data}
          label="Память кубика"
          copyLabel="Скопировать память"
        />
      ) : (
        !error && <p>Загрузка памяти…</p>
      )}
    </>
  );
}
