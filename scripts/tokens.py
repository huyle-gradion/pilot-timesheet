#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Capture observed timesheet tokens locally. Run with python3 scripts/tokens.py.

Pi and pig use message.usage input/output/cacheRead/cacheWrite. Claude uses
message.usage input_tokens/output_tokens/cache_*_input_tokens. Runtime-prefixed
CSV rows are refreshed, not appended twice. No transcript text leaves the reader.
These are observed session totals, not isolated judgment cost or performance.
"""

import argparse
import csv
from dataclasses import dataclass
from datetime import date
import json
import os
from pathlib import Path
import re
import sys
import tempfile
from typing import Final, TypeAlias, TypedDict

SKILL_MARKER: Final = re.compile(
    r'(?:^|[;&|\n])\s*["\']?(?:[^\n"\']*/timesheet|timesheet|\$TS_BINARY)["\']?\s+'
    r"(?:next|collect|draft|run|record|status|agent-submit)\b"
)
FIELDS: Final = ("date", "session_id", "input_tokens", "output_tokens", "cache_tokens", "total_tokens")
REPO: Final = Path(__file__).resolve().parents[1]
JSONValue: TypeAlias = None | bool | int | float | str | list["JSONValue"] | dict[str, "JSONValue"]
USAGE_KEYS: Final = (
    "input_tokens", "input", "output_tokens", "output",
    "cache_creation_input_tokens", "cache_read_input_tokens", "cacheRead", "cacheWrite",
)


class TokenRow(TypedDict):
    date: str
    session_id: str
    input_tokens: str
    output_tokens: str
    cache_tokens: str
    total_tokens: str


@dataclass(frozen=True, slots=True)
class SessionUsage:
    is_skill: bool
    days: dict[str, list[int]]


def has_invocation(record: JSONValue) -> bool:
    if isinstance(record, list):
        return any(has_invocation(item) for item in record)
    if not isinstance(record, dict):
        return False
    command = record.get("command")
    if isinstance(command, str) and SKILL_MARKER.search(command):
        return True
    return any(
        has_invocation(value)
        for value in record.values()
        if isinstance(value, (dict, list))
    )


def scan_session(path: Path) -> SessionUsage:
    is_skill = False
    per_day: dict[str, list[int]] = {}
    session_date = None
    with path.open(encoding="utf-8", errors="replace") as stream:
        for line in stream:
            try:
                record: JSONValue = json.loads(line)
            except json.JSONDecodeError:
                continue
            if not isinstance(record, dict):
                continue
            message = record.get("message")
            if not isinstance(message, dict):
                message = {}
            is_skill = is_skill or has_invocation(message.get("content", []))
            ts = record.get("timestamp")
            try:
                day = (
                    date.fromisoformat(ts[:10]).isoformat()
                    if isinstance(ts, str)
                    else session_date
                )
            except ValueError:
                continue
            session_date = session_date or day
            usage = message.get("usage", record.get("usage"))
            if not day or not isinstance(usage, dict) or not usage:
                continue
            if not any(key in usage for key in USAGE_KEYS):
                continue
            counts = [usage.get(key, 0) for key in USAGE_KEYS]
            if any(not isinstance(v, int) or isinstance(v, bool) or v < 0 for v in counts):
                continue
            tokens = {k: v for k, v in usage.items() if isinstance(v, int) and not isinstance(v, bool)}
            values = [
                tokens.get("input_tokens", tokens.get("input", 0)),
                tokens.get("output_tokens", tokens.get("output", 0)),
                sum(tokens.get(key, 0) for key in USAGE_KEYS[4:]),
            ]
            agg = per_day.setdefault(day, [0, 0, 0])
            for index, value in enumerate(values):
                agg[index] += value
    return SessionUsage(is_skill, per_day)


def expand_home(path: str) -> Path:
    if path == "~":
        return Path.home()
    return Path.home() / path[2:] if path.startswith("~/") else Path(path)


def pig_session_roots(override: str | None) -> list[Path]:
    if override:
        return [expand_home(override)]
    if os.environ.get("PIG_USE_PI_DIRS") == "1":
        agent = expand_home(
            os.environ.get("PI_CODING_AGENT_DIR") or str(Path.home() / ".pi/agent")
        )
    else:
        root = Path.home() / ".pig"
        if xdg := os.environ.get("XDG_CONFIG_HOME"):
            root = expand_home(xdg) / "pig"
        if home := os.environ.get("PIG_HOME"):
            root = expand_home(home)
        agent = (
            expand_home(os.environ["PIG_CODING_AGENT_DIR"])
            if os.environ.get("PIG_CODING_AGENT_DIR")
            else root / "agent"
        )
    return [agent / "sessions", Path.home() / ".config/pig/agent/sessions"]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--claude-dir", default=str(Path.home() / ".claude"))
    parser.add_argument(
        "--pi-dir",
        default=str(
            expand_home(
                os.environ.get("PI_CODING_AGENT_DIR") or str(Path.home() / ".pi/agent")
            )
            / "sessions"
        ),
    )
    parser.add_argument(
        "--pig-dir",
        help="override pig environment/default stores and the legacy fallback",
    )
    parser.add_argument("--out", default=str(REPO / "benchmarks/tokens.csv"))
    parser.add_argument(
        "--rows", choices=("newest", "all", "none"), default="newest",
        help="session-day rows to print: newest (default), all, or none",
    )
    args = parser.parse_args()
    sources = [
        ("claude", Path(args.claude_dir) / "projects"),
        ("pi", Path(args.pi_dir)),
    ]
    sources.extend(
        ("pig", root)
        for root in dict.fromkeys(
            root.resolve() for root in pig_session_roots(args.pig_dir)
        )
    )
    transcripts: dict[Path, tuple[Path, set[str]]] = {}
    for runtime, root in sources:
        for path in sorted(root.glob("**/*.jsonl")):
            transcripts.setdefault(path.resolve(), (path, set()))[1].add(runtime)
    out_path = Path(args.out)
    rows: dict[tuple[str, str], TokenRow] = {}
    if out_path.exists():
        with out_path.open(newline="", encoding="utf-8") as stream:
            for row in csv.DictReader(stream):
                rows[row["date"], row["session_id"]] = TokenRow(
                    date=row["date"],
                    session_id=row["session_id"],
                    input_tokens=row["input_tokens"],
                    output_tokens=row["output_tokens"],
                    cache_tokens=row["cache_tokens"],
                    total_tokens=row["total_tokens"],
                )
    captured = 0
    for path, runtimes in transcripts.values():
        runtime = (
            "+".join(sorted(runtimes)) + "-shared"
            if len(runtimes) > 1
            else next(iter(runtimes))
        )
        try:
            session = scan_session(path)
        except OSError:
            print("tokens.py: unreadable transcript skipped", file=sys.stderr)
            continue
        if not session.is_skill:
            continue
        for day, values in session.days.items():
            if (
                runtimes <= {"pi", "pig"}
                and (day, f"pi+pig-shared:{path.stem}") in rows
            ):
                runtime = "pi+pig-shared"
            session_id = f"{runtime}:{path.stem}"
            for producer in runtimes:
                rows.pop((day, f"{producer}:{path.stem}"), None)
            rows[day, session_id] = TokenRow(
                date=day,
                session_id=session_id,
                input_tokens=str(values[0]),
                output_tokens=str(values[1]),
                cache_tokens=str(values[2]),
                total_tokens=str(sum(values)),
            )
            captured += 1
    if not captured:
        print("tokens.py: usage unavailable; no matching sessions with valid usage")
        return 0
    out_path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w", newline="", encoding="utf-8", dir=out_path.parent, delete=False
        ) as stream:
            temporary = Path(stream.name)
            writer = csv.DictWriter(stream, fieldnames=FIELDS)
            writer.writeheader()
            writer.writerows(rows[key] for key in sorted(rows))
        os.replace(temporary, out_path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    print(f"tokens.py: refreshed {captured} observed session-day row(s) in {out_path}")
    ordered = [rows[key] for key in sorted(rows)]
    if args.rows == "newest":
        ordered = ordered[-1:]
    elif args.rows == "none":
        ordered = []
    for row in ordered:
        print(
            f"tokens.py: {row['session_id']} {row['date']} total={row['total_tokens']}"
        )
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, csv.Error, KeyError) as error:
        print(
            f"tokens.py: capture unavailable ({type(error).__name__})", file=sys.stderr
        )
        sys.exit(1)
