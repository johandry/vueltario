# Vueltario — Architecture & Setup Guide

Vueltario is a curated catalog of Cuban dance moves (*vueltas*) across **casino**, **Timba**, and **rueda de casino**, with rights-cleared Instagram publishing to [`@cubansocial.sd`](https://www.instagram.com/cubansocial.sd/).

- **Live GitHub Pages Website:** [https://www.johandry.com/vueltario/](https://www.johandry.com/vueltario/) *(planned custom domain: [https://vueltario.com](https://vueltario.com))*
- **Direct Admin Studio:** `https://www.johandry.com/vueltario/admin/` *(planned custom domain: `https://vueltario.com/admin/`)*
- **GitHub Repositories:** [`johandry/vueltario`](https://github.com/johandry/vueltario) (Web, CLI, Wails Desktop) · [`johandry/VueltariOS`](https://github.com/johandry/VueltariOS) (Native SwiftUI iOS + Share Extension)

## Serverless & Multi-Client Architecture

```text
GitHub Pages (site/)
  ├── /          (Render-only Vanilla JS landing page — NO sign-in or admin links)
  └── /admin     (Direct-URL Admin Studio — Supabase Auth + full CRUD, timing, comments, taxonomy)
         │
         ▼
Supabase (Managed PostgreSQL + Auth + Row-Level Security)
  ├── anon key           → SELECT only on published entries, curated moves, aliases & creators
  └── authenticated JWT  → Full CRUD strictly for UUIDs listed in public.admin_users
         ▲
         │
Instagram @cubansocial.sd Publishers (Shared Publishing Rules + Cloudflare R2 / Supabase Storage)
  ├── Go CLI             (cmd/vueltario)
  ├── Go + Wails Studio  (cmd/vueltario-desktop)
  └── Native SwiftUI iOS (../VueltariOS + Share Extension)
```

---

## 1. Supabase Setup (Database + Auth + RLS)

> For a complete step-by-step walkthrough tailored to the `johandry` GitHub user and Supabase organization, see [**`docs/Supabase-Setup.md`**](./docs/Supabase-Setup.md).

1. Create a Supabase project at [supabase.com](https://supabase.com).
2. Open **SQL Editor** and execute [`supabase/migrations/0001_vueltario_supabase.sql`](./supabase/migrations/0001_vueltario_supabase.sql).
   - Creates `admin_users`, `creators`, `creator_identities`, `moves`, `move_aliases`, `combination_steps`, `entries`, `admin_entry_notes`, `occurrences`, `media_assets`, `rights_records`, and `publication_jobs`.
   - Enables strict **Row-Level Security** so public visitors on `/` can never mutate rows or read `admin_entry_notes`.
   - Pre-seeds core Cuban dance moves (*Dile que no*, *Enchufla*, *Enchufla doble*, *Setenta*, *Exhibela / Sácala*, *Prima / Adiós*, *Sombrero*, *Tornillo*, etc.).
3. In **Authentication → Users**, create your administrator user (`email` + `password`) and copy its **User UID**.
4. Register that UID in `public.admin_users` via SQL Editor:

```sql
INSERT INTO public.admin_users (user_id, email, display_name)
VALUES ('<YOUR-AUTH-USER-UUID>', 'you@example.com', 'Johandry');
```

---

## 2. Static Website on GitHub Pages (`site/`)

- **Landing page (`/` → [`site/index.html`](./site/index.html)):** Live at [https://www.johandry.com/vueltario/](https://www.johandry.com/vueltario/) *(and [https://vueltario.com](https://vueltario.com) once DNS is connected)*. Strictly **render-only**: visitors search canonical Spanish move names or regional aliases, filter by platform/style, watch embedded YouTube/TikTok/Facebook/Instagram players or hosted R2/Supabase videos, jump to timed move segments, and follow original creators. There is **no** Sign In button and **no** link to `/admin`.
- **Hidden Admin Studio (`.../admin` → [`site/admin/index.html`](./site/admin/index.html)):** Navigate directly to `https://www.johandry.com/vueltario/admin/` *(or `https://vueltario.com/admin` / `http://localhost:8000/admin/` locally)*. Prompts immediately for Supabase Auth email + password, verifies membership in `public.admin_users`, and unlocks:
  - Saving & deduplicating YouTube, TikTok, Facebook, and Instagram URLs (plus automatic YouTube/TikTok oEmbed metadata lookup and batch URL import)
  - **✨ AI Metadata & Vuelta Extractor:** One-click taxonomy-grounded prompt generator for ChatGPT, Claude, or local vision agents (supports screenshots of on-screen text overlays + pasted captions/comments) and live-preview JSON importer that maps aliases (`70` → `Setenta`) and creates new moves automatically
  - Live player **Mark Start `[`** / **Mark End `]`** / **Preview `P`** and manual `MM:SS.mmm` timing
  - Inline editing & deletion of move occurrences, segment notes, public comments, private admin notes, creator attribution, and video/music rights records
  - Move taxonomy management (canonical moves, regional aliases with collision detection, and ordered combinations)
  - Full JSON catalog export

### Deploying to GitHub Pages

1. In your GitHub repository **Settings → Pages**, set **Source** to **GitHub Actions**.
2. In **Settings → Secrets and variables → Actions → Variables**, add:
   - `VUELTARIO_SUPABASE_URL` — `https://<project-ref>.supabase.co`
   - `VUELTARIO_SUPABASE_ANON_KEY` — your public Supabase `anon` key
   - `VUELTARIO_R2_PUBLIC_URL` *(optional)* — custom public R2 CDN domain if used
3. Push to `main`: [`.github/workflows/deploy-pages.yml`](./.github/workflows/deploy-pages.yml) deploys `./site` automatically, and [`.github/workflows/supabase-keepalive.yml`](./.github/workflows/supabase-keepalive.yml) pings Supabase every 4 days so Free-tier projects never pause.

### Previewing the Static Website Locally

```bash
make preview-site
# Public landing page (render-only): http://localhost:8000/
# Admin studio (sign-in gated):      http://localhost:8000/admin/
```

---

## 3. Video Storage (Cloudflare R2 Recommended)

When storing authorized MP4/MOV/WebM originals or staging videos for Meta's Instagram Content Publishing URL-fetch API, configure **Cloudflare R2** ($0 egress fees, 10 GB free tier) or **Supabase Storage** in `.env`:

```bash
cp .env.example .env
# Set VUELTARIO_STORAGE_PROVIDER=r2 and your R2 bucket credentials (or VUELTARIO_STORAGE_PROVIDER=supabase)
```

During `publish run`, Vueltario uploads/stages the authorized asset and generates a short-lived AWS SigV4 presigned URL (R2) or signed URL (Supabase Storage) for Meta's Reels container fetch.

---

## 4. Desktop Publishing Studio (`vueltario-desktop`)

`vueltario-desktop` is the Wails v2 native desktop GUI for publishing to `@cubansocial.sd`, sharing the exact same Go publishing and validation engine as the CLI (`DESK-01`).

### Building & Launching

```bash
# Build standalone desktop binary (passes required -tags desktop,production and CGO_ENABLED=1)
make build-desktop
./bin/vueltario-desktop

# Or directly with go build:
CGO_ENABLED=1 go build -tags desktop,production -o bin/vueltario-desktop ./cmd/vueltario-desktop

# Optional: package native OS .app / .exe bundle with Wails CLI
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
wails build -s -o vueltario-desktop
```

### Connecting `vueltario-desktop` & CLI to Remote Supabase Postgres

Set your Supabase Postgres connection string from **Supabase Dashboard → Connect (Session Pooler)**:

```bash
export VUELTARIO_DB_DRIVER=postgres
export VUELTARIO_DATABASE_URL="postgresql://postgres.<project-ref>:<password>@aws-0-us-west-1.pooler.supabase.com:5432/postgres?sslmode=require"
```

*(When `VUELTARIO_DATABASE_URL` is omitted, both `vueltario` and `vueltario-desktop` default to local SQLite `./data/vueltario.db` for offline testing.)*

### Desktop Authentication & Publishing Workflow

1. Launch `./bin/vueltario-desktop` and sign in—session credentials are negotiated via RFC 7636 `S256` PKCE and stored in your **OS Keychain** (`com.vueltario.desktop`), never in plaintext files.
2. **Batch Workflows tab:** Batch-ingest links or batch-create `@cubansocial.sd` drafts with isolated error handling.
3. **Queue & Draft Editor tab:** Verify video + music rights, edit caption & optional first comment, toggle explicit `#CubanSocial_Move` companion hashtags alongside `#CubanSocial-Move` display labels, approve the exact SHA-256 draft hash, run **Dry-Run Check**, and click **Execute Publish** (or **Reconcile**).

---

## 5. Go CLI (`vueltario`)

```bash
make build

# Provider capability matrix & fixtures
./bin/vueltario capabilities

# Ingest single URL or batch file
./bin/vueltario ingest "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=23s"
./bin/vueltario ingest --batch urls.txt --json

# Manage catalog & occurrences
./bin/vueltario catalog list --q "Enchufla"
./bin/vueltario catalog add-occurrence --entry <ENTRY_ID> --move "Dile que no" --start 00:23 --end 00:30
./bin/vueltario catalog backup --out backup.json

# Import authorized video to R2 / Supabase / local storage & record rights
./bin/vueltario media import --source <SOURCE_ID> --file ./authorized-clip.mp4
./bin/vueltario media rights-add --source <SOURCE_ID> --asset <ASSET_ID> \
  --grantor "Creator Name" --basis "Written permission" --video-cleared --music-cleared

# Publish to Instagram @cubansocial.sd
./bin/vueltario publish draft --entry <ENTRY_ID> --asset <ASSET_ID>
./bin/vueltario publish approve --hash <EXACT_SHA256_DRAFT_HASH> --account @cubansocial.sd <PUB_ID>
./bin/vueltario publish run --dry-run <PUB_ID>
./bin/vueltario publish run --idempotency-key run-001 <PUB_ID>
./bin/vueltario publish reconcile --retry-comment <PUB_ID>

# AI Metadata & Vuelta Extractor (works with remote --supabase or local DB + optional ffmpeg frame sampling)
./bin/vueltario ai prompt --entry "https://www.facebook.com/reel/1507199801474207" --supabase
./bin/vueltario ai prompt --entry <ENTRY_ID> --video ./reel.mp4 --supabase | pbcopy
./bin/vueltario ai apply --entry "https://www.facebook.com/reel/1507199801474207" --supabase --in ai-output.json
./bin/vueltario ai run-agent --entry <ENTRY_ID> --supabase --video ./reel.mp4 --bin ollama --args "run qwen2.5vl"
```

---

## 6. Native iOS App (`VueltariOS`)

The companion SwiftUI iOS app + Share Extension lives in the sibling repository `../VueltariOS`, supporting offline Share Sheet link capture (`group.com.vueltario.shared`), Supabase sync, and mobile Instagram `@cubansocial.sd` publishing & native-repost handoffs.

---

## Testing

```bash
make test
```
