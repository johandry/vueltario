# Vueltario

Vueltario is a personal and community catalog of Cuban dance moves (*vueltas*) across **casino**, **Timba**, and **rueda de casino**, paired with an administrator workflow for rights-cleared Instagram publishing to `@cubansocial.sd`.

Built from [docs/Vueltario-PRD.md](./docs/Vueltario-PRD.md) (Phases 0–5 across web, CLI, community sync, and Wails desktop, with native SwiftUI iOS in sibling repo `VueltariOS`):

- **Go modular monolith & durable job worker** (`cmd/vueltario`, `internal/*`)
- **Dual-driver persistence**: **PostgreSQL** (`migrations/postgres/`) for shared/production deployments + **SQLite** (`migrations/sqlite/`) for zero-config local execution and deterministic tests
- **Same-origin responsive Vanilla JS website** (`web/`) — zero heavy frontend frameworks, WCAG 2.2 AA accessible controls, English UI with Spanish-ready `i18n.js` hooks, official YouTube/TikTok `postMessage` player bridges, conditional Meta embeds with honest fallbacks, and keyboard timing shortcuts (`[`, `]`, `P`)
- **Unified Go CLI** (`vueltario serve | worker | migrate | capabilities | ingest | catalog | media | publish`)
- **Wails v2 Desktop Studio** (`cmd/vueltario-desktop`, `internal/desktop`, `desktop/`, `wails.json`) — invokes the exact same Go publishing core as the CLI (`DESK-01`), authenticates via RFC 7636 S256 PKCE stored in the OS Keychain (`go-keyring`), and provides queue management, draft editing, rights checklist, destination confirmation, batch workflows, and publication history

---

## Quick Start (Zero-Config Local SQLite)

```bash
# Build binary
make build

# Run API + Web UI + background enrichment worker on http://localhost:8080
./bin/vueltario serve --with-worker

# Build Wails v2 Desktop Publishing Studio
make build-desktop
```

Default local bootstrap administrator credentials (configurable via `.env` / environment variables):

- **Email:** `admin@vueltario.com`
- **Password:** `vueltario-local-admin`

---

## Running with PostgreSQL (Docker Compose)

```bash
cp .env.example .env
docker compose up --build
```

This starts PostgreSQL 16, runs migrations automatically, and separates the `server` and `worker` containers sharing `/var/lib/vueltario/storage`.

---

## CLI Reference

```bash
# Phase 0 provider capability contracts & representative fixtures
./bin/vueltario capabilities
./bin/vueltario capabilities --json

# Ingest single URL or batch file (ING-01..ING-05)
./bin/vueltario ingest "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=23s"
./bin/vueltario ingest --batch urls.txt --json

# Browse & annotate catalog
./bin/vueltario catalog list --q "Enchufla"
./bin/vueltario catalog moves
./bin/vueltario catalog add-occurrence --entry <ENTRY_ID> --move "Dile que no" --start 00:23 --end 00:30

# Portability export, backup & tested restore (Section 14 & Phase 1 acceptance)
./bin/vueltario catalog export > personal-export.json
./bin/vueltario catalog backup --out backup.json
./bin/vueltario catalog restore --in backup.json

# Authorized media import & separate video/music rights clearance (Phase 2)
./bin/vueltario media import --source <SOURCE_ID> --file ./authorized-clip.mp4
./bin/vueltario media rights-add --source <SOURCE_ID> --asset <ASSET_ID> \
  --grantor "Creator Name" --basis "Written permission" --video-cleared --music-cleared
./bin/vueltario media orphan-scan

# Instagram @cubansocial.sd publishing workflow (Phase 3)
./bin/vueltario publish draft --entry <ENTRY_ID> --asset <ASSET_ID>
./bin/vueltario publish preview <PUB_ID>
./bin/vueltario publish approve --hash <EXACT_SHA256_DRAFT_HASH> --account @cubansocial.sd <PUB_ID>
./bin/vueltario publish run --dry-run <PUB_ID>
./bin/vueltario publish run --idempotency-key run-001 <PUB_ID>
./bin/vueltario publish reconcile --retry-comment <PUB_ID>
```

### CLI Exit Codes

- `0` — Success
- `1` — Operational / runtime failure
- `2` — Usage, validation, or unsupported source host
- `3` — Authentication, role, or rights/token blocked (`blocked_auth`)
- `4` — Optimistic concurrency conflict, invalidated draft/asset approval hash, or `outcome_unknown` requiring `publish reconcile`

