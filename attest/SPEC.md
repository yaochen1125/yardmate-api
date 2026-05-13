# `attest` package — Apple App Attest verification

> Status: **design spec, no code yet**. Implementation lands in the next session.
> Companion: option D architecture in `yardmate-swiftui/docs/releases/v1/shared/api-secrets/api-secrets.md`.

---

## 1. Five questions (per `AI_ENGINEERING_STANDARD.md` §6)

### 1.1 What this package is responsible for

- Verify Apple App Attest **attestations** (first-time-per-install key registration).
- Verify Apple App Attest **assertions** (per-request signature with replay protection).
- Persist and look up the (keyID → publicKey, counter) credential record in BoltDB.
- Issue + remember single-use server challenges with a TTL.

### 1.2 What this package is NOT responsible for

- HTTP routing or request decoding — `main.go` / handlers wire those.
- Loading or returning secrets — that's the `secrets` package (commit 3).
- Rate limiting — that's the `ratelimit` package (commit 4).
- Refreshing Apple's **receipt** every 24 h server-side — explicitly deferred to V2 (see §6 pitfall 5).
- Reasoning about the iOS-side `DCAppAttestService` lifecycle — client owns that (D-Client PR).

### 1.3 Inputs

| Function | Input |
|---|---|
| `IssueChallenge(purpose)` | a purpose tag (`"register"` or `"secrets"`) |
| `VerifyAttestation(keyID, attestationBytes, challenge)` | raw CBOR attestation object + the challenge bytes the client claims to have used |
| `VerifyAssertion(keyID, assertionBytes, challenge)` | raw CBOR assertion + claimed challenge |

All `[]byte` are the raw decoded bytes — base64 decoding happens at the HTTP layer, **not** inside this package.

### 1.4 Outputs

| Function | Output | Error cases |
|---|---|---|
| `IssueChallenge` | 32-byte random challenge | — |
| `VerifyAttestation` | `nil` on success (credential stored) | `ErrBadCBOR`, `ErrCertChain`, `ErrAppIDMismatch`, `ErrAAGUIDMismatch`, `ErrCounterNotZero`, `ErrChallengeUnknown`, `ErrChallengeExpired`, `ErrChallengeReplay`, `ErrCredentialIDMismatch` |
| `VerifyAssertion` | `nil` on success (counter updated) | `ErrCredentialUnknown`, `ErrBadSignature`, `ErrCounterNotMonotonic`, `ErrChallengeUnknown`/`Expired`/`Replay`, `ErrAppIDMismatch` |

Errors are typed sentinels so callers can map them to HTTP `401` vs `409` precisely.

### 1.5 External dependencies

| Dep | Why |
|---|---|
| BoltDB (`go.etcd.io/bbolt`) | Persist `{keyID → {publicKey DER, counter, registeredAt}}`. Single-file, no daemon, fits "V1 self-host" sizing |
| Apple App Attestation Root CA (embedded PEM) | Trust anchor for the cert chain in every attestation. Pinned, not fetched at runtime |
| System time (`time.Now`) | Challenge TTL + registration timestamps. No NTP-strict requirement (5 min TTL is forgiving) |
| stdlib crypto (see §4) | All signature / hash / cert verification |

No outbound network calls. No SQLite / Redis / Vault.

---

## 2. Data flow

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE A — initial registration (once per device install)                │
└──────────────────────────────────────────────────────────────────────────┘

iOS App                                            yardmate-api
  │                                                     │
  │   POST /v1/attest/challenge                         │
  │   {"purpose":"register"}                            │
  │─────────────────────────────────────────────────────▶
  │                                            attest.IssueChallenge
  │                                              ├─ rand 32 bytes
  │                                              └─ cache {purpose,bytes}
  │                                                  TTL 5 min, single use
  │◀──── 200 {"challenge":"<base64 32B>"} ──────────────│
  │                                                     │
  │   DCAppAttestService.attestKey(                     │
  │     keyID,                                          │
  │     clientDataHash = SHA256(challenge)              │
  │   ) → attestation CBOR                              │
  │                                                     │
  │   POST /v1/attest/register                          │
  │   { keyID, attestation, challenge }                 │
  │─────────────────────────────────────────────────────▶
  │                                            attest.VerifyAttestation
  │                                              ├─ §3.1 step 1-9
  │                                              ├─ on success: store
  │                                              │    {keyID → publicKey,
  │                                              │     counter=0,
  │                                              │     registeredAt}
  │                                              └─ mark challenge consumed
  │◀──── 200 (empty body) ──────────────────────────────│
  │                                                     │

┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE B — per-request auth (every /v1/app-secrets call)                 │
└──────────────────────────────────────────────────────────────────────────┘

iOS App                                            yardmate-api
  │                                                     │
  │   POST /v1/secrets/challenge                        │
  │   {"purpose":"secrets"}                             │
  │─────────────────────────────────────────────────────▶
  │                                            attest.IssueChallenge
  │◀──── 200 {"challenge":"<base64>"} ──────────────────│
  │                                                     │
  │   DCAppAttestService.generateAssertion(             │
  │     keyID,                                          │
  │     clientDataHash = SHA256(challenge)              │
  │   ) → assertion CBOR                                │
  │                                                     │
  │   POST /v1/app-secrets                              │
  │   { keyID, assertion, challenge }                   │
  │─────────────────────────────────────────────────────▶
  │                                            attest.VerifyAssertion
  │                                              ├─ §3.2 step 1-6
  │                                              ├─ on success: counter++
  │                                              └─ mark challenge consumed
  │                                                     │
  │                                            secrets.Vend
  │                                              └─ read /etc/yardmate-api/
  │                                                 secrets.env, build JSON
  │◀──── 200 {"openai":"…","plantId":"…", …} ───────────│
  │                                                     │
```

### Refinement vs `api-secrets.md`

The decision doc shows a single `POST /v1/app-secrets` with `{ deviceToken: <token> }`. That conflates registration with per-request auth, which Apple's protocol doesn't allow (every signature requires a server-issued challenge). The spec above refines this into **four endpoints**: two challenge endpoints + register + app-secrets. Doc fix will land as a separate small docs PR after this implementation PR merges.

---

## 3. Apple's verification steps (canonical reference)

Source: <https://developer.apple.com/documentation/devicecheck/validating_apps_that_connect_to_your_server>

### 3.1 Attestation — 9 steps

1. **Verify cert chain.** The attestation contains `x5c` = `[credCert, intermediateCert]`. Chain must validate against the Apple App Attestation Root CA (NOT any other Apple root — see §6 pitfall 3).
2. **Build `clientDataHash`** = `SHA256(challenge_bytes)`. Append it to `authData` (also in the attestation object). Call the concatenation `composite`.
3. **Compute `nonce`** = `SHA256(composite)`.
4. **Extract embedded nonce** from `credCert`'s extension with OID `1.2.840.113635.100.8.2`. The extension is a DER-encoded ASN.1 SEQUENCE containing a single OCTET STRING. **Verify equality** with the `nonce` from step 3.
5. **Extract publicKey** from `credCert` (P-256 SubjectPublicKeyInfo). Compute `SHA256(SPKI)` and verify it equals the keyID claimed by the client.
6. **Compute `appIDHash`** = `SHA256("<TeamID>.<BundleID>")` — for YardMate: `SHA256("PMX32RG52M.com.chenyao.plantapp")`. Verify it equals the first 32 bytes of `authData` (the RP ID hash slot).
7. **Verify counter** in `authData` equals **0** (it must be a fresh key).
8. **Verify aaguid** in `authData` is in the set permitted by `ATTEST_ALLOW_DEV` (see §6.1):
   - `appattest\0\0\0\0\0\0\0` (`appattest` + 7 null bytes) — **production**, always accepted.
   - `appattestdevelop` (ASCII, 16 bytes) — **development**, accepted only when `ATTEST_ALLOW_DEV=true`.
9. **Verify credentialID** in `authData` equals the claimed `keyID`.

On success: store `{keyID, publicKeyDER, counter=0, registeredAt=now}` in BoltDB.

### 3.2 Assertion — 6 steps

1. **Look up** `publicKey` and stored `counter` by `keyID`. If missing → `ErrCredentialUnknown`.
2. **Build `clientDataHash`** = `SHA256(challenge_bytes)`.
3. **Build composite** = `authenticatorData || clientDataHash`, then **`nonce` = `SHA256(composite)`**.
4. **Verify ECDSA signature** in the assertion using stored `publicKey` over `nonce`.
5. **Verify counter**: assertion's `authenticatorData.counter` > `stored.counter`. Atomically update stored counter on success.
6. **Verify appIDHash** in `authenticatorData` matches `SHA256("PMX32RG52M.com.chenyao.plantapp")` (same as attestation step 6).

On success: counter is updated and the request proceeds.

---

## 4. Library choices

### 4.1 stdlib (only — for crypto)

| Import | Purpose |
|---|---|
| `crypto/x509` | Parse `credCert` + `intermediateCert`; build cert pool; verify chain to Apple Root |
| `crypto/x509/CertPool` | Hold the embedded Apple App Attestation Root CA |
| `crypto/sha256` | All hash computations (clientDataHash, nonce, appIDHash, keyID check) |
| `crypto/ecdsa` | ECDSA-P256 signature verification on assertions |
| `crypto/elliptic` | P-256 curve handle for ecdsa.Verify |
| `encoding/asn1` | Decode the `1.2.840.113635.100.8.2` extension (SEQUENCE → OCTET STRING) |
| `encoding/binary` | Read big-endian uint32 counter from `authData` |
| `encoding/base64` | Encode challenges for transport (no decoding inside this package — see §1.3) |
| `crypto/rand` | Issue challenges |

### 4.2 Third-party (allowed)

| Import | Purpose | Rationale |
|---|---|---|
| `github.com/fxamacker/cbor/v2` | CBOR decode of attestation/assertion objects | Most-vetted Go CBOR lib; supports the non-strict CBOR Apple emits |
| `go.etcd.io/bbolt` | Credential store | Single-file, embedded, atomic transactions — meets V1 "self-host, no daemon" need |

### 4.3 Explicitly excluded

- **Generic WebAuthn libraries** (`go-webauthn/webauthn`, `duo-labs/webauthn`, …). App Attest's CBOR layout and AAGUID semantics differ from WebAuthn enough that adapting a WebAuthn lib is more error-prone than writing native code from the 9-step spec. Read their cert-chain helpers for reference, do not import.
- **SQLite / PostgreSQL drivers.** Overkill for the credential count V1 will see.
- **Generic JWT libraries.** App Attest doesn't use JWTs anywhere.

---

## 5. Persistence schema (BoltDB)

Path: `/var/lib/yardmate-api/credentials.db` (created on first start, `chmod 600`, owned by `yardmate-api` user).

### Buckets

| Bucket | Key | Value (gob-encoded struct) |
|---|---|---|
| `credentials` | `keyID` (32 bytes) | `{ PublicKeyDER []byte, Counter uint32, RegisteredAt time.Time }` |
| `challenges` | `challenge` (32 bytes) | `{ Purpose string, IssuedAt time.Time, Consumed bool }` |

### Lifecycle

- Challenges expire after 5 min — background goroutine sweeps the bucket every 60 s.
- Credentials are immutable except for `Counter`, which is updated atomically inside `bbolt.Update`.

---

## 6. Pitfalls (read before writing the implementation)

### 6.1 Production vs development environment

`aaguid` differs by environment:

- `appattest\0\0\0\0\0\0\0` (`appattest` + 7 null bytes) — **production** entitlement (App Store / TestFlight Distribution build).
- `appattestdevelop` (ASCII, 16 bytes) — **development** entitlement (Xcode dev signing, sideloaded).

Server behavior is **stage-aware** via env flag `ATTEST_ALLOW_DEV`, set in `/etc/yardmate-api/secrets.env` and read at process start (restart required to change):

| `ATTEST_ALLOW_DEV` | Accepted aaguid | Stage |
|---|---|---|
| **`false` (default)** | production only | App Store launch — the only safe value once live |
| `true` | production OR development | Dev phase (xcodebuild + sideload to test iPhone); TestFlight internal where some testers may run dev-signed builds |

**Default is `false` on purpose** — safe by default. `ATTEST_ALLOW_DEV` must NOT be `true` once the binary is in App Store production. Ship-time SOP enforcing this lives in `## 待确认`.

