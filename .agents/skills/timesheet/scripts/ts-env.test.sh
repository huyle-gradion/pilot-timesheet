#!/bin/sh
# Behavior tests for ts-env: exit status and resolved paths only, no prose.
# Run from anywhere: sh .agents/skills/timesheet/scripts/ts-env.test.sh

set -eu

here=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
helper=$here/ts-env
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ts-env-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

fails=0
fail() {
	echo "FAIL: $*" >&2
	fails=$((fails + 1))
}
pass() { echo "ok: $*"; }

# fake_binary <path>: an executable that identifies itself.
fake_binary() {
	cat >"$1" <<'EOF'
#!/bin/sh
printf 'fake-timesheet\n'
EOF
	chmod +x "$1"
}

# make_repo <dir>: the skill layout with a repository-local binary.
make_repo() {
	repo=$1
	mkdir -p "$repo/.agents/skills/timesheet/scripts" "$repo/bin"
	cp "$helper" "$repo/.agents/skills/timesheet/scripts/ts-env"
	chmod +x "$repo/.agents/skills/timesheet/scripts/ts-env"
	fake_binary "$repo/bin/timesheet"
}

# make_layout <dir>: the skill layout without any binary.
make_layout() {
	mkdir -p "$1/.agents/skills/timesheet/scripts"
	cp "$helper" "$1/.agents/skills/timesheet/scripts/ts-env"
	chmod +x "$1/.agents/skills/timesheet/scripts/ts-env"
}

# A PATH that still has the helper's utilities but no timesheet command.
toolpath="$tmp/toolpath"
mkdir -p "$toolpath"
for tool in dirname basename readlink find mkdir cat chmod touch rm; do
	ln -s "$(command -v "$tool")" "$toolpath/$tool"
done

cwd="$tmp/cwd with spaces"
mkdir -p "$cwd"

# 1. Repository path with spaces, invoked from an unrelated cwd with spaces.
repo="$tmp/repo with spaces"
make_repo "$repo"
skill="$repo/.agents/skills/timesheet/scripts/ts-env"
if got=$(cd "$cwd" && "$skill"); then
	[ "$got" = "$repo/bin/timesheet" ] && pass "spaces and different cwd" ||
		fail "spaces and different cwd: got '$got'"
else
	fail "spaces and different cwd: helper failed"
fi
if got=$(cd "$cwd" && "$skill" --root); then
	[ "$got" = "$repo" ] && pass "--root" || fail "--root: got '$got'"
else
	fail "--root: helper failed"
fi

# 2. A symlinked helper resolves to the repository that owns the real script.
ln -s "$skill" "$tmp/linked-ts-env"
if got=$(cd "$cwd" && "$tmp/linked-ts-env"); then
	[ "$got" = "$repo/bin/timesheet" ] && pass "symlinked helper" ||
		fail "symlinked helper: got '$got'"
else
	fail "symlinked helper: helper failed"
fi

# 3. No binary in the repository, TIMESHEET_ROOT, or PATH: a hard failure.
nobin="$tmp/repo no binary"
make_layout "$nobin"
nobin_skill="$nobin/.agents/skills/timesheet/scripts/ts-env"
if err=$(cd "$cwd" && PATH="$toolpath" TIMESHEET_ROOT= "$nobin_skill" 2>&1 >/dev/null); then
	fail "missing command exited 0"
else
	[ -n "$err" ] && pass "missing command fails" || fail "missing command printed no error"
fi

# 4. TIMESHEET_ROOT fallback when the repository has no binary.
other="$tmp/other root"
mkdir -p "$other/bin"
fake_binary "$other/bin/timesheet"
if got=$(cd "$cwd" && PATH="$toolpath" TIMESHEET_ROOT="$other" "$nobin_skill"); then
	[ "$got" = "$other/bin/timesheet" ] && pass "TIMESHEET_ROOT fallback" ||
		fail "TIMESHEET_ROOT fallback: got '$got'"
else
	fail "TIMESHEET_ROOT fallback: helper failed"
fi

