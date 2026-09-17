import Markdown, { defaultUrlTransform } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Link } from '@gravity-ui/uikit';

// Разрешён только файловый обработчик VS Code. URL команд и другие схемы
// не проходят сюда; абсолютные пути кодируются, сохраняя номер строки/колонки.
export function localFileLink(value: string): string | undefined {
  let path = value;
  if (/^(file|vscode):/i.test(path)) {
    try {
      const url = new URL(path);
      if (
        url.search ||
        url.hash ||
        (url.protocol === 'file:'
          ? url.host && url.host !== 'localhost'
          : url.host !== 'file')
      )
        return;
      path = decodeURIComponent(url.pathname);
    } catch {
      return;
    }
  } else {
    try {
      path = decodeURIComponent(path);
    } catch {
      return;
    }
  }
  if (/^[A-Za-z]:[\\/]/.test(path)) path = '/' + path.replaceAll('\\', '/');
  if (
    !path.startsWith('/') ||
    path.startsWith('//') ||
    /[\u0000-\u001f]/.test(path)
  )
    return;
  return (
    'vscode://file' +
    path
      .split('/')
      .map((part) => encodeURIComponent(part).replaceAll('%3A', ':'))
      .join('/')
  );
}

// Небольшое дополнение к GFM: файловые пути в обычном тексте становятся
// ссылками. Уже размеченные ссылки и блоки кода не изменяются; пути с пробелами
// можно оформить Markdown-ссылкой или inline code, где границы однозначны.
interface Node {
  type: string;
  value?: string;
  url?: string;
  children?: Node[];
}
function remarkLocalFiles() {
  return (tree: Node) => {
    function visit(parent: Node) {
      if (!parent.children || ['link', 'image', 'code'].includes(parent.type))
        return;
      parent.children = parent.children.flatMap((child) => {
        if (child.type !== 'text') {
          visit(child);
          return [child];
        }
        const value = child.value || '';
        const result: Node[] = [];
        const paths = /(?:^|(?<=\s))\/(?!\/)[^\s<>"`]+\/[^\s<>"`]+/g;
        let offset = 0;
        for (const match of value.matchAll(paths)) {
          const path = match[0].replace(/[.,;!?)\]}]+$/, '');
          if (!localFileLink(path)) continue;
          result.push({
            type: 'text',
            value: value.slice(offset, match.index),
          });
          result.push({
            type: 'link',
            url: path,
            children: [{ type: 'text', value: path }],
          });
          offset = match.index! + path.length;
        }
        if (!result.length) return [child];
        result.push({ type: 'text', value: value.slice(offset) });
        return result;
      });
    }
    visit(tree);
  };
}

// HTML не исполняется, изображения остаются ссылками без фоновой загрузки.
// Выделение адресата — только оформление: маршрутизацию уже выполнил сервер.
export function TeamMarkdown({ text, to }: { text: string; to?: string }) {
  const content =
    to && text.startsWith(`@${to} `)
      ? `**@${to}**${text.slice(to.length + 1)}`
      : text;
  return (
    <Markdown
      remarkPlugins={[remarkGfm, remarkLocalFiles]}
      skipHtml
      urlTransform={(url) => localFileLink(url) || defaultUrlTransform(url)}
      components={{
        a: ({ href, children }) =>
          href ? (
            <Link
              href={href}
              target={href.startsWith('vscode:') ? undefined : '_blank'}
              rel="noopener noreferrer"
            >
              {children}
            </Link>
          ) : (
            <>{children}</>
          ),
        img: ({ src, alt }) =>
          typeof src === 'string' ? (
            <Link href={src}>{alt || 'Изображение'}</Link>
          ) : (
            <>{alt}</>
          ),
        code: ({ children, className }) => {
          const href =
            !className &&
            typeof children === 'string' &&
            !children.includes('\n')
              ? localFileLink(children)
              : undefined;
          return (
            <code className={className}>
              {href ? <Link href={href}>{children}</Link> : children}
            </code>
          );
        },
        strong: ({ children }) => (
          <strong
            className={children === `@${to}` ? 'team-mention' : undefined}
          >
            {children}
          </strong>
        ),
      }}
    >
      {content}
    </Markdown>
  );
}
