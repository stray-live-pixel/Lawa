<p align="center">
  <img src="assets/lawa-logo.png" width="220" alt="Логотип Lawa">
</p>

<h1 align="center">Lawa</h1>

<p align="center">
  <strong>Workflow-оркестратор параллельной работы AI-агентов через Codex App Server.</strong><br>
  Опишите DAG в JSON — Lawa запустит готовые ветки, дождётся зависимостей,
  сохранит состояние и предоставит единый интерфейс наблюдения.
</p>

## Что даёт Lawa

У каждого кубика workflow есть отдельный prompt, thread Codex и файл памяти.
Независимые кубики стартуют параллельно, а зависимые — только после успешного
завершения входов. После сигнала или сбоя run остаётся на диске и продолжается по
тому же ID.

Пользователю не нужно работать с протоколом Codex:

```text
lawa run / resume
        │
        ▼
  фасад Lawa
  ├── процессы codex app-server
  ├── thread и turn
  ├── зависимости и параллельные волны
  ├── meta.json и events.jsonl
  └── status / logs / dashboard
```

Для короткой линейной задачи Lawa обычно не нужна. Она полезна, когда есть
несколько независимых исследований или изменений и отдельный этап сборки результата.

## Единственный runtime: App Server

Lawa запускает официальный `codex app-server --stdio` напрямую. Отдельного
app-native режима, `--mode` и команд второго протокола нет. Один CLI одинаково
работает на личном macOS/Linux и на Linux-сервере. Установленный Codex Desktop не
меняет способ исполнения; нужен доступный и авторизованный Codex CLI.

Lawa сознательно не создаёт нативные задачи Codex Desktop. У Desktop нет публично
документированного программного API, через который внешний Go-процесс мог бы
создавать, продолжать и наблюдать такие задачи. Посредник в виде управляющего
агента добавлял бы модельные запросы, задержку, стоимость и узкое место при
параллельном запуске множества workflow. Поэтому наблюдаемость реализована самой
Lawa, а появление App Server thread в Desktop не гарантируется.

