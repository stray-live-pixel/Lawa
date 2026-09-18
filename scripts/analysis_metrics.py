"""Преобразует отчёты закреплённых версий инструментов в проверяемые метрики.

Отсутствующие обязательные поля — ошибка формата, а не отсутствие замечаний.
Пороги относятся к выбранной политике проекта и не доказывают качество продукта.
"""

from collections import Counter
import json
import math
import re


def go_lint(data):
    """Отделяет сложность от остальных замечаний; сохраняет распределение правил."""
    issues = data["Issues"] or []
    if data.get("Report", {}).get("Error"):
        raise ValueError(str(data["Report"]["Error"]))
    counts = Counter(issue["FromLinter"] for issue in issues)
    if counts["typecheck"]:
        raise ValueError("Go не удалось загрузить/проверить: см. исходный отчёт")
    complexity = counts["gocognit"] + counts["gocyclo"]
    return {"go_issues": len(issues) - complexity, "go_complexity": complexity}, dict(counts)


def ui_lint(data):
    """Одна функция может дать несколько сообщений; сложность отделена от warning."""
    if not isinstance(data, list) or not data:
        raise ValueError("ESLint не вернул результаты по файлам")
    if any(file["fatalErrorCount"] for file in data):
        raise ValueError("ESLint не смог разобрать исходники: см. исходный отчёт")
    messages = [message for file in data for message in file["messages"]]
    complexity = sum(m["ruleId"] == "complexity" for m in messages)
    return {
        "ui_errors": sum(m["severity"] == 2 for m in messages),
        "ui_warnings": sum(m["severity"] == 1 and m["ruleId"] != "complexity" for m in messages),
        "ui_complexity": complexity,
    }, dict(Counter(m["ruleId"] for m in messages))


def unused_code(data):
    """Считает элементы замечаний Knip, а не только файлы с замечаниями."""
    counts = Counter()
    for issue in data["issues"]:
        for category, entries in issue.items():
            if category not in {"file", "owners"}:
                if not isinstance(entries, list):
                    raise ValueError("Неизвестный формат Knip: " + category)
                counts[category] += len(entries)
    return {"unused": sum(counts.values())}, dict(counts)


def duplication(data):
    """Использует знаменатель jscpd: он отличается от строк кода cloc."""
    total = data["statistics"]["total"]
    if total["lines"] <= 0:
        raise ValueError("jscpd не проанализировал строки")
    return {"duplication": total["percentage"]}, total


def dependencies(data):
    """Считает нарушения правил графа, включая циклы и неразрешимые импорты."""
    if not data["modules"]:
        raise ValueError("dependency-cruiser не проанализировал модули")
    violations = data["summary"]["violations"]
    return {"dependency_violations": len(violations)}, dict(
        Counter(v["rule"]["name"] for v in violations)
    )


def json_stream(text):
    """govulncheck выдаёт последовательность многострочных JSON-объектов."""
    decoder = json.JSONDecoder()
    while text.strip():
        text = text.lstrip()
        value, end = decoder.raw_decode(text)
        yield value
        text = text[end:]


def go_security(text):
    """Считает уникальные OSV с вызываемым символом, а не число путей вызова."""
    messages = list(json_stream(text))
    configs = [m["config"] for m in messages if "config" in m]
    if len(configs) != 1 or configs[0]["scan_level"] != "symbol":
        raise ValueError("govulncheck не выполнил анализ на уровне символов")
    if not any("SBOM" in m or "sbom" in m for m in messages):
        raise ValueError("govulncheck не вернул состав проверенных модулей")
    findings = [m["finding"] for m in messages if "finding" in m]
    called = {f["osv"] for f in findings if f["trace"][0].get("function")}
    return {"go_vulnerabilities": len(called)}, {
        "called": sorted(called), "database": configs[0],
        "all_reported": sorted({f["osv"] for f in findings}),
    }


def npm_security(data):
    """Аудит lock-файла приложения, включая devDependencies для сборки и тестов."""
    if "error" in data:
        raise ValueError("npm audit: " + str(data["error"]))
    counts = data["metadata"]["vulnerabilities"]
    return {
        "npm_high": counts["high"] + counts["critical"],
        "npm_other": counts["info"] + counts["low"] + counts["moderate"],
    }, counts


def typecheck(text, returncode):
    """Не принимает аварийный выход tsc без диагностики TypeScript за успех."""
    errors = len(re.findall(r"\berror TS\d+:", text))
    if returncode and not errors:
        raise ValueError("tsc завершился без распознаваемой диагностики")
    return {"type_errors": errors}, {}


def evaluate(value, rule):
    """Порог включителен: 5% разрешено при warn_above=5, больше — WARN."""
    if not isinstance(value, (int, float)) or not math.isfinite(value) or value < 0:
        raise ValueError("Метрика должна быть конечным неотрицательным числом")
    for status, key in (("FAIL", "fail_above"), ("WARN", "warn_above")):
        if key in rule and value > rule[key]:
            return status
    return "PASS"


def overall(metrics, errors):
    """Неполный анализ приоритетнее замечаний: ERROR никогда не становится PASS."""
    if errors:
        return "ERROR"
    for status in ("FAIL", "WARN"):
        if any(metric["status"] == status for metric in metrics.values()):
            return status
    return "PASS"


def compare(current, previous):
    """Сравнивает только полные отчёты с той же методикой и набором инструментов."""
    if previous["schema_version"] != current["schema_version"]:
        raise ValueError("Отчёты имеют разные схемы")
    if previous["method"] != current["method"]:
        raise ValueError("Методика/версии инструментов изменились; сравнение некорректно")
    if previous["errors"] or current["errors"]:
        raise ValueError("Нельзя сравнивать неполные отчёты")
    if previous["metrics"].keys() != current["metrics"].keys():
        raise ValueError("Отчёты содержат разные метрики")
    return {key: item["value"] - previous["metrics"][key]["value"]
            for key, item in current["metrics"].items()}
