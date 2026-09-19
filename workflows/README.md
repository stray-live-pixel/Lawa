# Workflow разработки Lawa

Каждый рабочий workflow хранится в своей папке: `workflow.json`, инструкция
`README.md` и локальные `prompts/` или `templates/`, если они нужны.
Демонстрационные примеры возможностей Lawa остаются в `../examples`.

| Workflow | Назначение |
| --- | --- |
| [pr-cycle](pr-cycle/README.md) | Задача → PR → ревью и исправления до APPROVE, без слияния |
| [development-cycle](development-cycle/README.md) | Разработка, техническое ревью, frontend QA и продуктовый QA |
| [review-by-criteria](review-by-criteria/README.md) | Подробное read-only ревью по отдельным критериям |

Пути в `prompt.file` и шаблонах считаются от `workflow.json`, а `--cwd` задаёт
проект, над которым работают агенты. После переноса обнови команды новых запусков.
Уже созданные запуски сохраняют снимок инструкций и не требуют переноса данных.
