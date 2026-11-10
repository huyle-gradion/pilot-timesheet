# First-run setup on Linux (Omarchy)

Status: Linux (Omarchy/Arch-based) is the qualified setup target for this
release. macOS shares the same POSIX code path but is **not runtime-qualified**
here; see the last section.

Nothing in this guide edits a desktop configuration. Omarchy's stock shell,
terminal and filesystem behaviour are sufficient; the timesheet only writes two
files under your home directory.

## 1. Get the binary

Any Go toolchain matching `go.mod` (`go 1.27.1`) builds the repository. On this
host `go` may not be on `PATH`; the mise install is environment troubleshooting,
not a project requirement:

```bash
cd ~/Projects/pilot-timesheet
go build -trimpath -o bin/timesheet ./cmd/timesheet   # or: mise exec -- go build ...
bin/timesheet version
```

No `sudo`, no system packages, no service installation.

## 2. Create the home store once

The home defaults are (discovery precedence is in section 3):

| Path | Mode | Contents |
|---|---|---|
| `~/.timesheet/` | `0700` | store directory, created only when absent |
| `~/.timesheet/config.json` | `0600` | non-secret configuration |
| `~/.timesheet/.env.local` | `0600` | credential **names** only, no values |

Run setup with at least one project. Ids and names are never invented from the
directory name, the git remote or a default classification:

```bash
bin/timesheet init --project 'pilot=Personal Pilot Timesheet'
```

- `--project` is `<id>=<name>[=<classification>]`, repeatable. Quote values
  that contain spaces. The project id must match the configuration's project-id
  grammar (no slash). The classification is optional and stays absent when you
  omit it.
- The timezone is taken from an explicit `--tz <IANA zone>`, otherwise from the
  host: `$TZ`, `/etc/timezone`, then the `/etc/localtime` symlink (the Omarchy
  path). If none resolves, setup stops and tells you to pass `--tz`; it never
  guesses `Local` or `UTC`.
- `state_path` in the generated config is **absolute**
  (`~/.timesheet/state.json`), so commands using that config do not resolve
  their store from the cwd. The state file itself is not created by setup; the
  first command that opens the store creates it.
- `--seed-config <path>` copies a loadable configuration's projects and
  `required_env` instead of `--project` values, and resolves that seed's
  relative paths against the seed file's own directory. Pass either `--project`
  or `--seed-config`, not both.

After setup, start the day with `bin/timesheet next` (it defaults to today in
the configured timezone) and follow its one typed state until `stopped` or
`human_gates`; the skill does the same loop.

Then add credential values yourself:

```bash
$EDITOR ~/.timesheet/.env.local
```

The template lists the expected names as comments. Setup never reads, parses,
prints, copies or writes a credential value — and you should not paste values
into an agent prompt. GitHub/calendar values are only needed when the matching
source is enabled in configuration.

## 3. Configuration discovery on every command

```text
explicit --config  >  ./timesheet.json in the cwd  >  ~/.timesheet/config.json
```

Rules that matter in daily use:

- The tiers are never merged and no ancestor directory is searched. A
  `timesheet.json` in the current directory is used only from that directory.
- The selected path is printed once on stderr per invocation, labelled
  `project-local`, `home default` or `explicit`, so a surprising config is
  visible without opening files.
- `--env-file` still defaults only to `~/.timesheet/.env.local`; there is no
  project-local env tier.
- `serve` keeps requiring both `--config` and `--env-file` verbatim because it
  is the protocol server, not because a launcher supplies them.
- Existing configurations are loaded exactly as before. A relative
  `state_path` keeps today's meaning (resolved against the process cwd); only
  setup-generated configs bake an absolute path.

## 4. Re-running setup is safe

```bash
bin/timesheet init
```

When both target files exist, setup is a no-op and reports each one:

- `loads OK` for a valid config, or the loader's field error for an invalid one;
- `present, not read` for the env file;
- existing modes are reported as found, including `unsafe: group/other
  permissions`, and are never chmodded;
- a symlink or non-file at a final path is reported and not followed.

Setup **never deletes, replaces, repairs or renames an existing file**, valid
or invalid. An interrupted earlier run is recovered per file: a missing file is
created on the next run, a present one is left byte-identical. A leftover
staging file from an interrupted run is left in place — setup does not clean up
by filename, age or process id — and nothing else in the directory is touched.

If `config.json` fails to load, fix it yourself — setup will not "own" or
rewrite it. Deleting the file and re-running setup is your decision, not
setup's.

## 5. Publication guarantees

Each file is written to a private `0600` temporary file inside
`~/.timesheet/`, fsynced, closed, and then published with `os.Link` (a hard
link). The link cannot overwrite an existing final path, so a concurrent setup
loses cleanly with `EEXIST` and reports `present`; a crash cannot leave a
half-written final file. The temporary file is removed when the attempt
finishes; a crash can leave one behind, and setup will neither reuse nor delete
it. The whole generated configuration is validated with the real loader before
anything under `~/.timesheet` is touched, so invalid project ids, timezone
values or seed content leave the directory untouched.

The directory is created `0700` only when absent. An existing `~/.timesheet`
that is a symlink or a regular file is refused, not followed or modified.

## 6. Troubleshooting

| Symptom | Meaning and fix |
|---|---|
| `no configuration at ./timesheet.json or ~/.timesheet/config.json` | Run `bin/timesheet init --project …`, or pass `--config <path>`. |
| `cannot determine the host IANA timezone; pass --tz` | Zone files are missing or unreadable on this host; pass `--tz Europe/Amsterdam` (or your zone). |
| `exists but does not load: "config: …"` | The existing config is invalid and was left untouched; fix that field yourself. |
| `unsafe: group/other permissions` | The existing file is readable by other users; setup reports it and does not chmod it. Fix the mode yourself. |
| `is a symlink; refusing to follow it` | `~/.timesheet` points elsewhere; resolve the link yourself, then re-run setup. |

## 7. macOS

macOS is **not runtime-qualified** for this release. The implementation uses
only POSIX filesystem operations (`0700`/`0600`, `fsync`, hard links) and has no
Keychain, launchd or desktop integration, but the owner has not run the setup
tests on macOS. Treat the above as Linux guidance; verify on macOS before
relying on it.
