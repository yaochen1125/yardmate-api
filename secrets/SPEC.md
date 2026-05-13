# `secrets` package — load secrets.env, vend client subset

> Sibling of `attest/`. Loads `/etc/yardmate-api/secrets.env` at process start,
> exposes typed accessors for server config (ATTEST_ALLOW_DEV) + a subset of
> keys to vend to authenticated clients via `/v1/app-secrets`.

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for

- Parse a `.env`-style file: `KEY=VALUE` lines, `#` comments, blank lines.
- Hold the parsed map behind a `Vault` type and expose `Get` / `GetBool` / `Has` / `Snapshot`.
- `Snapshot(keys)` returns a stable JSON-friendly map (lowercase keys) for the HTTP layer to write to clients.

### 1.2 What this package is NOT responsible for

- Reading or watching the file at runtime — load happens once at process start, rotation requires a `systemctl restart`. See deploy/README (commit 5) for the rotation runbook.
- Encrypting secrets at rest — file lives on a 0600-perm path owned by the `yardmate-api` user; that is the only confidentiality boundary.
- Deciding *which* keys are vended — that's a `main` package concern (`vendedKeys` slice). This package is the bag of strings, not the policy.
- Variable expansion (`${OTHER}`), multi-line values, or escape sequences. V1 explicitly does not support these — out of scope.

### 1.3 Inputs

| Function | Input |
|---|---|
| `Load(path)` | filesystem path to a `.env` file |
| `Parse(r)` | any `io.Reader` over `.env`-formatted bytes (used by tests) |
| `Vault.Snapshot(keys)` | slice of UPPER_SNAKE key names to include in the snapshot |

### 1.4 Outputs

| Function | Output | Error cases |
|---|---|---|
| `Load` | `*Vault` | file open error, parse error |
| `Parse` | `*Vault` | line with no `=`, line with empty key |
| `Vault.Get(k)` | `string` (empty if unset) | — |
| `Vault.GetBool(k, def)` | parsed bool, falling back to `def` if unset or unparseable | — |
| `Vault.Has(k)` | `bool` | — |
| `Vault.Snapshot(keys)` | `map[string]string` (lowercase keys, missing → `""`) | — |

### 1.5 External dependencies

stdlib only (`bufio`, `os`, `strconv`, `strings`). No third-party.

## 2. File format

```env
# Comments and blank lines are skipped.
OPENAI_API_KEY=sk-abc...
PLANT_ID_API_KEY="quoted-value-with=signs"
ATTEST_ALLOW_DEV=false
```

Rules:

- One `KEY=VALUE` per line.
- Leading/trailing whitespace on the line is trimmed.
- Value can be wrapped in matching single (`'`) or double (`"`) quotes; the outermost pair is stripped.
- First `=` splits key from value — values may contain `=` (no need to quote).
- `#` at the start of a (trimmed) line marks a comment; inline `#` mid-line is **NOT** a comment (it's part of the value).

## 3. Vend semantics

`Vault.Snapshot(keys)` returns a fresh map intended for direct JSON encoding in `/v1/app-secrets` responses. Two non-obvious choices:

1. **Lowercase keys.** Env files use UPPER_SNAKE; client JSON uses snake_case. The snapshot transforms `OPENAI_API_KEY` → `openai_api_key` so Swift's `JSONDecoder.keyDecodingStrategy = .convertFromSnakeCase` produces `openaiApiKey` automatically.
2. **Missing keys map to empty strings.** During a partial rotation (admin removes a key but hasn't added its replacement yet), clients still see a stable response shape with `""` rather than the field disappearing. They can detect rotation gaps without crashing on missing JSON keys.

## 4. Pitfalls

### 4.1 Vended set is policy, not data

Pick `vendedKeys` (in `main` package) carefully. Adding a key here exposes it to every authenticated client — there's no per-client gating in V1. If a key is sensitive to a subset of users (e.g. an admin-only endpoint), it does NOT belong in `vendedKeys`.

### 4.2 Reload requires restart

`Load` runs once. Editing `secrets.env` while the process is live changes nothing in memory. `deploy.sh` (commit 5) calls `systemctl restart yardmate-api` after every rotation — do not ship a hot-reload watcher without a corresponding test for race conditions during reload.

### 4.3 Logging

Never log `Vault.data`. Never log values. Log only key names + counts when debugging. The package itself emits no log lines — the caller (main) is responsible for the redacted summary printed at startup.

## 5. References

- systemd `EnvironmentFile=` directive (same format as accepted here): <https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#EnvironmentFile=>
- `attest/SPEC.md` — the consumer of `ATTEST_ALLOW_DEV` + the parent service.
