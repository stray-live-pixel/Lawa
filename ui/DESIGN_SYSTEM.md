# Карта дизайн-системы Lawa UI

[Библиотека в Figma](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=0-1)
отражает общий интерфейс dashboard, workflow и офиса на Gravity UI.
Используй экземпляры её компонентов для макетов, `@gravity-ui/uikit`,
`@gravity-ui/icons` и существующие обёртки Lawa — для реализации.
Карта frontend и поведение приложения описаны в [FRONTEND.md](FRONTEND.md).

## Где что находится

| Страницы Figma | Назначение и связь с кодом |
|---|---|
| [00 · Lawa UI](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=0-1), [01 · Начало работы](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-176) | Обзор, правила использования и ограничения доступности |
| [02 · Цвет](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-177), [03 · Типографика и размеры](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-178) | Токены Gravity, Inter, размеры; [styles.css](src/styles.css), [Theme.tsx](src/components/Theme.tsx) |
| [10 · Button](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-180), [11 · TextInput](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-181), [12 · Choice](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-182) | Контролы Gravity; Button и Choice обёрнуты в [ui.tsx](src/components/ui.tsx) |
| [13 · Status](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-183), [18 · Dialog](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-188), [19 · ErrorNotice](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-189) | Статусы, диалог и ошибка из [ui.tsx](src/components/ui.tsx) |
| [14 · Tabs](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-184), [15 · RunRow](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-185) | Gravity Tabs в [App.tsx](src/App.tsx); строки дерева в [Tree.tsx](src/components/Tree.tsx) |
| [16 · WorkflowNode](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-186), [17 · PanelToolbar](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-187) | Cube из [WorkflowGraph.tsx](src/components/WorkflowGraph.tsx); PanelToolbar — шаблон компоновки из [styles.css](src/styles.css), отдельного React-компонента нет |
| [20 · Icons](https://www.figma.com/design/Ohltjk2saaQi4jyueGqUzq/Main?node-id=5-191) | Исходные SVG Gravity; заменяемые иконки в свойствах компонентов |

## Токены и оформление библиотеки

- `Lawa · Primitives` — исходные значения цветов; `Lawa · Color` — семантические
  ссылки с режимами Light/Dark; `Lawa · Layout` — отступы, радиусы и размеры.
  У переменных указан CSS-синтаксис; текстовые стили имеют префикс `Lawa/`.
- Тему образцов задавай на родительской подложке через `Lawa · Color`.
  В приложении темы Light/Dark/System обеспечивает [Theme.tsx](src/components/Theme.tsx).
- Фон всех страниц Figma — `#1E1E1E`. Образцы Light/Dark и главные компоненты
  размещай на отдельных подложках. Пары выравнивай по верхнему краю;
  между подложками и между шапкой и образцами оставляй 80 px.
  Это оформление документации в Figma, а не требование тёмной темы приложения.
- Code Connect не подключён: связь с реализацией указана в описаниях компонентов
  и этой карте. Варианты Figma описывают внешний вид; ввод, popup, клавиатуру
  и управление фокусом реализуют Gravity UI и код Lawa.

## Поддержание актуальности

При изменении компонента или токена синхронизируй код, Figma-варианты, привязки
переменных и описание. Проверяй обе темы, значимые состояния и длинный текст.
Существующие компоненты обновляй, сохраняя связи экземпляров, вместо создания дублей.
При расхождении выясни актуальное поведение по задаче и исходникам; не подменяй
его приближённым макетом. Недоступность Figma и оставшиеся расхождения укажи в итоге.

Обновляй эту карту при изменении страниц, коллекций или соответствия компонентов
коду. Сохраняй только ориентиры и ссылки, без истории правок и полного каталога вариантов.