#### Why `false` at launch is non-negotiable

Production attestation is Apple's cryptographic guarantee that the App Attest token came from an App-Store-signed YardMate binary on real Apple hardware. If `ATTEST_ALLOW_DEV=true` is left on after launch, an attacker holding any Apple-issued dev signing cert tied to YardMate's bundle ID could:

1. Build a "fake YardMate" with their dev cert.
2. Produce a *development*-environment attestation (`appattestdevelop` aaguid).
3. The server accepts it (dev allowed).
4. The attacker pulls down production OpenAI / Plant.id keys → drains the YardMate quota and bills our account.

The §3.1 step-6 appID hash check is the primary defense (it rejects any bundle-ID / Team-ID mismatch), but `ATTEST_ALLOW_DEV=false` is **defense in depth**: it removes the entire class of attack predicated on a leaked or shared dev cert. Both walls intact at launch.

#### Dev → prod transition

iOS entitlement `com.apple.developer.devicecheck.appattest-environment` is fixed at build time. When the same physical device switches between a dev build and an App Store build, **the keyID changes** — Apple generates a separate App Attest key pair per environment. Old dev-env credentials become inert in BoltDB; the client re-attests automatically when its locally stored production keyID is absent from the server.

### 6.2 Counter monotonicity is the replay defense