---

## Desktop Publishing Studio (`vueltario-desktop`)

`vueltario-desktop` is the Wails v2 native desktop GUI for administrator Instagram publishing to `@cubansocial.sd`. It binds directly to the same Go domain, media, and publishing services as the CLI (`DESK-01` — zero duplicated validation or caption/hash logic).

### Building & Launching

```bash
# Option 1: Build standalone desktop binary via Make (passes required -tags desktop,production)
make build-desktop
./bin/vueltario-desktop

# Or directly with go build (Wails v2 requires CGO_ENABLED=1 and -tags desktop,production):
CGO_ENABLED=1 go build -tags desktop,production -o bin/vueltario-desktop ./cmd/vueltario-desktop

# Option 2: Package a native OS app bundle (.app on macOS / .exe on Windows) when the Wails v2 CLI is installed
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
wails build -s -o vueltario-desktop
```

### Configuration & Database Sharing

`vueltario-desktop` reads the exact same `VUELTARIO_*` environment variables as `vueltario` (see [`.env.example`](./.env.example)):

- **Local SQLite (default):** Uses `./data/vueltario.db` and `./data/storage` out of the box—changes made in `vueltario-desktop` and the `vueltario` CLI share the same catalog, media blobs, and publication queue.
- **Shared PostgreSQL:** Set `VUELTARIO_DB_DRIVER=postgres` and `VUELTARIO_DATABASE_URL=postgres://...` before launching `vueltario-desktop` to work against a shared PostgreSQL deployment.

### Authentication & Keychain Storage

1. Launch `./bin/vueltario-desktop` and sign in with your administrator email and password (defaults locally to `admin@vueltario.com` / `vueltario-local-admin`).
2. Authentication runs an RFC 7636 `S256` PKCE exchange and stores the session token in your **OS Keychain** (`com.vueltario.desktop` via macOS Keychain, Windows Credential Manager, or Linux Secret Service)—never in plaintext files.
3. Subsequent launches and app updates automatically restore your active session until you click **Sign Out**.

### Studio Workflow

1. **Batch Workflows tab:** Paste multiple YouTube, TikTok, Facebook, or Instagram URLs to ingest in bulk (per-item errors are isolated), or select catalog entries to generate publication drafts in a batch.
2. **Queue & Draft Editor tab:**
   - Review the **Asset & Mode Preview** (external MP4/MOV/WebM upload vs. manual Instagram native-repost handoff).
   - Verify or record **Rights & Attribution** (both video redistribution and music clearance are required before external uploads can be approved).
   - Edit the caption, optional first comment, or opt-in underscore companion hashtags (`#CubanSocial_Move` alongside `#CubanSocial-Move`). Saving any edit automatically invalidates prior approval (`PUB-03`).
   - Confirm the destination account (`@cubansocial.sd`), click **Approve Exact SHA-256**, run a **Dry-Run Check**, and click **Execute Publish** (or **Reconcile** if a network timeout left a remote container in `outcome_unknown`).
3. **Publication History tab:** Audit completed publications, result permalinks, and native-repost handoffs.

---

## Security, Rights & Capability Guarantees

- **Strict SSRF protection** (`internal/netguard`): allowlisted social/API hosts only, redirect cap with per-hop revalidation, DNS/IP check blocking loopback, RFC1918 private networks, CGNAT, and cloud metadata (`169.254.169.254`).
- **Session & browser hardening** (`internal/api`): `HttpOnly` + `SameSite=Lax` cookies, constant-time `X-CSRF-Token` verification on cookie mutations, strict `Content-Security-Policy` (`script-src 'self'`, narrow `frame-src`), and origin/schema-validated `postMessage` listeners.
- **Dash-separated labels (`#CubanSocial-Enchufla`)**: stored and previewed honestly as display labels without claiming hyphenated clickable Instagram hashtags; compatible `#CubanSocial_Enchufla` underscore companions are included **only** when explicitly opted in and approved by the administrator.
- **Instagram native reposts**: Instagram-origin sources default to `native_repost_handoff`, omit extra follow promotion, and never upload a duplicate video disguised as a native repost.

---

## Testing

```bash
make test
# or directly:
go test -race -count=1 ./...
```
