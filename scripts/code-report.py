#!/usr/bin/env python3
"""Считает собственный код Lawa через cloc, не меняя рабочую папку.

Git задаёт список неигнорируемых файлов, cloc отделяет комментарии от кода.
Включаются локальные изменения и новые файлы, поэтому коммитить перед запуском
не нужно. Границы приложения задаёт application_part; скрипт не считает себя.
"""

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]
PARTS = ("Backend (Go, включая CLI)", "UI (TS/JS, CSS, HTML)")
EXCLUDED_DIRS = {
    "test", "tests", "testdata", "__tests__", "__mocks__", "fixtures",
    "vendor", "node_modules", "dist", "coverage", "generated",
}
GENERATED = re.compile(r"Code generated[^\n]*DO NOT EDIT|@generated", re.I)


def application_part(path):
    """Возвращает часть приложения; тесты, сборки и утилиты не входят в выборку."""
    name = path.name
    if EXCLUDED_DIRS.intersection(path.parts):
        return None
    if (name.endswith("_test.go") or re.search(r"\.(test|spec|stories)\.", name)
            or name == "test-setup.ts" or re.search(r"\.(generated|gen|min)\.", name)):
        return None
    if path.suffix == ".go" and path.parts[0] in {"cmd", "internal", "assets"}:
        return PARTS[0]
    if path == Path("ui/index.html"):
        return PARTS[1]
    if path.parts[:2] == ("ui", "src") and path.suffix in {
        ".ts", ".tsx", ".js", ".jsx", ".css", ".html",
    }:
        return PARTS[1]
    return None


def source_files(root):
    """Выбирает существующие исходники из Git, включая новые, без внешних symlink."""
    output = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=root,
    )
    selected = {}
    for raw in sorted(set(output.split(b"\0")) - {b""}):
        name = os.fsdecode(raw)
        part = application_part(Path(name))
        path = root / name
        if part is None or not path.is_file() or path.is_symlink():
            continue
        if root.resolve() not in path.resolve().parents:
            continue
        if "\n" in name or "\r" in name:
            raise ValueError("cloc не поддерживает переносы строки в имени: " + repr(name))
        # Маркер генератора ожидается в заголовке, а не в строковом литерале в теле.
        header = "\n".join(path.read_text(encoding="utf-8").splitlines()[:10])
        if not GENERATED.search(header):
            selected[name] = part
    return selected


def count_files(root, selected):
    """Вызывает cloc один раз; неполный отчёт считается ошибкой, а не нулём строк."""
    if not selected:
        raise ValueError("Не найдены исходники приложения в cmd/, internal/, assets/ и ui/.")
    cloc = shutil.which("cloc")
    if cloc is None:
        raise ValueError("Нужен cloc. Установите: brew install cloc (macOS) "
                         "или sudo apt install cloc (Debian/Ubuntu).")
    # Свои настройки cloc и дедупликация не должны менять смысл подсчёта:
    # каждый файл приложения учитывается, даже если его содержимое повторяется.
    result = subprocess.run(
        [cloc, "--config=" + os.devnull, "--list-file=-", "--json", "--by-file",
         "--skip-uniqueness", "--hide-rate", "--timeout=0"],
        input="\n".join(selected) + "\n", text=True, capture_output=True,
        cwd=root, check=True,
    )
    report = json.loads(result.stdout)
    rows = []
    for name, part in selected.items():
        counts = report.get(name)
        if counts is None:
            # cloc пропускает пустые файлы. Любой другой пропуск требует внимания.
            if (root / name).stat().st_size:
                raise ValueError("cloc не посчитал файл: " + name)
            counts = dict(code=0, comment=0, blank=0)
        rows.append(dict(path=name, part=part, **{
            key: counts[key] for key in ("code", "comment", "blank")
        }))
    return rows


def scale(code):
    """Условная шкала объёма для сравнения запусков, не отраслевой стандарт."""
    if code < 10_000:
        return "небольшой"
    if code < 50_000:
        return "умеренный"
    return "крупный"


def print_report(rows):
    """Показывает объём и концентрацию кода без выводов о качестве и сложности."""
    def number(value):
        """Разделяет тысячи пробелом для быстрого чтения."""
        return f"{value:,}".replace(",", " ")

    print("Код приложения — текущие локальные файлы\n")
    print(f"{'Часть':<30} {'Файлов':>7} {'Код':>9} {'Комментарии':>12} {'Пустые':>9}")
    for part in (*PARTS, "Всего"):
        items = [r for r in rows if part == "Всего" or r["part"] == part]
        values = [len(items)] + [sum(r[k] for r in items) for k in ("code", "comment", "blank")]
        print(f"{part:<30} {number(values[0]):>7} {number(values[1]):>9} "
              f"{number(values[2]):>12} {number(values[3]):>9}")
    total = sum(r["code"] for r in rows)
    large = sum(r["code"] >= 500 for r in rows)
    print(f"\nМасштаб: {scale(total)} — {number(total)} строк кода.")
    print("Условная шкала: <10 тыс. — небольшой; <50 тыс. — умеренный; от 50 тыс. — крупный.")
    print(f"Файлов от 500 строк кода: {large}. Это ориентир для обзора, не признак дефекта.")
    print("\nСамые крупные файлы (строки кода):")
    for row in sorted(rows, key=lambda r: (-r["code"], r["path"]))[:5]:
        print(f"  {number(row['code']):>7}  {row['path']}")
    print("\nСложность: по объёму не определяется. Нужен анализ поведения, зависимостей,")
    print("параллельного выполнения, сохранения состояния и восстановления после сбоев.")
    print("\nКод — физические строки по cloc без пустых строк и строк только с комментариями.")
    print("Учтены cmd/, internal/, assets/ (*.go), ui/src/ и ui/index.html.")
    print("Исключены тесты, зависимости, скрипты, конфиги, документация, лендинг,")
    print("изображения, сборки UI и исходники с маркерами генерации.")


def main():
    """Запускается из любой папки; --json выводит измерения по каждому файлу."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--json", action="store_true", help="вывести строки по файлам в JSON")
    args = parser.parse_args()
    try:
        rows = count_files(ROOT, source_files(ROOT))
        if args.json:
            print(json.dumps(rows, ensure_ascii=False, indent=2))
        else:
            print_report(rows)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print("Ошибка анализа: " + str(error), file=sys.stderr)
        if isinstance(error, subprocess.CalledProcessError) and error.stderr:
            print(error.stderr.strip(), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
