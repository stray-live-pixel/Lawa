import { useState } from 'react';
import { Icon, useThemeValue } from '@gravity-ui/uikit';
import { ArrowUpRightFromSquare, ArrowDownToLine } from '@gravity-ui/icons';
import { Button, Choice } from './ui';

// Экспорт запускается только по ссылке. Начальная палитра совпадает с UI,
// затем пользователь может выбрать её независимо от темы приложения.
export function ImageExport({ runID }: { runID: string }) {
  const uiTheme = useThemeValue();
  const [theme, setTheme] = useState(uiTheme === 'dark' ? 'dark' : 'light');
  const path = `/graph-image/${encodeURIComponent(runID)}?theme=${theme}`;
  return (
    <section aria-label="Экспорт графа" className="image-export">
      <Choice
        aria-label="Тема картинки"
        value={theme}
        onUpdate={setTheme}
        options={[
          { value: 'dark', content: 'Тёмная' },
          { value: 'light', content: 'Светлая' },
        ]}
      />
      <Button href={path} target="_blank" rel="noopener noreferrer">
        <Icon data={ArrowUpRightFromSquare} />
        Показать PNG
      </Button>
      <Button href={`${path}&download=1`}>
        <Icon data={ArrowDownToLine} />
        Скачать PNG
      </Button>
    </section>
  );
}
