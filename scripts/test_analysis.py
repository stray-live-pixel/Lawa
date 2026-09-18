"""Проверяет честность статусов, единицы метрик и существенные настройки инструментов."""

import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import analysis_metrics as metrics
import analyze


class MetricsTest(unittest.TestCase):
    """Защищает от ложного PASS и несопоставимых чисел после смены методики."""

    def test_thresholds_and_error_priority(self):
        """Равенство порогу допустимо, неполный анализ приоритетнее всех оценок."""
        rule = {"warn_above": 5, "fail_above": 10}
        for value, status in ((5, "PASS"), (5.01, "WARN"), (10, "WARN"), (10.01, "FAIL")):
            self.assertEqual(metrics.evaluate(value, rule), status)
        for invalid in (-1, float("nan"), float("inf"), "0"):
            with self.assertRaises(ValueError):
                metrics.evaluate(invalid, rule)
        self.assertEqual(metrics.overall({"x": {"status": "FAIL"}}, ["сбой"]), "ERROR")

    def test_go_categories(self):
        """Сложность не смешивается с ошибками; загрузка пакетов обязательна."""
        data = {"Issues": [{"FromLinter": name} for name in ("gocognit", "gocyclo", "errcheck")]}
        self.assertEqual(metrics.go_lint(data)[0], {"go_issues": 1, "go_complexity": 2})
        with self.assertRaises(ValueError):
            metrics.go_lint({"Issues": [{"FromLinter": "typecheck"}]})

    def test_knip_v6_schema(self):
        """Knip 6 хранит files внутри issues: отсутствие верхнего files нормально."""
        self.assertEqual(metrics.unused_code({"issues": []})[0], {"unused": 0})
        data = {"issues": [{"file": "x.ts", "files": [{"name": "x.ts"}],
                            "exports": [{"name": "unused"}], "types": []}]}
        self.assertEqual(metrics.unused_code(data)[0], {"unused": 2})
        with self.assertRaises(KeyError):
            metrics.unused_code({})

    def test_go_security_reachability_and_deduplication(self):
        """Одна уязвимость с двумя путями — одна находка; модуль без вызова не считается."""
        finding = {"finding": {"osv": "GO-1", "trace": [{"function": "Unsafe"}]}}
        stream = [{"config": {"scan_level": "symbol"}}, {"SBOM": {}}, finding, finding,
                  {"finding": {"osv": "GO-2", "trace": [{"module": "example"}]}}]
        result, _ = metrics.go_security("\n".join(json.dumps(m, indent=2) for m in stream))
        self.assertEqual(result, {"go_vulnerabilities": 1})
        with self.assertRaises(ValueError):
            metrics.go_security('{"config":{"scan_level":"symbol"}}')

    def test_invalid_results_are_not_zero(self):
        """Ошибку API, пустой граф и падение tsc нельзя превратить в успешный ноль."""
        for parser, value in ((metrics.ui_lint, []), (metrics.npm_security, {"error": "network"}),
                              (metrics.dependencies, {"modules": [], "summary": {"violations": []}}),
                              (metrics.duplication, {"statistics": {"total": {"lines": 0}}})):
            with self.assertRaises(ValueError):
                parser(value)
        with self.assertRaises(ValueError):
            metrics.typecheck("process crashed", 2)
        self.assertEqual(metrics.typecheck("a.ts(1,2): error TS1234: bad", 2)[0], {"type_errors": 1})

    def test_comparison_requires_same_method(self):
        """Смена версий или неполный отчёт запрещают сравнение, но FAIL сравнивать можно."""
        previous = {"schema_version": 1, "method": {"version": 1}, "errors": [],
                    "metrics": {"issues": {"value": 4, "status": "FAIL"}}}
        current = copy.deepcopy(previous)
        current["metrics"]["issues"]["value"] = 2
        self.assertEqual(metrics.compare(current, previous), {"issues": -2})
        current["method"]["version"] = 2
        with self.assertRaises(ValueError):
            metrics.compare(current, previous)

    def test_runner_records_process_failure(self):
        """При аварии процесса оставляем логи и ERROR, даже если stdout похож на JSON."""
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "raw").mkdir()
            report = {"directory": temporary, "policy": {"metrics": {}},
                      "tools": {}, "metrics": {}, "errors": []}
            analyze.run_tool(report, "broken", [sys.executable, "-c", 'print("{}"); raise SystemExit(2)'],
                             root, lambda text, code: ({}, {}))
            self.assertEqual(report["tools"]["broken"]["status"], "error")
            self.assertEqual(report["metrics"], {})
            self.assertEqual((root / "raw/broken.stdout").read_text().strip(), "{}")

    def test_runner_rejects_failure_without_findings(self):
        """Даже допустимый код 1 должен подтверждаться находками в отчёте."""
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "raw").mkdir()
            report = {"directory": temporary, "policy": {"metrics": {"unused": {"warn_above": 0}}},
                      "tools": {}, "metrics": {}, "errors": []}
            analyze.run_tool(report, "empty-failure", [sys.executable, "-c", 'raise SystemExit(1)'],
                             root, lambda text, code: ({"unused": 0}, {}))
            self.assertEqual(report["tools"]["empty-failure"]["status"], "error")
            self.assertEqual(report["metrics"], {})

    def test_cli_preserves_existing_report(self):
        """Повторное указание каталога не должно стирать прошлые результаты."""
        with tempfile.TemporaryDirectory() as temporary:
            marker = Path(temporary) / "report.json"
            marker.write_text("previous report")
            with patch.object(sys, "argv", ["analyze.py", "--output", temporary]):
                self.assertEqual(analyze.main(), 2)
            self.assertEqual(marker.read_text(), "previous report")

    def test_cli_saves_incomplete_report(self):
        """Отсутствующий инструмент даёт сохранённый ERROR, не обрыв без результата."""
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "report"
            with patch.object(sys, "argv", ["analyze.py", "--output", str(output)]):
                with patch.object(analyze, "method", side_effect=ValueError("missing tool")):
                    self.assertEqual(analyze.main(), 2)
            report = json.loads((output / "report.json").read_text())
            self.assertEqual(report["status"], "ERROR")
            self.assertEqual(report["metrics"], {})
            self.assertTrue((output / "report.md").is_file())


