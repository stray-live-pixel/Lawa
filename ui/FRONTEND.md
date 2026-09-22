# Карта frontend Lawa

Краткая карта точек входа и связей. Пути относительны корню репозитория.
Запуск, сборка и подробные соглашения описаны в [README](README.md); версии — в [package.json](package.json).
Figma-компоненты, токены и их соответствие коду — в [карте дизайн-системы](DESIGN_SYSTEM.md).

## Стек и точки входа

React + TypeScript, Vite; Gravity UI и Gravity icons. Схема — React Flow (`@xyflow/react`) и Dagre. Markdown — `react-markdown` и GFM. Тесты — Vitest, Testing Library, jsdom. Версии и команды смотри в `ui/package.json`.

| Область | Исходники |
|---|---|
| Инициализация, маршруты, dashboard | `ui/src/main.tsx`, `ui/src/App.tsx` |
| Определение workflow (`/view`) | `ui/src/pages/WorkflowDefinition.tsx`, `ui/src/components/DefinitionDetails.tsx` |
| Граф и геометрия | `ui/src/components/WorkflowGraph.tsx`, `graphLayout.ts`, `graphRouting.ts`, `graphDrawing.ts`, `graphLabel.ts`, `graphModel.ts`, `displayVisit.ts` в той же папке |
| Дерево, фильтры, детали запуска | `ui/src/components/Tree.tsx`, `DashboardFilters.tsx`, `RunInfo.tsx` |
| Общие контролы, темы и компоновка | `ui/src/components/ui.tsx`, `Theme.tsx`, `ui/src/styles.css` |
| Размеры панелей | `ui/src/components/ResizableRunList.tsx` |
| Markdown, копирование, исходники | `ui/src/components/MarkdownDocument.tsx`, `CopyIdentity.tsx`, `WorkflowSource.tsx` |
| Офис | `ui/src/pages/Office.tsx`, `officeLayout.ts`, `office.css` |
| Причины ожидания: подпись, длительность, происхождение; мобильный список вне камеры в `Office.tsx` | `ui/src/components/teamWait.ts`, поля `wait` в `TeamPhone.tsx`; время берётся из курсора плеера |
| Сводки, единый архив и переход к источникам; граница только из видимого префикса истории | `ui/src/components/TeamConversation.tsx`; `TeamPhone.tsx` сохраняет ориентир прокрутки |
| Карта офиса, чат, воспроизведение | `ui/src/components/OfficeMap.tsx`, `TeamPhone.tsx`, `TeamPlayer.tsx` и их CSS |
| API и типы | `ui/src/hooks/api.ts`, `ui/src/types.ts` |

Соседние `*.test.ts(x)` показывают проверяемые сценарии. `/preview` даёт демонстрационные данные dashboard, но не заменяет fixtures конкретного экрана.

## Важные условия

- `Theme.tsx` задаёт русский язык Gravity и системную/светлую/тёмную тему. Тема хранится в `lawa-theme`; сбой localStorage не должен ломать UI.
- `styles.css` содержит общие `--panel-toolbar-height`, `--panel-inset` и алиасы цветов Gravity. Глобальные заголовки и классы используются несколькими экранами.
- `ResizableRunList` обслуживает обе панели и сохраняет их ширину независимо; есть pointer- и клавиатурное управление. Не делай отдельную несовместимую реализацию для нового экрана.
- `graphModel.ts` добавляет только визуальные маркеры и проверяет сохранённую причину посещения. `graphLayout.ts` и `graphDense.ts` размещают узлы и порты по [геометрическому контракту](GRAPH_DESIGN.md), `graphRouting.ts` обходит препятствия, `graphDrawing.ts` объединяет общие стволы и обрезает их у непрозрачного наконечника. `graphLabel.ts` измеряет и сокращает подписи до раскладки.
- Геометрия зависит от топологии и текста, а не polling, статусов и выбранного посещения. Выбор связывает статус, причину, результат и сообщения одного visit; `Attempt` — отдельный технический счётчик. Старые исполнения без структурированного `Cause` не получают выдуманную подсветку.
- В `/view` выбор шага связан с URL. Back/forward должен восстанавливать выбор. Определение не содержит реальных runtime-статусов и посещений.
- `usePoll` отменяет запросы и защищается от устаревших ответов. Не добавляй конкурирующий polling; скрытая тяжёлая панель не должна начинать ненужные загрузки.
- `MarkdownDocument` копирует исходный Markdown, имеет ручной fallback при отказе Clipboard API, не исполняет HTML и показывает изображения ссылками. Сохраняй эти свойства при упрощении интерфейса.
- URL ресурсов приходят из API. Не собирай файловые пути в UI. PascalCase DTO соответствует Go-контракту.

## Когда обновлять

При изменении UI проверь, затронуты ли точки входа, ответственность компонентов,
связи между ними или перечисленные условия. Если да — обнови карту вместе с кодом.
Обычная правка подписи или отступа без структурных изменений обновления не требует.

Сохраняй карту короткой: основные области, ключевые файлы и неочевидные связи.
Не перечисляй все компоненты, функции и CSS-классы, не веди историю изменений.
Детали оставляй рядом с кодом или в тематической документации со ссылкой отсюда;
не дублируй README. При добавлении сведений убирай устаревшее и повторы.
