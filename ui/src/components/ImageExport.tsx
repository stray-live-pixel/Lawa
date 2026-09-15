import { useState } from 'react';

// PNG создаётся только по явному действию. Обычный polling React-графа не
// запускает PlantUML. Ссылки используют нативный просмотр/скачивание браузера;
// ошибки renderer возвращает endpoint, не подменяя их сохранённой картинкой.
export function ImageExport({ runID }: { runID: string }) {
  const [theme, setTheme] = useState('dark');
  const path = `/graph-image/${encodeURIComponent(runID)}?theme=${theme}`;
  return (
    <section aria-label="Экспорт графа" className="actions">
      <select
        aria-label="Тема картинки"
        value={theme}
        onChange={(event) => setTheme(event.target.value)}
      >
        <option value="dark">Тёмная</option>
        <option value="light">Светлая</option>
      </select>
      <a
        className="button"
        href={path}
        target="_blank"
        rel="noopener noreferrer"
      >
        Показать PNG ↗
      </a>
      <a className="button" href={`${path}&download=1`}>
        Скачать PNG ↓
      </a>
    </section>
  );
}