App Attest's counter is the **only** mechanism preventing replayed assertions. Implementation must:

- Compare strictly `>`, not `>=`.
- Persist counter inside the same `bbolt.Update` transaction as the verification — never read, verify, then write in two transactions.
- On `ErrCounterNotMonotonic`, return 401 to the client; client SHOULD discard its keyID and re-attest. iOS framework guarantees monotonicity per-key, so this error in production means tampering or a bug.

### 6.3 Trust anchor MUST be the App Attestation Root CA specifically

Apple has multiple roots. Use **Apple App Attestation Root CA** only:

```
https://www.apple.com/certificateauthority/Apple_App_Attestation_Root_CA.pem
```

Embed as `//go:embed` PEM in `attest/apple_root_ca.pem`. Do not fetch at runtime (network failure → service down). Verify SHA256 fingerprint after embedding (one-time check during commit 2 review).

### 6.4 clientDataHash bytes must round-trip exactly

iOS side: client computes `SHA256(challenge_bytes)` and passes that 32-byte hash to `attestKey` / `generateAssertion`.

Server side: server receives the **raw challenge bytes** back from the client and re-computes `SHA256(challenge_bytes)` for the verification math.

Common bugs:

- Base64-decoding the challenge twice on the server.
- iOS sending base64 string, server hashing the base64 instead of the raw bytes.
- Trailing newline / whitespace from JSON parsing.

Mitigation: this package accepts `[]byte` only, never `string`. HTTP layer is responsible for clean decode.

### 6.5 Receipt-based 24 h server-side recheck is V2 work

App Attest's *receipt* mechanism lets the server periodically re-verify a key with Apple's servers. V1 explicitly skips this — full rationale and revisit conditions live in `## 待确认 → V2 todos`.

### 6.6 Challenge single-use enforcement

After successful verification (attestation OR assertion), mark the challenge `Consumed = true` in the same BoltDB transaction. A second request with the same challenge → `ErrChallengeReplay`.

Sweeping consumed-and-expired entries is the background goroutine's job, not the verifier's.

### 6.7 App ID hash byte-exactness

`appIDHash = SHA256("PMX32RG52M.com.chenyao.plantapp")` — exact ASCII bytes, no leading/trailing whitespace, no NUL terminator. Hardcode the string literal in the test, do not build it via `fmt.Sprintf` with externalized parts (regression risk).

### 6.8 CBOR decoding tolerance

Apple's attestation/assertion are *valid* CBOR but not always canonical (e.g. integer encoding choices). Configure `fxamacker/cbor` with:

```
cbor.DecOptions{
    DupMapKey: cbor.DupMapKeyEnforcedAPIError,
    IndefLength: cbor.IndefLengthAllowed,
    TagsMd: cbor.TagsForbidden,
}
```

(Final values to confirm during implementation review — these are the conservative defaults.)

### 6.9 Public key storage format

Store `publicKey` as `x509.MarshalPKIXPublicKey(...)` (PKIX/SubjectPublicKeyInfo DER). Reload via `x509.ParsePKIXPublicKey` on lookup. Do **not** store the raw `(X, Y)` curve point — adds reconstruction bugs.

---

## 7. Test-vector strategy

There is **no Apple-published official App Attest test fixture** (unlike FIDO2/WebAuthn which has CTAP test vectors). Plan:

1. **Primary source: real-device captures.** During D-Test phase, run YardMate on a real iPhone (production entitlement) + a real iPhone (development entitlement). Capture the raw bytes of each attestation and assertion via debug logging. Commit redacted captures (with private parts zeroed, public parts retained) to `attest/testdata/` as fixtures.

