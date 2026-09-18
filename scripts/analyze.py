#!/usr/bin/env python3
"""Запускает статический анализ Lawa и сохраняет Markdown, JSON и сырые отчёты.

Обычный запуск ничего не исправляет. --install устанавливает закреплённые
инструменты перед анализом. Код выхода: 0 — полный отчёт, 1 — FAIL с --check,
2 — неполный анализ/ошибка. WARN остаётся рекомендацией при любом режиме.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import subprocess
import sys

sys.dont_write_bytecode = True
import analysis_metrics as metrics


ROOT = Path(__file__).resolve().parents[1]
CONFIG = ROOT / "tools/analysis"
BIN = ROOT / "bin/analysis"
NODE_BIN = CONFIG / "node_modules/.bin"
SPEC = importlib.util.spec_from_file_location("code_report", ROOT / "scripts/code-report.py")
CODE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CODE)


def read_json(path):
    """Читает UTF-8 JSON; повреждённый отчёт не заменяется пустым объектом."""
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path, value):
    """Записывает один артефакт в уникальную папку текущего запуска."""
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def install(policy):
    """Ставит инструменты в проект; go.mod и зависимости приложения не обновляет."""
    BIN.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, GOBIN=str(BIN))
    for target in policy["go_tools"].values():
        subprocess.run(["go", "install", target], cwd=ROOT, env=env, check=True)
    subprocess.run(["npm", "ci", "--prefix", str(CONFIG), "--ignore-scripts",
                    "--no-audit", "--no-fund"], cwd=ROOT, check=True)
    # UI нужен для разрешения импортов и проверки реальным компилятором проекта.
    subprocess.run(["npm", "ci", "--prefix", str(ROOT / "ui"), "--ignore-scripts",
                    "--no-audit", "--no-fund"], cwd=ROOT, check=True)


def digest(paths):
    """Хеширует имена и содержимое, чтобы заметить изменение состава и методики."""
    result = hashlib.sha256()
    for path in sorted(paths):
        result.update(str(path.relative_to(ROOT)).encode() + b"\0")
        result.update(path.read_bytes() + b"\0")
    return result.hexdigest()


def command_text(command):
    """Короткие сведения о среде; отсутствие команды не выдаётся за версию."""
    return subprocess.check_output(command, cwd=ROOT, text=True, stderr=subprocess.PIPE).strip()


def method(policy):
    """Проверяет версии установленных инструментов и фиксирует методику сравнения."""
    versions = {}
    for name, target in policy["go_tools"].items():
        version = target.rsplit("@", 1)[1]
        build = command_text(["go", "version", "-m", str(BIN / name)])
        if not any(line.strip().startswith("mod\t") and line.split()[2] == version
                   for line in build.splitlines()):
            raise ValueError(f"Нужен {name} {version}; запустите --install")
        versions[name] = version
    for name, expected in read_json(CONFIG / "package.json")["devDependencies"].items():
        actual = read_json(CONFIG / "node_modules" / name / "package.json")["version"]
        if actual != expected:
            raise ValueError(f"Нужен {name} {expected}, установлен {actual}; запустите --install")
        versions[name] = actual
    compiler = read_json(ROOT / "ui/node_modules/typescript/package.json")["version"]
    locked_compiler = read_json(ROOT / "ui/package-lock.json")["packages"]["node_modules/typescript"]["version"]
    if compiler != locked_compiler:
        raise ValueError("Компилятор UI не соответствует lock-файлу; запустите --install")
    versions["ui_typescript"] = compiler
    versions.update(go=command_text(["go", "version"]), node=command_text(["node", "--version"]),
                    npm=command_text(["npm", "--version"]), cloc=command_text(["cloc", "--version"]))
    files = [p for p in CONFIG.iterdir() if p.is_file()]
    files += [Path(__file__), ROOT / "scripts/analysis_metrics.py", ROOT / "scripts/code-report.py"]
    target = json.loads(command_text(["go", "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS"]))
    return {"config_sha256": digest(files), "versions": versions,
            "platform": platform.system(), "go_target": target}


def source_digest(selected):
    """Защищает отчёт от редактирования кода и lock-файлов во время проверки."""
    paths = [ROOT / name for name in selected]
    paths += [ROOT / name for name in (
        "go.mod", "go.sum", "ui/package.json", "ui/package-lock.json", "ui/tsconfig.json", "ui/vite.config.ts",
    )]
    # Тесты исключены из метрик размера, но входят в запуск tsc через tsconfig.
    paths += [p for p in (ROOT / "ui/src").rglob("*") if p.suffix in {".ts", ".tsx"}]
    return digest(set(paths))


def run_tool(report, name, command, cwd, parser, output=None, accepted=(0, 1), timeout=600):
    """Сохраняет команду и логи; сбой одной проверки не мешает получить остальные.

    Код 1 принимается только для инструментов, где он означает найденные замечания.
    Даже тогда обязательный машиночитаемый результат должен успешно разобрать parser.
    """
    print(f"Проверка: {name}", flush=True)
    raw = Path(report["directory"]) / "raw"
    entry = {"command": [str(x) for x in command], "cwd": str(cwd)}
    report["tools"][name] = entry
    try:
        result = subprocess.run(entry["command"], cwd=cwd, text=True, capture_output=True,
                                timeout=timeout, env=dict(os.environ, NO_COLOR="1"))
        (raw / f"{name}.stdout").write_text(result.stdout, encoding="utf-8")
        (raw / f"{name}.stderr").write_text(result.stderr, encoding="utf-8")
        entry["exit_code"] = result.returncode
        if result.returncode not in accepted:
            raise ValueError(f"код выхода {result.returncode}; см. raw/{name}.stderr")
        content = output.read_text(encoding="utf-8") if output else result.stdout
        values, details = parser(content, result.returncode)
        if result.returncode and not any(values.values()):
            raise ValueError("Ненулевой код выхода без замечаний: результат нельзя считать полным")
        measured = {}
        for key, value in values.items():
            rule = report["policy"]["metrics"][key]
            measured[key] = {"value": value, "status": metrics.evaluate(value, rule), "tool": name}
        report["metrics"].update(measured)
        entry.update(status="complete", details=details)
    except (OSError, ValueError, LookupError, TypeError, AttributeError, subprocess.SubprocessError) as error:
        # При тайм-ауте сохраняем и частичную диагностику, если она была получена.
        for suffix, attribute in (("stdout", "stdout"), ("stderr", "stderr")):
            path = raw / f"{name}.{suffix}"
            if not path.exists():
                partial = getattr(error, attribute, None) or ""
                if isinstance(partial, bytes):
                    partial = partial.decode("utf-8", errors="replace")
                path.write_text(partial, encoding="utf-8")
        entry.update(status="error", error=str(error))
        report["errors"].append(f"{name}: {error}")
        print(f"  Ошибка: {error}", flush=True)


def json_parser(parser):
    """Адаптирует JSON-парсер к общему контракту запуска инструмента."""
    return lambda text, code: parser(json.loads(text))


def duplication_command(selected, output):
    """Не даёт стандартным ограничениям jscpd скрыть самые крупные исходники."""
    max_lines = max(len((ROOT / name).read_text(encoding="utf-8").splitlines()) for name in selected) + 1
    max_bytes = max((ROOT / name).stat().st_size for name in selected) + 1
    return [NODE_BIN / "jscpd", "--min-lines", "10", "--min-tokens", "70",
            "--max-lines", str(max_lines), "--max-size", str(max_bytes),
            "--mode", "weak", "--reporters", "json", "--output", output, "--silent", *selected]


def analyze(report, selected):
    """Запускает проверки production-кода; audit проверяет и зависимости сборки."""
    raw = Path(report["directory"]) / "raw"
    run_tool(report, "golangci", [BIN / "golangci-lint", "run", "--config", CONFIG / "golangci.json",
             "--output.json.path", raw / "golangci.json", "--output.text.path", os.devnull,
             "./cmd/...", "./internal/...", "./assets/..."], ROOT,
             json_parser(metrics.go_lint), output=raw / "golangci.json")
    ui_files = [str(ROOT / name) for name, part in selected.items()
                if part == CODE.PARTS[1] and Path(name).suffix in {".ts", ".tsx"}]
    run_tool(report, "eslint", [NODE_BIN / "eslint", "--config", CONFIG / "eslint.config.mjs",
             "--format", "json", *ui_files], ROOT / "ui", json_parser(metrics.ui_lint))
    run_tool(report, "typecheck", [ROOT / "ui/node_modules/.bin/tsc", "--noEmit", "--pretty", "false"],
             ROOT / "ui", metrics.typecheck, accepted=(0, 1, 2))
    run_tool(report, "knip", [NODE_BIN / "knip", "--config", CONFIG / "knip.json",
             "--production", "--reporter", "json", "--no-progress"], ROOT / "ui",
             json_parser(metrics.unused_code))
    run_tool(report, "jscpd", duplication_command(selected, raw / "jscpd"),
             ROOT, json_parser(metrics.duplication), output=raw / "jscpd/jscpd-report.json", accepted=(0,))
    run_tool(report, "dependencies", [NODE_BIN / "depcruise", "--config", CONFIG / "dependencies.cjs",
             "--output-type", "json", "src/main.tsx"], ROOT / "ui", json_parser(metrics.dependencies))
    run_tool(report, "govulncheck", [BIN / "govulncheck", "-json", "./cmd/...", "./internal/...", "./assets/..."],
             ROOT, lambda text, code: metrics.go_security(text), accepted=(0,))
    run_tool(report, "npm-audit", ["npm", "audit", "--json", "--package-lock-only", "--ignore-scripts"],
             ROOT / "ui", json_parser(metrics.npm_security), timeout=180)


def metric_groups(report):
    """Единые разделы для Markdown и терминала; новые группы не теряются."""
    groups = {name: {} for name in ("GO", "UI", "NPM", "Общее")}
    for key, rule in report["policy"]["metrics"].items():
        groups.setdefault(rule.get("group", "Общее"), {})[key] = rule
    return {name: rules for name, rules in groups.items() if rules}


def group_status(report, rules):
    """Отсутствующая метрика даёт ERROR только своему разделу."""
    measured = {key: report["metrics"][key] for key in rules if key in report["metrics"]}
    return metrics.overall(measured, set(rules) - measured.keys())


def markdown(report):
    """Формирует читаемый отчёт; исходные диагностики остаются рядом в raw/."""
    lines = [f"# Анализ Lawa: {report['status']}", "", f"Дата: {report['created_at']}",
             f"Commit: `{report.get('commit', 'не определён')}`. Анализируются локальные файлы.", "",
             "PASS — пороги соблюдены; WARN — требуется внимание; FAIL — порог нарушен;",
             "ERROR — анализ неполный. Эти статусы не доказывают отсутствие дефектов.", ""]
    if report["errors"]:
        lines += ["## Ошибки анализа", ""] + [f"- {error}" for error in report["errors"]] + [""]
    for group, rules in metric_groups(report).items():
        lines += [f"## {group} — {group_status(report, rules)}", ""]
        part = {"GO": CODE.PARTS[0], "UI": CODE.PARTS[1]}.get(group)
        if part and "size" in report:
            rows = [r for r in report["size"] if r["part"] == part]
            lines += [f"Собственный код: **{sum(r['code'] for r in rows)} строк**, файлов: **{len(rows)}**.", ""]
        lines += ["| Метрика | Значение | Порог | Статус | Δ |", "|---|---:|---|---|---:|"]
        for key, rule in rules.items():
            item = report["metrics"].get(key, {})
            threshold = "; ".join(f"{status} >{rule[field]}" for status, field in
                                  (("WARN", "warn_above"), ("FAIL", "fail_above")) if field in rule)
            delta = report.get("delta", {}).get(key)
            delta_text = f"{delta:+g}" if delta is not None else "—"
            lines.append(f"| {rule['label']} | {item.get('value', '—')} | {threshold} | "
                         f"{item.get('status', 'ERROR')} | {delta_text} |")
        lines.append("")
    lines += ["", "## Диагностики", "", "Все команды, версии и распределения правил: [report.json](report.json).",
              "Исходные сообщения по файлам и строкам:", ""]
    for name, entry in report["tools"].items():
        lines.append(f"- {name}: [{entry['status']}](raw/{name}.stdout), [stderr](raw/{name}.stderr)")
    lines += ["- [Go: замечания по файлам](raw/golangci.json)",
              "- [Дубликаты: пары фрагментов](raw/jscpd/jscpd-report.json)", "",
              "Пороги — локальная политика проекта, а не универсальная шкала качества.",
              "Тесты, сторонний код и сборки исключены из метрик размера/сложности/дублирования.",
              "tsc проверяет весь tsconfig, включая тесты; npm audit включает зависимости сборки.",
              "Проверки безопасности используют актуальные базы: изменение числа находок не обязательно вызвано кодом.", ""]
    return "\n".join(lines)


def main():
    """Создаёт отдельный каталог на запуск; неполный отчёт всегда возвращает код 2."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--install", action="store_true", help="сначала установить закреплённые инструменты")
    parser.add_argument("--check", action="store_true", help="вернуть код 1 при статусе FAIL")
    parser.add_argument("--compare", type=Path, help="сравнить с сохранённым report.json")
    parser.add_argument("--output", type=Path, help="новая, ещё не существующая папка отчёта")
    args = parser.parse_args()
    now = datetime.now(timezone.utc)
    directory = args.output or ROOT / "reports/analysis" / now.strftime("%Y%m%dT%H%M%S%fZ")
    directory = directory.resolve()
    # Отчёты не перезаписываются: так прошлый успешный JSON не скрывает новый сбой.
    try:
        directory.mkdir(parents=True, exist_ok=False)
        (directory / "raw").mkdir()
    except OSError as error:
        print(f"Нельзя создать папку отчёта: {error}", file=sys.stderr)
        return 2
    report = {"schema_version": 1, "created_at": now.isoformat(), "directory": str(directory),
              "policy": {"metrics": {}}, "metrics": {}, "tools": {}, "errors": []}
    try:
        report["policy"] = read_json(CONFIG / "policy.json")
        if args.install:
            install(report["policy"])
        try:
            report["method"] = method(report["policy"])
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            raise ValueError(f"Проверка инструментов: {error}; проверьте зависимости или выполните --install") from error
        report["commit"] = command_text(["git", "rev-parse", "HEAD"])
        report["working_tree"] = command_text(["git", "status", "--short"])
        selected = CODE.source_files(ROOT)
        report["source_sha256"] = source_digest(selected)
        try:
            report["size"] = CODE.count_files(ROOT, selected)
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            report["errors"].append(f"cloc: {error}")
        analyze(report, selected)
        missing = report["policy"]["metrics"].keys() - report["metrics"].keys()
        if missing:
            report["errors"].append("Нет метрик: " + ", ".join(sorted(missing)))
        if source_digest(CODE.source_files(ROOT)) != report["source_sha256"]:
            report["errors"].append("Исходники изменились во время анализа; повторите запуск")
        if args.compare:
            report["delta"] = metrics.compare(report, read_json(args.compare))
            report["compared_with"] = str(args.compare.resolve())
    except (OSError, ValueError, LookupError, TypeError, AttributeError, subprocess.SubprocessError) as error:
        report["errors"].append(str(error))
    report["status"] = metrics.overall(report["metrics"], report["errors"])
    write_json(directory / "report.json", report)
    (directory / "report.md").write_text(markdown(report), encoding="utf-8")
    print(f"\nРезультат: {report['status']}")
    for group, rules in metric_groups(report).items():
        print(f"\n{group} — {group_status(report, rules)}")
        for key, rule in rules.items():
            item = report["metrics"].get(key, {})
            print(f"  {item.get('status', 'ERROR'):<5} {rule['label']}: {item.get('value', 'нет результата')}")
    print(f"Отчёт: {directory / 'report.md'}")
    for error in report["errors"]:
        print("Ошибка: " + error, file=sys.stderr)
    return 2 if report["errors"] else int(args.check and report["status"] == "FAIL")


if __name__ == "__main__":
    sys.exit(main())
