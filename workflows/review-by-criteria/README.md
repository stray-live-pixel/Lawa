# Ревью по критериям

[workflow.json](workflow.json) фиксирует снимок кода, распределяет ревью по
критериям и собирает общий отчёт. Использует личный скилл
`~/.codex/skills/review/SKILL.md`. Код не меняет и ревью в GitHub не публикует.

В постановке укажи конкретный PR, ветку, файл или локальные изменения:

```sh
lawa run workflows/review-by-criteria/workflow.json \
  --cwd /absolute/Lawa --task 'Проверь PR https://github.com/stray-live-pixel/Lawa/pull/123'
```

Перед запуском:

```sh
lawa validate workflows/review-by-criteria/workflow.json
```