class ToolConfigurationTest(unittest.TestCase):
    """Реальные утилиты на фикстурах ловят ошибки конфигурации, незаметные в JSON-парсерах."""

    def test_knip_production_entry(self):
        """Продакшен-вход должен вести к используемому модулю; тест не делает код используемым."""
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "src").mkdir()
            (root / "package.json").write_text('{"name":"fixture","private":true}')
            (root / "src/main.tsx").write_text('import { used } from "./used"; console.log(used);')
            (root / "src/used.ts").write_text('export const used = 1;')
            (root / "src/unused.ts").write_text('export const unused = 2;')
            (root / "src/unused.test.ts").write_text('import { unused } from "./unused"; console.log(unused);')
            result = subprocess.run([str(analyze.NODE_BIN / "knip"), "--config", str(analyze.CONFIG / "knip.json"),
                                     "--production", "--reporter", "json", "--no-progress"],
                                    cwd=root, capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, result.stderr)
            data = json.loads(result.stdout)
            files = [issue["file"] for issue in data["issues"] if issue.get("files")]
            self.assertEqual(files, ["src/unused.ts"])

    def test_jscpd_includes_large_sources(self):
        """Файл больше 1000 строк не должен исчезать из статистики дублирования."""
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            # Все строки разные, чтобы повторяемость теста не зависела от размера
            # буфера перекрывающихся совпадений детектора.
            content = "package fixture\n" + "\n".join(f"var item{i} = {i}" for i in range(1200)) + "\n"
            for name in ("first.go", "second.go"):
                (root / name).write_text(content)
            output = root / "report"
            with patch.object(analyze, "ROOT", root):
                command = analyze.duplication_command(["first.go", "second.go"], output)
            subprocess.run([str(x) for x in command], cwd=root, capture_output=True, check=True)
            data = json.loads((output / "jscpd-report.json").read_text())
            self.assertEqual(data["statistics"]["formats"]["go"]["total"]["sources"], 2)
            self.assertGreater(metrics.duplication(data)[0]["duplication"], 0)

    def test_dependency_exports_and_cycles(self):
        """Разрешаем subpath exports как Vite, но замечаем настоящий цикл исходников."""
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "src").mkdir()
            package = root / "node_modules/example"
            package.mkdir(parents=True)
            (package / "package.json").write_text('{"name":"example","exports":{"./feature":"./actual.js"}}')
            (package / "actual.js").write_text('export const value = 1;')
            (root / "package.json").write_text('{"name":"fixture","dependencies":{"example":"1.0.0"}}')
            (root / "tsconfig.json").write_text('{"compilerOptions":{"moduleResolution":"bundler","module":"esnext"}}')
            (root / "src/main.tsx").write_text('import { value } from "example/feature"; import "./cycle"; console.log(value);')
            (root / "src/cycle.ts").write_text('import "./main";')
            result = subprocess.run([str(analyze.NODE_BIN / "depcruise"), "--config",
                                     str(analyze.CONFIG / "dependencies.cjs"), "--output-type", "json", "src/main.tsx"],
                                    cwd=root, capture_output=True, text=True)
            self.assertIn(result.returncode, (0, 1), result.stderr)
            _, counts = metrics.dependencies(json.loads(result.stdout))
            self.assertGreater(counts.get("no-circular", 0), 0)
            self.assertNotIn("no-unresolved", counts)


if __name__ == "__main__":
    unittest.main()
