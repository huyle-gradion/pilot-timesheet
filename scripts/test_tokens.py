import csv
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class CaptureFixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(
            prefix="timesheet metrics ", dir="/tmp/opencode"
        )
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.env = dict(
            os.environ,
            HOME=str(self.home),
            XDG_CONFIG_HOME=str(self.home / "active config"),
        )
        for name in [
            "PI_CODING_AGENT_DIR",
            "PIG_HOME",
            "PIG_CODING_AGENT_DIR",
            "PIG_USE_PI_DIRS",
        ]:
            self.env.pop(name, None)
        self.out = self.home / "output.csv"

    def transcript(
        self,
        path,
        usage,
        marker="/a path/bin/timesheet next --date 2026-10-20",
        stamp="2026-10-20T09:00:00Z",
    ):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            json.dumps(
                {"type": "session", "version": 3, "timestamp": stamp}
            )
            + "\n"
            + json.dumps(
                {
                    "message": {
                        "role": "assistant",
                        "content": [
                            {
                                "type": "toolCall",
                                "name": "bash",
                                "arguments": {"command": marker},
                            }
                        ],
                    }
                }
            )
            + "\n"
            + json.dumps(
                {
                    "timestamp": stamp,
                    "message": {"role": "assistant", "usage": usage},
                }
            )
            + "\n"
        )

    def capture(self, *args):
        return subprocess.run(
            ["python3", str(ROOT / "scripts/tokens.py"), "--out", str(self.out), *args],
            cwd=self.home,
            env=self.env,
            capture_output=True,
            text=True,
            check=True,
        )

    def rows(self):
        with self.out.open() as stream:
            return list(csv.DictReader(stream))


