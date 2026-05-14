# `deploy/` — yardmate-api production deployment

Self-host runbook for the App-Attest-gated secret-vending service. Server is a
single 4-vCPU / 8 GB / 160 GB box at `5.78.183.252`. systemd-managed,
Linux/amd64 binary built from this repo and shipped via scp.

## 0. One-time server bootstrap (admin only)

Run once when provisioning a fresh box. Subsequent deploys re-use this layout.

```bash
ssh root@5.78.183.252

# Dedicated user (no login shell, no home dir).
useradd --system --user-group --no-create-home --shell /usr/sbin/nologin yardmate-api

# Paths.
install -d -o yardmate-api -g yardmate-api -m 0750 /etc/yardmate-api
install -d -o yardmate-api -g yardmate-api -m 0750 /var/lib/yardmate-api

# systemd unit (the file from this repo is installed by deploy.sh on first run).
# Just verify the path is what we'll use:
ls -la /etc/systemd/system/yardmate-api.service 2>/dev/null || \
    echo "Will be installed by deploy.sh"
```

## 1. `secrets.env` — format + lifecycle

Lives at `/etc/yardmate-api/secrets.env` on the server, `chmod 600`, owned by
the `yardmate-api` user. **Never committed to git.** Maintained locally by
the operator (in `~/.config/yardmate-api/secrets.env.prod` or similar — keep
it outside this repo) and pushed via `deploy.sh`.

Format (see `secrets/SPEC.md §2` + `secrets.env.example`):

```env
# Server config
ATTEST_ALLOW_DEV=false

# Vended to authenticated clients via /v1/app-secrets
OPENAI_API_KEY=sk-...
PLANT_ID_API_KEY=...
```

### Required keys (deploy.sh refuses to ship without all of these set)

- `ATTEST_ALLOW_DEV` — `true` or `false`, no implicit default. See §3.
- `OPENAI_API_KEY` — OpenAI GPT-4o-mini key for AI features.
- `PLANT_ID_API_KEY` — Plant.id API key for plant identification.

Adding a new key:

1. Add it to your local `secrets.env.prod`.
2. Add the env name (UPPER_SNAKE) to `vendedKeys` in `handlers.go` if it should reach the iOS client.
3. Run `./deploy/deploy.sh`. Restart picks up the new value.

## 2. Deploy

From a clean checkout of this repo on macOS, with your local secrets file at
`~/.config/yardmate-api/secrets.env.prod`:

```bash
YARDMATE_SECRETS=~/.config/yardmate-api/secrets.env.prod ./deploy/deploy.sh
```

What `deploy.sh` does, in order:

1. **Pre-flight checks (local):**
   - `go test ./...` — refuses to ship a failing build.
   - Validate `$YARDMATE_SECRETS` exists, is `0600`, contains the three required keys + `ATTEST_ALLOW_DEV` explicitly.
2. **Build:** `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/yardmate-api-linux-amd64 .`
3. **Ship binary:** `scp bin/yardmate-api-linux-amd64 root@host:/tmp/yardmate-api.new`
4. **Ship secrets:** `scp "$YARDMATE_SECRETS" root@host:/tmp/secrets.env.new`
5. **Ship unit file:** `scp deploy/yardmate-api.service root@host:/etc/systemd/system/yardmate-api.service`
6. **Install + restart (over ssh):**
   ```bash
   install -o yardmate-api -g yardmate-api -m 0755 /tmp/yardmate-api.new /usr/local/bin/yardmate-api
   install -o yardmate-api -g yardmate-api -m 0600 /tmp/secrets.env.new /etc/yardmate-api/secrets.env
   shred -u /tmp/yardmate-api.new /tmp/secrets.env.new
   systemctl daemon-reload
   systemctl enable --now yardmate-api
   systemctl restart yardmate-api
   ```
7. **Verify:** `curl -sf http://127.0.0.1:8080/healthz` over ssh → must return `{"status":"ok"}` within 5 s.
8. **Print effective config loudly:** reads `grep ATTEST_ALLOW_DEV /etc/yardmate-api/secrets.env` on the host and prints it in big letters before declaring success. The operator's eyeball check is the final gate.

## 3. `ATTEST_ALLOW_DEV` — stage-aware ship SOP

Recap of `attest/SPEC §待确认`:

