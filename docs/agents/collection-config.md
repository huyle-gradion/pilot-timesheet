# Collection configuration: the `sources` section

This note describes how the current binary collects evidence for a day and the
configuration keys that control it. The configuration schema is the `Config`
type in `internal/timesheet/model.go`; the loader
(`internal/timesheet/config.go`) rejects unknown fields, so a key that is not
listed here fails to load by name (`config: unknown field "…"`).

Start from `timesheet init` (see [first-run setup](../setup-linux.md)), which
writes a valid `~/.timesheet/config.json` with no sources, or from the
synthetic template `cmd/timesheet/timesheet.example.json`. Configuration holds
only non-secret values and credential variable **names**; every credential
value lives in the env file described in [credentials](credentials.md).

## Two collectors

| Collector | Config | Credential (env-file variable) |
| --- | --- | --- |
| GitHub activity | `sources.github` | `GITHUB_TOKEN` |
| Private calendar feed (ICS) | `sources.calendar` | `GCAL_ICS_URL` (the feed URL itself) |

Both run only when `sources.enabled` **and** the collector's own `enabled` are
`true`. Without a `sources` section, or with `sources.enabled: false`, no
collector runs and the day is recorded manually.

## `sources`

| Key | Meaning |
| --- | --- |
| `enabled` | Master switch for every collector. |
| `github` | Optional GitHub collector block (below). |
| `calendar` | Optional calendar collector block (below). |

A `sources` object with neither block and `enabled` false is rejected as
`config: sources is present but empty`; omit the section instead.

### `sources.github`

| Key | Meaning |
| --- | --- |
| `enabled` | Turns this collector on. The checks below apply only when it is `true`. |
| `user` | **Required.** The GitHub login whose attributable activity is collected. |
| `repositories` | **Required.** 1–50 host-less `owner/name` entries, e.g. `example-org/example-repo`. A host-qualified slug or a filesystem path is rejected. |
| `api_base` | Optional absolute `http(s)` API root; default `https://api.github.com/`. Use it only for GitHub Enterprise or a trusted local test service. |

Items are kept only when GitHub names `user` as the acting identity; a
commit-author email is never treated as ownership. Requests carry
`GITHUB_TOKEN` as a Bearer credential, and neither the token nor a request URL
appears in errors, state or reports. There is no local-checkout (`git log`)
collector in the current build.

### `sources.calendar`

| Key | Meaning |
| --- | --- |
| `enabled` | Turns this collector on. |
| `user` | Optional calendar identity (email) whose invitation answer (`PARTSTAT`) is shown as the event's attendance status. |

The feed URL is the credential `GCAL_ICS_URL`; it is never a configuration key
and is never written to state. Events are expanded for the recorded day in the
configuration's top-level `timezone`; there is no per-source timezone.

## Missing credentials and empty days

A missing or empty credential does not stop the day. The affected collector
reports `missing_credentials` naming the **variable**, never a value, and the
other collector still runs. A collector that ran and found nothing is a
legitimate empty result. `required_env` in the configuration only reserves
names; declaring one never makes a local operation fail.

## Example: GitHub and calendar collection

Paths are placeholders; replace `state_path` with your own absolute path (the
loader does not expand `~`).

```json
{
  "timezone": "Europe/Amsterdam",
  "state_path": "/path/to/home/.timesheet/state.json",
  "projects": [
    { "id": "pilot", "name": "Pilot Timesheet" }
  ],
  "required_env": ["GITHUB_TOKEN", "GCAL_ICS_URL"],
  "sources": {
    "enabled": true,
    "github": {
      "enabled": true,
      "user": "your-github-login",
      "repositories": ["example-org/example-repo"]
    },
    "calendar": {
      "enabled": true,
      "user": "you@example.invalid"
    }
  }
}
```

The matching env file (`~/.timesheet/.env.local`, mode `0600`) holds the two
values; `timesheet init` creates it with the names commented out and no
values. Edit it yourself and never paste a value into an agent prompt.

## Example: GitHub only

```json
{
  "timezone": "Europe/Amsterdam",
  "state_path": "/path/to/home/.timesheet/state.json",
  "projects": [
    { "id": "pilot", "name": "Pilot Timesheet" }
  ],
  "sources": {
    "enabled": true,
    "github": {
      "enabled": true,
      "user": "your-github-login",
      "repositories": ["example-org/example-repo", "example-org/example-lib"]
    }
  }
}
```

## Retired schema

Earlier releases documented an `evidence.sources[]` array with `type`/`config`
string maps (git `mode`, `local_repos`, `repos`, calendar `url_ref`), a
`storage_dir` key, `keychain:<service>` credential references and the
`preflight`/`review`/`preview` commands. None of these exist in the current
build: those keys fail to load as unknown fields, and the Keychain resolver
was removed (see [credentials](credentials.md), "Historical record").