2. **Secondary source: open-source reference implementations** (study logic, don't copy code). Searches to run before writing the implementation:

   ```
   github.com/search?q=DCAppAttestService+language%3AGo&type=code
   github.com/search?q=appattest+verifyAttestation&type=code
   ```

   Two or three repos that come up consistently in these searches are typically the right ones to compare against. The actual list will be captured in the commit-2 PR description (verifying the URLs at implementation time prevents stale link rot).

3. **Tertiary source: WebAuthn test vectors** (FIDO2 CTAP2 spec). These use a similar `authenticatorData` byte layout — useful for sanity-checking the byte parsing math (counter offset, aaguid offset, etc.) even though the aaguid + extension semantics differ.

4. **Negative-test fixtures**: flip one byte in a known-good fixture and assert each verification step's specific error sentinel. Per error in §1.4, there should be at least one negative-case test.

---

## 8. Open questions for commit-2 author (resolve before coding)

- **Endpoint paths**: stick with `/v1/attest/challenge` + `/v1/attest/register` + `/v1/secrets/challenge` + `/v1/app-secrets`? Or collapse the two challenge endpoints into one with a `purpose` body field?
- **Challenge bytes encoding on the wire**: base64-std or base64-url? Doc says "base64", which is ambiguous. Pick one and document it; iOS client must match.
- **BoltDB file path**: `/var/lib/yardmate-api/credentials.db` per §5, or under `/etc/yardmate-api/`? `/var/lib/` is FHS-conventional for mutable state.
- **First-attestation rate limit**: should `/v1/attest/register` have its own rate limit (e.g. 3 per IP per hour) to prevent storage flooding? Ratelimit package (commit 4) is per-keyID, but a fresh attestation has no established keyID yet. Open for design in commit 4.

---

## 待确认

### App Store ship-time SOP (MUST live in `deploy/README.md`; execute in this order)

Before pushing the App Store binary to App Store Connect:

1. On the production server, set `ATTEST_ALLOW_DEV=false` in `/etc/yardmate-api/secrets.env`.
2. `systemctl restart yardmate-api` (env changes take effect only on (re)start, not on `reload`).
3. With a dev-signed YardMate build sideloaded onto a test iPhone, hit `/v1/app-secrets`. **Expect 401** — server rejects the development aaguid. If you get 200, the flag is still effectively `true` somewhere — STOP, fix, re-run from step 1.
4. Only after the dev-build smoke test returns 401: push the production binary to App Store Connect.

`deploy.sh` (commit 5) enforces this by:

- Refusing to deploy if `ATTEST_ALLOW_DEV` is absent from the env file (no implicit defaults at deploy time — only at process-start time, where the in-code default is `false`).
- Printing the effective `ATTEST_ALLOW_DEV` value loudly at the end of every deploy run so the operator visually sees it before declaring "ship ready".

### V2 todos

- **24 h receipt server-side recheck.** App Attest's receipt lets the server periodically re-verify with Apple that a given key is still in good standing (device not jailbroken, app not pulled, account not suspended). V1 skips this because:
  - YardMate's threat model = protect API quota, not user data; per-keyID 10 req/day rate limit (commit 4) is the primary quota defense.
  - Receipt rechecks require outbound HTTPS to Apple per key per day + retry / backoff infra not justified by V1 scale.
  - Revisit if an abuse signal appears (sudden quota spike, rate-limit thrash in logs, support tickets about quota exhaustion despite low real-user counts).
- **Per-purpose challenge TTLs.** V1 uses 5 min uniformly. If clients in poor-network areas frequently see `ErrChallengeExpired`, consider longer for `register` (e.g. 10 min) and shorter for `secrets` (e.g. 2 min).

## 9. References

- Apple, *Validating apps that connect to your server*: <https://developer.apple.com/documentation/devicecheck/validating_apps_that_connect_to_your_server>
- Apple, *Establishing your app's integrity*: <https://developer.apple.com/documentation/devicecheck/establishing_your_app_s_integrity>
- Apple App Attestation Root CA download: <https://www.apple.com/certificateauthority/Apple_App_Attestation_Root_CA.pem>
- WWDC 2020 session 10145: *Mitigate fraud with App Attest and DeviceCheck*
- FIDO2 CTAP2 authenticatorData byte layout (for cross-checking parser math): <https://fidoalliance.org/specs/fido-v2.1-ps-20210615/fido-client-to-authenticator-protocol-v2.1-ps-20210615.html>
- `fxamacker/cbor/v2`: <https://github.com/fxamacker/cbor>
- `go.etcd.io/bbolt`: <https://github.com/etcd-io/bbolt>
