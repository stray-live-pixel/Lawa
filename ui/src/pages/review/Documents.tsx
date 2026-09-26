import { useState, useEffect, useRef } from 'react';
import { Icon, Label, TextArea } from '@gravity-ui/uikit';
import { ArrowUpRight, Copy } from '@gravity-ui/icons';
import Markdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Button, Dialog, ErrorNotice } from '../../components/ui';
import { usePoll } from '../../hooks/api';
import { artifactURL, safeURL } from './helpers';

// Markdown агентов не исполняет HTML; внешние картинки остаются явными ссылками.
export function ReviewMarkdown({ text }: { text: string }) {
  return (
    <div className="markdown cr-markdown">
      <Markdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        components={{
          a: ({ href, children }) => (
            <a
              href={safeURL(href || '')}
              target="_blank"
              rel="noopener noreferrer"
            >
              {children}
            </a>
          ),
          img: ({ src, alt }) => (
            <a
              href={safeURL(typeof src === 'string' ? src : '')}
              target="_blank"
              rel="noopener noreferrer"
            >
              {alt || 'Изображение'}
            </a>
          ),
        }}
      >
        {text}
      </Markdown>
    </div>
  );
}
export function LinkLabel({ label, url }: { label: string; url: string }) {
  const href = safeURL(url);
  return href ? (
    <Button href={href} target="_blank" rel="noopener noreferrer" size="s">
      {label}
      <Icon data={ArrowUpRight} size={14} />
    </Button>
  ) : (
    <Label size="s">{label}</Label>
  );
}
export function SavedText({
  id,
  path,
  markdown = true,
  preview = false,
}: {
  id: string;
  path: string;
  markdown?: boolean;
  preview?: boolean;
}) {
  const { data, error } = usePoll<string>(
    path ? artifactURL(id, path) : null,
    0,
    'text',
  );
  return (
    <>
      <ErrorNotice error={error} />
      {data !== undefined ? (
        <div className={preview ? 'cr-text-preview' : ''}>
          {markdown ? (
            <ReviewMarkdown text={data} />
          ) : (
            <pre className="cr-code-raw">{data}</pre>
          )}
        </div>
      ) : (
        !error && (
          <p className="cr-muted">
            {path ? 'Загрузка…' : 'Документ не сохранён'}
          </p>
        )
      )}
    </>
  );
}
export function TextDialog({
  id,
  path,
  title,
  close,
  markdown = true,
}: {
  id: string;
  path: string;
  title: string;
  close: () => void;
  markdown?: boolean;
}) {
  return (
    <Dialog open onOpenChange={close} title={title}>
      <SavedText id={id} path={path} markdown={markdown} />
    </Dialog>
  );
}
// Полный исходник остаётся доступен при отказе системного буфера обмена.
export function CopyPrompt({ id, path }: { id: string; path: string }) {
  const [open, setOpen] = useState(false),
    [text, setText] = useState(''),
    [busy, setBusy] = useState(false),
    [error, setError] = useState(''),
    [copied, setCopied] = useState(false);
  const raw = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    if (open) {
      raw.current?.focus();
      raw.current?.select();
    }
  }, [open]);
  const copy = async () => {
    setBusy(true);
    setError('');
    try {
      const response = await fetch(artifactURL(id, path));
      if (!response.ok) throw new Error(await response.text());
      const value = await response.text();
      setText(value);
      try {
        await navigator.clipboard.writeText(value);
        setCopied(true);
      } catch {
        setOpen(true);
      }
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <Button
        disabled={!path || busy}
        loading={busy}
        onClick={() => void copy()}
      >
        <Icon data={Copy} size={14} />
        {copied ? 'Скопировано' : 'Скопировать промпт'}
      </Button>
      <ErrorNotice error={error} />
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="Промпт исправления"
        initialFocus={raw}
      >
        <p className="cr-muted">
          Буфер обмена недоступен. Скопируйте текст: Ctrl+C или ⌘C.
        </p>
        <TextArea
          controlRef={raw}
          readOnly
          value={text}
          minRows={12}
          onFocus={(event) => event.currentTarget.select()}
          controlProps={{
            'aria-label': 'Промпт для ручного копирования',
          }}
        />
      </Dialog>
    </>
  );
}

// Unified diff сохраняет строки исходника; цвет кодирует добавление/удаление,
// а не критичность замечания. Большие фрагменты прокручиваются без переноса кода.
export function DiffDialog({
  id,
  path,
  close,
}: {
  id: string;
  path: string;
  close: () => void;
}) {
  const { data, error } = usePoll<string>(artifactURL(id, path), 0, 'text');
  return (
    <Dialog open onOpenChange={close} title="Изменения">
      <ErrorNotice error={error} />
      <pre className="cr-code-raw cr-diff">
        {data?.split('\n').map((line, i) => (
          <div
            key={i}
            className={
              line.startsWith('+') && !line.startsWith('+++')
                ? 'added'
                : line.startsWith('-') && !line.startsWith('---')
                  ? 'deleted'
                  : line.startsWith('@@')
                    ? 'context'
                    : ''
            }
          >
            {line || ' '}
          </div>
        ))}
      </pre>
    </Dialog>
  );
}