class CaptureTests(CaptureFixture):
    def test_runtime_defaults_refresh_growing_sessions_without_duplicates(self):
        paths = [
            self.home / ".pi/agent/sessions/pi.jsonl",
            self.home / "active config/pig/agent/sessions/pig.jsonl",
            self.home / ".config/pig/agent/sessions/fallback.jsonl",
            self.home / ".claude/projects/repo/claude.jsonl",
        ]
        for path in paths:
            self.transcript(
                path, {"input": 10, "output": 3, "cacheRead": 2, "cacheWrite": 1}
            )
        self.capture()
        self.assertEqual(
            {r["session_id"] for r in self.rows()},
            {"pi:pi", "pig:pig", "pig:fallback", "claude:claude"},
        )
        self.transcript(
            paths[1], {"input": 20, "output": 6, "cacheRead": 4, "cacheWrite": 2}
        )
        self.capture()
        rows = self.rows()
        self.assertEqual(len(rows), 4)
        self.assertEqual(
            next(r["total_tokens"] for r in rows if r["session_id"] == "pig:pig"), "32"
        )

    def test_override_malformed_records_unrelated_sessions_and_missing_usage(self):
        store = self.home / "override with spaces"
        self.transcript(
            store / "valid.jsonl",
            {"input_tokens": 7, "output_tokens": 2, "cache_read_input_tokens": 1},
            '"/binary with spaces/timesheet" next',
        )
        with (store / "valid.jsonl").open("a") as stream:
            stream.write(
                '\nnot json\n[]\nnull\n{"message":{"usage":{"input":"bad"}}}\n'
            )
        self.transcript(
            store / "unrelated.jsonl",
            {"input": 999},
            "discuss timesheet next in a design document",
        )
        self.transcript(store / "empty.jsonl", {})
        self.capture("--pig-dir", str(store))
        self.assertEqual(
            [(r["session_id"], r["total_tokens"]) for r in self.rows()],
            [("pig:valid", "10")],
        )

    def test_missing_stores_are_unavailable(self):
        result = self.capture()
        self.assertIn("unavailable", result.stdout)
        self.assertFalse(self.out.exists())

    def test_empty_runtime_directory_environment_uses_defaults(self):
        self.env.update(PI_CODING_AGENT_DIR="", XDG_CONFIG_HOME="")
        self.transcript(self.home / ".pi/agent/sessions/pi.jsonl", {"input": 2})
        self.transcript(
            self.home / "pig/agent/sessions/unrelated.jsonl", {"input": 999}
        )
        self.capture()
        self.assertEqual([r["session_id"] for r in self.rows()], ["pi:pi"])

    def test_pi_override_and_claude_native_usage_remain_supported(self):
        store = self.home / "custom pi sessions"
        self.transcript(store / "pi.jsonl", {"input": 11, "output": 4, "cacheRead": 2})
        path = self.home / ".claude/projects/repo/claude.jsonl"
        self.transcript(
            path,
            {
                "input_tokens": 7,
                "output_tokens": 2,
                "cache_creation_input_tokens": 3,
                "cache_read_input_tokens": 1,
            },
        )
        data = path.read_text().replace(
            '"type": "toolCall", "name": "bash", "arguments":',
            '"type": "tool_use", "name": "Bash", "input":',
        )
        path.write_text(data)
        self.capture("--pi-dir", str(store))
        self.assertEqual(
            {r["session_id"]: r["total_tokens"] for r in self.rows()},
            {"pi:pi": "17", "claude:claude": "13"},
        )

    def test_unreadable_transcript_does_not_block_valid_data(self):
        store = self.home / "pig sessions"
        self.transcript(store / "valid.jsonl", {"input": 2, "output": 1})
        (store / "unreadable.jsonl").symlink_to("/proc/self/mem")
        self.capture("--pig-dir", str(store))
        self.assertEqual([r["total_tokens"] for r in self.rows()], ["3"])

    def test_rows_the_binary_wrote_survive_a_refresh(self):
        self.out.write_text(
            "date,session_id,input_tokens,output_tokens,cache_tokens,total_tokens\n"
            "2026-10-20,timesheet-auto:20261020T150000Z,400,40,0,440\n"
        )
        self.transcript(
            self.home / ".pi/agent/sessions/pi.jsonl", {"input": 5, "output": 1}
        )
        self.capture()
        ids = [r["session_id"] for r in self.rows()]
        self.assertIn("timesheet-auto:20261020T150000Z", ids)
        self.assertEqual(len(ids), 2)

    def test_rows_defaults_to_the_newest_session_day(self):
        self.transcript(self.home / ".pi/agent/sessions/a.jsonl", {"input": 1})
        self.transcript(
            self.home / ".pi/agent/sessions/b.jsonl",
            {"input": 2},
            stamp="2026-10-21T09:00:00Z",
        )
        self.transcript(
            self.home / ".pi/agent/sessions/c.jsonl",
            {"input": 3},
            stamp="2026-10-22T09:00:00Z",
        )
        default = self.capture()
        row_lines = [line for line in default.stdout.splitlines() if "total=" in line]
        self.assertEqual(len(row_lines), 1)
        self.assertIn("pi:c", row_lines[0])
        self.assertIn("2026-10-22", row_lines[0])
        self.assertEqual(len(self.rows()), 3)

    def test_rows_all_prints_every_historical_row(self):
        self.transcript(self.home / ".pi/agent/sessions/a.jsonl", {"input": 1})
        self.transcript(
            self.home / ".pi/agent/sessions/b.jsonl",
            {"input": 2},
            stamp="2026-10-21T09:00:00Z",
        )
        result = self.capture("--rows", "all")
        row_lines = [line for line in result.stdout.splitlines() if "total=" in line]
        self.assertEqual(len(row_lines), 2)
        self.assertIn("pi:a", row_lines[0])
        self.assertIn("pi:b", row_lines[1])

    def test_rows_none_keeps_the_refresh_line_and_the_csv(self):
        self.transcript(self.home / ".pi/agent/sessions/a.jsonl", {"input": 1})
        result = self.capture("--rows", "none")
        self.assertIn("refreshed 1 observed session-day row(s)", result.stdout)
        self.assertNotIn("total=", result.stdout)
        self.assertEqual(len(self.rows()), 1)

    def test_skill_marker_matches_the_documented_next_do_invocations(self):
        script = ROOT / "scripts" / "tokens.py"
        spec = importlib.util.spec_from_file_location("tokens_under_test", script)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for command in [
            '"$TS_BINARY" next --do',
            "/path/to/timesheet next --do --date 2026-11-04",
            "timesheet next",
        ]:
            with self.subTest(command=command):
                self.assertIsNotNone(module.SKILL_MARKER.search(command))
        for command in [
            "discuss the timesheet next design",
            "timesheet documentation",
        ]:
            with self.subTest(command=command):
                self.assertIsNone(module.SKILL_MARKER.search(command))


if __name__ == "__main__":
    unittest.main()