Полное решение и условие его пересмотра описаны в
[исследовании интеграции](docs/codex-integration.md). Основа протокола —
[официальная документация Codex App Server](https://learn.chatgpt.com/docs/app-server).

## Установка

Готовые бинарники поддерживают macOS и Linux на `amd64` и `arm64`.

```sh
curl -fsSL https://github.com/stray-live-pixel/Lawa/releases/latest/download/install.sh | sh
```

Если `curl` отсутствует:

```sh
wget -qO- https://github.com/stray-live-pixel/Lawa/releases/latest/download/install.sh | sh
```

Сначала показать план без изменений:

```sh
curl -fsSL https://github.com/stray-live-pixel/Lawa/releases/latest/download/install.sh |
  sh -s -- --plan
```

По умолчанию устанавливаются:

```text
бинарник: ~/.local/bin/lawa
скилл:    ~/.codex/skills/lawa/SKILL.md
```

Установщик отдельно спрашивает разрешение на изменение PATH и системную установку
PlantUML. Для неинтерактивного запуска:

```sh
sh install.sh --yes
sh install.sh --yes --install-plantuml
```

После установки проверьте окружение:

```sh
command -v lawa
command -v codex
lawa version
lawa help
codex --version
```

Codex CLI должен быть авторизован для App Server. На удалённом сервере авторизация,
`cwd`, workflow и root должны быть доступны тому же системному пользователю,
который запускает Lawa.

Обновление и удаление:

```sh
lawa update
sh install.sh --uninstall
```

PlantUML необязателен: без него workflow, Markdown-статус и observability работают,
но `workflow-status.png` не создаётся.

## Быстрый старт

Проверьте граф:

```sh
lawa validate examples/review.json
```

Запустите его. Для многострочного или shell-чувствительного текста используйте
файлы:

```sh
lawa run examples/review.json --cwd /absolute/project \
  --task-file /absolute/task.md \
  --comment-file /absolute/comment.md
```

Lawa напечатает `runId`. Команда остаётся в foreground и наблюдает workflow до
результата. Не отправляйте её в случайный фоновый shell: для долговечной работы
используйте supervisor, systemd или другой контролируемый процесс.

Для продолжения:

```sh
lawa resume <run-id>
```

`resume` сверяет сохранённые thread и продолжает только явно interrupted-кубики.
Успешные кубики не перезапускаются. Не создавайте новый run после неоднозначной
ошибки `thread/start`: thread мог быть создан, а повтор породит дубль.

## Observability

Наблюдение не запускает модель и не требует агента-посредника:

```sh
lawa status <run-id>
lawa logs <run-id>
lawa logs <run-id> <step-id>
lawa logs <run-id> <step-id> --follow
lawa serve
```

`status` показывает состояние кубиков, thread/turn, PID App Server, последнюю
активность и краткую ошибку. `logs` читает приватный нормализованный
`events.jsonl`. В него входят lifecycle процесса, thread/turn, безопасный тип
item, финальный agent message, ошибка и числовые счётчики token usage.

В журнал намеренно не копируются reasoning, аргументы и вывод команд, tool/MCP
payload, diff и сырые rollout Codex. Это одновременно делает интерфейс устойчивым
к расширению протокола и уменьшает риск утечки секретов.

`lawa serve` поднимает read-only dashboard на
[http://127.0.0.1:60800](http://127.0.0.1:60800). Он показывает дерево связанных
run, PID/turn/активность кубиков, память, безопасные события, тикеты, папку run и
UML. Dashboard не читает внутренние каталоги Codex и не отдаёт raw rollout.

Для удалённого сервера оставьте loopback и используйте SSH forwarding:

```sh
ssh -L 60800:127.0.0.1:60800 user@server
```

Явный `--listen` на внешнем интерфейсе разрешён, но Lawa печатает предупреждение:
в root находятся постановки и память агентов.

## Формат workflow

Минимальный граф:

```json
{
  "id": "review",
  "model": "gpt-5.4",
  "steps": [
    {
      "id": "code",
      "type": "analysis",
      "prompt": "Проверь архитектуру и сохрани выводы в память."
    },
    {
      "id": "tests",
      "type": "analysis",
      "prompt": "Проверь тесты и граничные случаи."
    },
    {
      "id": "summary",
      "type": "analysis",
      "prompt": "Собери общий отчёт по памяти зависимостей.",
      "depends_on": ["code", "tests"]
    }
  ]
}
```

Правила:

- `id` workflow и кубиков непустые и уникальные;
- `steps` непуст;
- `type` и `prompt` обязательны;
- `depends_on` ссылается только на существующие кубики;
- граф не содержит циклов;
- `model`, `effort` и `speed` можно переопределить на уровне кубика;
- окончательную совместимость model/effort/service tier проверяет App Server.

Независимые готовые кубики Lawa резервирует одной атомарной волной и запускает
параллельно. Каждый агент читает снимок run и память коллег, но пишет только
собственный файл.

## Повторяющиеся серии

Каждый повтор создаёт самостоятельный app-server run:

```sh
# Следующий run сразу после успешного предыдущего.
lawa run workflow.json <параметры> --repeat immediate --max-runs 10

# Первый сразу, следующие через час после завершения.
lawa run workflow.json <параметры> --repeat after --repeat-delay 1h

# По будням в 09:00 по Москве.
lawa run workflow.json <параметры> --repeat cron \
  --cron "0 9 * * 1-5" --timezone Europe/Moscow
```

Cron использует пять стандартных полей. Два run одной серии не исполняются
одновременно. После failed/interrupted действует stop-on-failure.

```sh
lawa series-status <series-id>
lawa series-stop <series-id>
```

`series-stop` запрещает будущие run и не прерывает текущий.

## Хранилище и восстановление

По умолчанию данные находятся в `~/.light-ai-workflows`:

```text
<run-id>/
├── workflow.json
├── task.md
├── meta.json
├── events.jsonl
├── workflow-status.md
├── workflow-status.puml
├── workflow-status.png
├── coordinator.lock
└── memory/
    └── <thread-id>.md

series/<series-id>/
├── series.json
├── coordinator.lock
├── launch.lock
└── stop
```

`meta.json` и отчёты публикуются атомарно, журнал синхронизируется после каждой
строки, а `flock` запрещает двум координаторам одновременно управлять одним run.
Thread ID сохраняется до `turn/start`, turn ID — до ожидания событий.

Новые run имеют формат v3 и означают единственный app-server runtime. Форматы v1/v2
старых app-server run продолжают читаться и возобновляться. Если в v2 найдены
защитные marker-файлы app-native, run или серия доступны только для чтения:
автоматическая миграция могла бы повторно создать уже работающую Desktop-задачу.

## Безопасность

- Lawa сохраняет sandbox и managed restrictions Codex.
- `approvalPolicy: on-request` не даёт Lawa молча повышать права.
- Служебные файлы run доступны кубикам только для чтения; писать можно в свою память.
- Не храните секреты в task, comment, prompt или памяти.
- Параллельные агенты могут логически конфликтовать в общих файлах проекта;
  выражайте порядок через `depends_on`.
- Lawa не оценивает смысл ответа: успех определяется терминальным статусом turn.
- Собственного глобального лимита параллельности и таймаута нет; действуют лимиты
  Codex и окружения. Размер готовой волны равен параллелизму workflow.

## CLI

```text
lawa run <workflow.json> --cwd <проект> (--task <текст> | --task-file <путь>)
lawa resume <run-id>
lawa status <run-id>
lawa logs <run-id> [step-id] [--follow]
lawa serve [--root <путь>] [--listen 127.0.0.1:60800]
lawa series-status <series-id>
lawa series-stop <series-id>
lawa validate <workflow.json>
lawa skill
lawa version
lawa update
lawa help
```

Полные параметры и коды выхода всегда доступны через `lawa help`.

## Разработка

Нужна версия Go из `go.mod`:

```sh
git clone https://github.com/stray-live-pixel/Lawa.git
cd Lawa
go build -o bin/lawa ./cmd/lawa
go test -race -count=1 ./...
go vet ./...
go mod tidy -diff
sh -n install.sh
```

Основные пакеты:

```text
cmd/lawa/             универсальный CLI и встроенный скилл
internal/workflow/    строгий JSON и проверка DAG
internal/scheduler/   готовые волны и зависимости
internal/coordinator/ orchestration и нормализация событий
internal/codex/       клиент официального App Server
internal/runstore/    атомарный state и безопасный журнал
internal/dashboard/   read-only observability UI
internal/statusreport/ Markdown и PlantUML
internal/series/      повторяющиеся app-server run
```

## Документация

- [Архитектурное решение и исследование Codex](docs/codex-integration.md)
- [План и состояние MVP](docs/mvp-roadmap.md)
- [Продуктовые требования](product/1.md)
- [Встроенный скилл](cmd/lawa/SKILL.md)
- [GitHub issue #57](https://github.com/stray-live-pixel/Lawa/issues/57)
