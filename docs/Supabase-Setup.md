# Supabase Setup Guide for Vueltario (`johandry`)

This step-by-step guide walks through creating and configuring the **Vueltario** Supabase project under your existing GitHub account (`johandry`) and Supabase organization (`johandry`), then connecting it to:

1. **GitHub Pages Static Website** (`/` render-only public catalog + `/admin` sign-in gated studio)
2. **Go CLI (`vueltario`) & Wails Desktop (`vueltario-desktop`)**
3. **Native SwiftUI iOS App (`VueltariOS`)**

---

## Prerequisites

- Supabase account signed in via GitHub as **`johandry`**
- Existing Supabase organization **`johandry`**
- Repository files in place:
  - Schema & RLS migration: [`../supabase/migrations/0001_vueltario_supabase.sql`](../supabase/migrations/0001_vueltario_supabase.sql)
  - GitHub Pages workflow: [`../.github/workflows/deploy-pages.yml`](../.github/workflows/deploy-pages.yml)
  - Keepalive workflow: [`../.github/workflows/supabase-keepalive.yml`](../.github/workflows/supabase-keepalive.yml)

---

## Step 1: Create the `vueltario` Project in Organization `johandry`

1. Go to [https://supabase.com/dashboard](https://supabase.com/dashboard) and sign in with **Continue with GitHub** (`johandry`).
2. Select your **`johandry`** organization from the dashboard sidebar or organization list.
3. Click **New project** and fill in the form:
   - **Organization:** `johandry`
   - **Name:** `vueltario`
   - **Database Password:** Click **Generate a password**, **copy and save it immediately in your password manager** (you will need this later for the Go CLI / Wails PostgreSQL connection string).
   - **Region:** **`West US (North California)`** (`us-west-1`) — closest low-latency region for San Diego / `@cubansocial.sd`.
   - **Pricing Plan:** Current organization plan (Free tier works out of the box; the included GitHub Action cron keeps it awake).
4. Expand **Security options** / **Data API** (if shown during creation) and keep the defaults:
   - **Enable Data API (PostgREST):** **On** *(required for the serverless GitHub Pages Vanilla JS site)*
   - **Enable automatic RLS:** **On**
5. Click **Create new project** and wait ~60–90 seconds until the project status badge turns green (**Active / Healthy**).

---

## Step 2: Copy Your Project URL, Keys, and Postgres URI

### 2.1 Project URL & API Keys

1. In the left sidebar, go to **Project Settings** (gear icon at the bottom) → **Data API** (or **API** / **API Keys**).
2. Record these three values:
   - **Project URL:** `https://<PROJECT_REF>.supabase.co` *(note the 20-character `<PROJECT_REF>` subdomain)*
   - **Anon / Publishable Key (`anon` `public`):** Safe to expose in the GitHub Pages frontend because Row-Level Security blocks all public writes and hides `admin_entry_notes`.
   - **Service Role / Secret Key (`service_role`):** **Keep strictly secret!** Never put this in GitHub Pages Variables or browser code—use it only in your local `.env` if you use Supabase Storage from the CLI/Wails.

### 2.2 PostgreSQL Connection String (For Go CLI & `vueltario-desktop`)

1. Click the **Connect** button in the top header bar of the Supabase Dashboard.
2. Select the **ORMs / Connection String** → **URI** tab.
3. Under **Method**, select **Session pooler** (port `5432`, IPv4 + IPv6 compatible—works reliably on residential/mobile Wi-Fi without IPv6 issues):

   ```text
   postgresql://postgres.<PROJECT_REF>:[YOUR-PASSWORD]@aws-0-us-west-1.pooler.supabase.com:5432/postgres?sslmode=require
   ```

4. Replace `[YOUR-PASSWORD]` with the database password you saved in Step 1 (URL-encode special characters like `@`, `#`, `%`, or `/` if your password contains them).

---

## Step 3: Apply the Vueltario Schema, RLS Policies, and Seed Data

1. In the Supabase left sidebar, click **SQL Editor** → **New query**.
2. Open [`../supabase/migrations/0001_vueltario_supabase.sql`](../supabase/migrations/0001_vueltario_supabase.sql) locally, copy the **entire file contents**, and paste them into the Supabase SQL Editor.
3. Click **Run** (or press `Cmd + Enter`).
4. Confirm you see **`Success. No rows returned`** (or the inserted count for the seeded moves).
5. Quick verification—go to **Table Editor** in the left sidebar and confirm all 11 tables exist with the **RLS enabled** lock icon:
   - `admin_users`
   - `creators` & `creator_identities`
   - `moves` (pre-populated with 21 Cuban dance moves), `move_aliases`, & `combination_steps`
   - `entries`, `admin_entry_notes`, & `occurrences`
   - `media_assets`, `rights_records`, & `publication_jobs`

---

## Step 4: Configure Authentication & Lock Down Public Signups

Because Vueltario's landing page has no public user accounts and only you (`johandry`) sign in at `.../admin`:

1. In the left sidebar, go to **Authentication** → **URL Configuration**:
   - **Site URL:** `https://vueltario.com` *(or `https://johandry.github.io/Vueltario` until your custom domain DNS is pointed)*
   - **Redirect URLs:** Click **Add URL** and add:
     - `https://vueltario.com/admin`
     - `https://vueltario.com/admin/`
     - `https://johandry.github.io/Vueltario/admin/`
     - `http://localhost:8000/admin/` *(for local `make preview-site` testing)*
   - Click **Save changes**.

2. Create your Administrator user account:
   - Go to **Authentication** → **Users** → click **Add user** → **Create new user**.
   - **Email:** Enter your admin email (e.g., `johandry@gmail.com` or `admin@vueltario.com`).
   - **Password:** Choose a strong password (this is what you will type on `.../admin` and in `VueltariOS`).
   - **Auto Confirm User:** Check **Yes / Enabled**.
   - Click **Create user**.
   - Copy the newly created user's **User UID** (a UUID like `f47ac10b-58cc-4372-a567-0e02b2c3d479`).

3. Lock down open public signups *(recommended)*:
   - Go to **Authentication** → **Sign In / Providers** (or **User Signups**).
   - Toggle **Allow new users to sign up** to **OFF** (existing users can still sign in with email + password, and nobody on the internet can self-register accounts against your public `anon` key).
   - Click **Save**.

---

## Step 5: Grant Your User UID Administrator Access (`public.admin_users`)

Row-Level Security in Vueltario checks `public.is_vueltario_admin()`, which only allows inserts, updates, deletes, and private-note reads when `auth.uid()` is listed in `public.admin_users`.

1. Go back to **SQL Editor** → **New query**.
2. Paste and run the following SQL (substituting your **User UID** and **Email** from Step 4.2):

   ```sql
   INSERT INTO public.admin_users (user_id, email, display_name)
   VALUES (
       'PASTE-YOUR-AUTH-USER-UUID-HERE',
       'your-admin-email@example.com',
       'Johandry'
   )
   ON CONFLICT (user_id) DO UPDATE
   SET email = EXCLUDED.email,
       display_name = EXCLUDED.display_name;
   ```

3. Verify with:

   ```sql
   SELECT * FROM public.admin_users;
   ```

   You should see 1 row with your `user_id`, `email`, and `display_name`.

---

## Step 6: Optional — Create Supabase Storage Fallback Bucket

*(If you use **Cloudflare R2** for zero-egress video hosting as recommended, you can skip this step until needed. Creating the bucket now takes 30 seconds if you want a single-dashboard fallback.)*

1. Go to **Storage** in the Supabase left sidebar → click **New bucket**.
2. **Name of bucket:** `vueltario-media`
3. **Public bucket:** Keep **OFF** *(private bucket; signed URLs are generated on demand during Instagram staging)*.
4. Click **Save**.

---

## Step 7: Wire Credentials into GitHub Pages, Local `.env`, and `VueltariOS`

### 7.1 GitHub Repository Variables (For Static Website & Keepalive Action)

Once you push `johandry/Vueltario` to GitHub:

1. Open `https://github.com/johandry/Vueltario/settings/pages` and set **Build and deployment → Source** to **GitHub Actions**.
2. Open `https://github.com/johandry/Vueltario/settings/secrets/actions` → switch to the **Variables** tab → click **New repository variable**:
   - Name: `VUELTARIO_SUPABASE_URL`
     Value: `https://<PROJECT_REF>.supabase.co`
   - Name: `VUELTARIO_SUPABASE_ANON_KEY`
     Value: `<YOUR_ANON_PUBLIC_KEY>`
   - *(Optional)* Name: `VUELTARIO_R2_PUBLIC_URL`
     Value: `https://media.vueltario.com` *(if using a public R2 custom domain)*

> Note: Both `deploy-pages.yml` and `supabase-keepalive.yml` read these `vars.*` automatically—never put `service_role` or database passwords into public repository variables.

### 7.2 Local `.env` (For Go CLI `vueltario` & Wails `vueltario-desktop`)

In the root of your local `Vueltario` repository:

```bash
cp .env.example .env
```

Update these lines in `.env` (`.env` is gitignored so secrets are never committed):

```dotenv
VUELTARIO_DB_DRIVER=postgres
VUELTARIO_DATABASE_URL=postgresql://postgres.<PROJECT_REF>:<YOUR_DB_PASSWORD>@aws-0-us-west-1.pooler.supabase.com:5432/postgres?sslmode=require

VUELTARIO_SUPABASE_URL=https://<PROJECT_REF>.supabase.co
VUELTARIO_SUPABASE_ANON_KEY=<YOUR_ANON_PUBLIC_KEY>
VUELTARIO_SUPABASE_SERVICE_KEY=<YOUR_SERVICE_ROLE_SECRET_KEY>
VUELTARIO_SUPABASE_BUCKET=vueltario-media
```

*(Note: For the Go CLI and `vueltario-desktop` SQL layer, also run `./bin/vueltario migrate up` once if using the CLI's internal tables, or manage videos directly from `/admin`.)*

---

## Step 8: Smoke-Test End-to-End Locally

1. Start the local static site preview:

   ```bash
   make preview-site
   ```

2. Open [`http://localhost:8000/`](http://localhost:8000/):
   - Confirm there is **no Sign In button** and **no Admin link**.
   - Click **Move Dictionary** and verify the 21 seeded Cuban dance moves load directly from your remote Supabase database.

3. Open [`http://localhost:8000/admin/`](http://localhost:8000/admin/):
   - It goes straight to **Administrator Sign In** (your public Supabase URL and `sb_publishable_...` key are pre-configured in `site/js/config.js` and protected by PostgreSQL Row-Level Security).
   - Sign in with the admin email & password you created in **Step 4.2**.
   - Paste a YouTube or TikTok dance tutorial URL → click **Save Video Link** → mark a move's start/end timestamps (`[` / `]`) → click **Save Video Updates**.

4. Reload [`http://localhost:8000/`](http://localhost:8000/) and confirm your newly added video, demonstrated vuelta chips, and segment jump buttons render immediately on the public landing page.