# 5. PATH fallback when neither the repository nor TIMESHEET_ROOT has one.
pathbin="$tmp/path with binary"
mkdir -p "$pathbin"
fake_binary "$pathbin/timesheet"
if got=$(cd "$cwd" && PATH="$pathbin:$toolpath" TIMESHEET_ROOT= "$nobin_skill"); then
	[ "$got" = "$pathbin/timesheet" ] && pass "PATH fallback" ||
		fail "PATH fallback: got '$got'"
else
	fail "PATH fallback: helper failed"
fi

fake_legacy_next_binary() {
	cat >"$1" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "next" ] && [ "${2:-}" = "--help" ]; then
	exit 0
fi
exit 1
EOF
	chmod +x "$1"
}

fakego="$tmp/fakego"
mkdir -p "$fakego"
build_log="$tmp/go-build.log"
cat >"$fakego/go" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >>"$build_log"
mkdir -p bin
cat >bin/timesheet <<'INNER'
#!/bin/sh
if [ "\${1:-}" = "next" ] && [ "\${2:-}" = "--max-actions" ] && [ "\${3:-}" = "1" ] && [ "\${4:-}" = "--help" ]; then
	exit 0
fi
if [ "\${1:-}" = "next" ] && [ "\${2:-}" = "--help" ]; then
	exit 0
fi
exit 1
INNER
chmod +x bin/timesheet
EOF
chmod +x "$fakego/go"

srcrepo="$tmp/source repo"
make_layout "$srcrepo"
mkdir -p "$srcrepo/cmd/timesheet" "$srcrepo/internal" "$srcrepo/bin"
printf 'module example.com/timesheet\n' >"$srcrepo/go.mod"
printf 'package main\n' >"$srcrepo/cmd/timesheet/main.go"
printf 'package internal\n' >"$srcrepo/internal/pkg.go"
src_skill="$srcrepo/.agents/skills/timesheet/scripts/ts-env"

: >"$build_log"
fake_legacy_next_binary "$srcrepo/bin/timesheet"
if got=$(cd "$cwd" && PATH="$fakego:$toolpath" TIMESHEET_ROOT= "$src_skill"); then
	if [ "$got" = "$srcrepo/bin/timesheet" ] && [ -s "$build_log" ] && "$srcrepo/bin/timesheet" next --max-actions 1 --help; then
		pass "rebuilds pre---do binary"
	else
		fail "rebuilds pre---do binary: got='$got' log='$(cat "$build_log")'"
	fi
else
	fail "rebuilds pre---do binary: helper failed"
fi

: >"$build_log"
if got=$(cd "$cwd" && PATH="$fakego:$toolpath" TIMESHEET_ROOT= "$src_skill"); then
	if [ "$got" = "$srcrepo/bin/timesheet" ] && [ ! -s "$build_log" ]; then
		pass "skips build when binary is current"
	else
		fail "skips build when binary is current: got='$got' log='$(cat "$build_log")'"
	fi
else
	fail "skips build when binary is current: helper failed"
fi

: >"$build_log"
touch -t 202001010000 "$srcrepo/bin/timesheet"
if got=$(cd "$cwd" && PATH="$fakego:$toolpath" TIMESHEET_ROOT= "$src_skill"); then
	if [ "$got" = "$srcrepo/bin/timesheet" ] && [ -s "$build_log" ]; then
		pass "rebuilds when source is newer than binary"
	else
		fail "rebuilds when source is newer than binary: got='$got' log='$(cat "$build_log")'"
	fi
else
	fail "rebuilds when source is newer than binary: helper failed"
fi

: >"$build_log"
rm -f "$srcrepo/bin/timesheet"
if got=$(cd "$cwd" && PATH="$fakego:$toolpath" TIMESHEET_ROOT= "$src_skill"); then
	if [ "$got" = "$srcrepo/bin/timesheet" ] && [ -x "$srcrepo/bin/timesheet" ] && [ -s "$build_log" ]; then
		pass "builds missing binary in source repo"
	else
		fail "builds missing binary in source repo: got='$got' log='$(cat "$build_log")'"
	fi
else
	fail "builds missing binary in source repo: helper failed"
fi

if [ "$fails" -ne 0 ]; then
	echo "ts-env: $fails test(s) failed" >&2
	exit 1
fi
echo "ts-env: all tests passed"