| Stage | `ATTEST_ALLOW_DEV` | When |
|---|---|---|
| Local dev (xcodebuild on a paired iPhone) | `true` | Active D-Client development |
| Internal TestFlight with dev-signed builds | `true` | Pre-App-Store testing |
| App Store production | **`false`** | Once a binary is in App Store Connect, forever |

**The flip from `true` → `false` happens BEFORE the App Store binary is pushed to App Store Connect**, in this exact order:

1. Edit local `secrets.env.prod`: `ATTEST_ALLOW_DEV=false`.
2. `./deploy/deploy.sh` (this also runs the §2 step-8 eyeball check — read it).
3. On a test iPhone with a dev-signed YardMate build, hit `/v1/app-secrets`. **Expect 401** (`{"error":"aaguid_mismatch"}`). If you get 200, **STOP** — flag is still effectively `true` somewhere; debug then re-run from step 1.
4. Only after the dev-build smoke test returns 401: push the production binary to App Store Connect.

Why this matters: production attestation is Apple's cryptographic guarantee
that the App-Attest token came from an App-Store-signed YardMate binary on
real Apple hardware. Leaving `ATTEST_ALLOW_DEV=true` after launch lets anyone
holding *any* Apple-issued dev signing cert tied to this Bundle ID build a
"fake YardMate", produce a development-environment attestation, and drain
the OpenAI / Plant.id quota.

`deploy.sh` enforces defense-in-depth:

- Refuses to deploy if `ATTEST_ALLOW_DEV` is absent from the env file (no implicit defaults at deploy time).
- Refuses to deploy if `ATTEST_ALLOW_DEV=true` and `$YARDMATE_DEPLOY_STAGE=prod` (the operator must `YARDMATE_DEPLOY_STAGE=dev` explicitly to ship a dev-allowing binary).
- Prints the effective value loudly at the end of every deploy.

## 4. Rotation

Rotating an API key (`OPENAI_API_KEY`, `PLANT_ID_API_KEY`, etc.):

1. Generate the new key at the provider's dashboard.
2. Edit local `secrets.env.prod` — replace the old value with the new.
3. `./deploy/deploy.sh` — restart picks up the new value (the in-memory Vault
   is built from the file content at process start; `systemctl restart` is
   how rotation takes effect).
4. Wait ~30 s; check `journalctl -u yardmate-api -n 50` for healthy startup.
5. Revoke the old key at the provider (last step — only after the new key is confirmed working).

iOS clients re-fetch via `/v1/app-secrets` opportunistically (cached in memory). During the brief window where some clients have the old key and the server has only the new one, those clients see 401/403 from OpenAI/Plant.id and re-fetch. Within minutes the whole fleet is on the new key.

## 5. Rollback

If a deploy regresses behavior, roll the binary back:

```bash
ssh root@5.78.183.252
# The previous binary is saved by deploy.sh as /usr/local/bin/yardmate-api.prev
mv /usr/local/bin/yardmate-api.prev /usr/local/bin/yardmate-api
systemctl restart yardmate-api
journalctl -u yardmate-api -n 50
```

For a code change that needs a longer fix: revert the offending PR on `main`,
re-deploy. Don't hot-patch the server binary.

## 6. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `journalctl` shows `load secrets: open /etc/yardmate-api/secrets.env: permission denied` | File owner ≠ `yardmate-api` OR mode ≠ 0600 | Re-run deploy (it reinstalls with correct perms) |
| `open store: open /var/lib/yardmate-api/credentials.db: permission denied` | `/var/lib/yardmate-api` owner ≠ `yardmate-api` | `chown -R yardmate-api:yardmate-api /var/lib/yardmate-api` |
| `aaguid_mismatch` from a known-good production iOS build | `ATTEST_ALLOW_DEV` flipped wrong-way OR Team/Bundle ID drift | Check secrets.env on host + `YARDMATE_API_APP_ID` env in unit file |
| Client gets 429 immediately | per-IP limit too tight for shared NAT egress | Raise `YARDMATE_API_RL_IP_LIMIT` (env in systemd unit) |
| BoltDB grows past expected size | challenge sweeper not running | Restart service; bbolt compacts on open (rare path) |

## 7. References

- `attest/SPEC.md` — App Attest protocol implementation
- `secrets/SPEC.md` — env file format + load semantics
- `ratelimit/SPEC.md` — bucket policy + post-verify rationale
- `deploy/yardmate-api.service` — the unit file shipped to the host
- `deploy/secrets.env.example` — template for the local secrets file
- `deploy/deploy.sh` — the script
