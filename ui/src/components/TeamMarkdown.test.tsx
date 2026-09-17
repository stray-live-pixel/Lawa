import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it } from 'vitest';
import { AppTheme } from './Theme';
import { TeamMarkdown, localFileLink } from './TeamMarkdown';
afterEach(cleanup);

// И путь с пробелами, и указание строки остаются файловым URL, а не командой.
it('кодирует локальные файлы для VS Code', () => {
  expect(localFileLink('/Users/me/My Project/игра.ts:12:3')).toBe(
    'vscode://file/Users/me/My%20Project/%D0%B8%D0%B3%D1%80%D0%B0.ts:12:3',
  );
  expect(localFileLink('file:///tmp/My%20File.ts')).toBe(
    'vscode://file/tmp/My%20File.ts',
  );
  expect(localFileLink('vscode://file/tmp/game.ts:2')).toBe(
    'vscode://file/tmp/game.ts:2',
  );
  expect(localFileLink('vscode://command/run')).toBeUndefined();
  expect(localFileLink('file://remote/secret')).toBeUndefined();
});

// Форматирование и GFM работают в сообщениях, путь распознаётся в ссылке,
// обычной реплике и inline code. Кодовый блок не превращается в набор ссылок.
it('рендерит markdown и открывает пути через файловую схему', () => {
  render(
    <AppTheme>
      <TeamMarkdown
        to="boss"
        text={
          '@boss **Готово**\n\n- пункт\n\n[Исходник](</Users/me/My Project/main.ts:12>)\n\nОткрой /tmp/game.js. Или `/tmp/another file.ts`.\n\n```js\n/tmp/code.ts\n```\n\n[Сайт](https://example.com)'
        }
      />
    </AppTheme>,
  );
  expect(screen.getByText('Готово').tagName).toBe('STRONG');
  expect(screen.getByText('@boss')).toHaveClass('team-mention');
  expect(screen.getByRole('listitem')).toHaveTextContent('пункт');
  expect(screen.getByRole('link', { name: 'Исходник' })).toHaveAttribute(
    'href',
    'vscode://file/Users/me/My%20Project/main.ts:12',
  );
  expect(screen.getByRole('link', { name: '/tmp/game.js' })).toHaveAttribute(
    'href',
    'vscode://file/tmp/game.js',
  );
  expect(
    screen.getByRole('link', { name: '/tmp/another file.ts' }),
  ).toHaveAttribute('href', 'vscode://file/tmp/another%20file.ts');
  expect(
    screen.queryByRole('link', { name: '/tmp/code.ts' }),
  ).not.toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Сайт' })).toHaveAttribute(
    'target',
    '_blank',
  );
});

// Сообщения агентов — данные: HTML, команды и скрытые запросы запрещены.
it('не исполняет HTML и опасные ссылки и не загружает изображения', () => {
  const { container } = render(
    <AppTheme>
      <TeamMarkdown
        text={
          '<script>alert(1)</script>\n\n[Опасно](javascript:alert%281%29) [Команда](vscode://command/unsafe)\n\n![Картинка](https://example.com/private.png)'
        }
      />
    </AppTheme>,
  );
  expect(container.querySelector('script')).toBeNull();
  expect(container.querySelector('img')).toBeNull();
  expect(
    screen.queryByRole('link', { name: 'Опасно' }),
  ).not.toBeInTheDocument();
  expect(
    screen.queryByRole('link', { name: 'Команда' }),
  ).not.toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Картинка' })).toHaveAttribute(
    'href',
    'https://example.com/private.png',
  );
});
