-- ============================================================================
-- Vueltario — Supabase PostgreSQL Schema, Row-Level Security & Seed Data
-- Architecture:
--   * Static Website on GitHub Pages (vueltario.com):
--       - `/`       : Public render-only catalog (queries via anon key; RLS permits SELECT on published rows only; private_notes hidden in admin_entry_notes).
--       - `/admin`  : Hidden admin studio (authenticates via Supabase Auth; RLS permits full CRUD only for auth.uid() listed in public.admin_users).
--   * Instagram Publishing (@cubansocial.sd):
--       - Executed via Go CLI (`vueltario`), Go + Wails Desktop (`vueltario-desktop`), or native SwiftUI iOS (`VueltariOS`).
--   * Video Storage:
--       - Supports Cloudflare R2 (zero egress fees, recommended) and Supabase Storage.
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Helper to auto-maintain updated_at timestamps and increment optimistic concurrency revisions
CREATE OR REPLACE FUNCTION public.vueltario_touch_updated_at()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.vueltario_bump_revision()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at = NOW();
    NEW.revision = OLD.revision + 1;
    RETURN NEW;
END;
$$;

-- ----------------------------------------------------------------------------
-- 1. Administrator Allowlist & Role Helper
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.admin_users (
    user_id UUID PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
    email TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT 'Administrator',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION public.is_vueltario_admin()
RETURNS BOOLEAN
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = public
AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.admin_users WHERE user_id = auth.uid()
    );
$$;

ALTER TABLE public.admin_users ENABLE ROW LEVEL SECURITY;

CREATE POLICY "Admin users can view own admin record"
    ON public.admin_users FOR SELECT
    TO authenticated
    USING (user_id = auth.uid() OR public.is_vueltario_admin());

-- ----------------------------------------------------------------------------
-- 2. Creators & Verified Platform Identities
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.creators (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    biography TEXT NOT NULL DEFAULT '',
    notes TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_creators_norm_trgm ON public.creators USING gin (normalized_name gin_trgm_ops);

CREATE TRIGGER trg_creators_updated_at
    BEFORE UPDATE ON public.creators
    FOR EACH ROW EXECUTE FUNCTION public.vueltario_touch_updated_at();

CREATE TABLE IF NOT EXISTS public.creator_identities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_id UUID NOT NULL REFERENCES public.creators(id) ON DELETE CASCADE,
    platform TEXT NOT NULL CHECK (platform IN ('youtube', 'tiktok', 'facebook', 'instagram')),
    platform_user_id TEXT,
    handle TEXT,
    profile_url TEXT NOT NULL DEFAULT '',
    verified BOOLEAN NOT NULL DEFAULT FALSE,
    verification_source TEXT NOT NULL DEFAULT '',
    verified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_creator_identities_creator ON public.creator_identities(creator_id);

-- ----------------------------------------------------------------------------
-- 3. Cuban Dance Taxonomy (Moves, Regional Aliases, Ordered Combinations)
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.moves (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    canonical_name TEXT NOT NULL,
    normalized_search_key TEXT NOT NULL,
    portable_token TEXT NOT NULL,
    disambiguator TEXT NOT NULL DEFAULT '',
    style_tags TEXT[] NOT NULL DEFAULT '{}',
    kind TEXT NOT NULL DEFAULT 'move' CHECK (kind IN ('move', 'combination')),
    curation_state TEXT NOT NULL DEFAULT 'curated' CHECK (curation_state IN ('proposed', 'curated', 'rejected')),
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_moves_search_trgm ON public.moves USING gin (normalized_search_key gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_moves_portable_token ON public.moves(portable_token);

CREATE TRIGGER trg_moves_updated_at
    BEFORE UPDATE ON public.moves
    FOR EACH ROW EXECUTE FUNCTION public.vueltario_touch_updated_at();

CREATE TABLE IF NOT EXISTS public.move_aliases (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    move_id UUID NOT NULL REFERENCES public.moves(id) ON DELETE CASCADE,
    alias TEXT NOT NULL,
    normalized_alias TEXT NOT NULL,
    language TEXT NOT NULL DEFAULT 'es',
    qualifier TEXT NOT NULL DEFAULT '',
    provenance TEXT NOT NULL DEFAULT 'admin',
    collision_flagged BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_move_aliases_trgm ON public.move_aliases USING gin (normalized_alias gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_move_aliases_move ON public.move_aliases(move_id);

CREATE TABLE IF NOT EXISTS public.combination_steps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    combination_move_id UUID NOT NULL REFERENCES public.moves(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    child_move_id UUID NOT NULL REFERENCES public.moves(id) ON DELETE RESTRICT,
    notes TEXT NOT NULL DEFAULT '',
    UNIQUE (combination_move_id, ordinal),
    CHECK (combination_move_id <> child_move_id)
);

-- ----------------------------------------------------------------------------
-- 4. Catalog Entries / Video Sources & Separate Admin-Only Private Notes
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    platform TEXT NOT NULL CHECK (platform IN ('youtube', 'tiktok', 'facebook', 'instagram')),
    platform_content_id TEXT,
    canonical_url TEXT NOT NULL UNIQUE,
    original_url TEXT NOT NULL,
    title TEXT,
    description TEXT,
    public_comment TEXT NOT NULL DEFAULT '',
    creator_id UUID REFERENCES public.creators(id) ON DELETE SET NULL,
    creator_role TEXT NOT NULL DEFAULT 'uploader' CHECK (creator_role IN ('original', 'uploader', 'collaborator')),
    duration_ms BIGINT CHECK (duration_ms IS NULL OR duration_ms >= 0),
    observed_offset_ms BIGINT CHECK (observed_offset_ms IS NULL OR observed_offset_ms >= 0),
    availability_state TEXT NOT NULL DEFAULT 'available'
        CHECK (availability_state IN ('unknown', 'available', 'restricted', 'private', 'deleted', 'login_required')),
    embed_eligible BOOLEAN NOT NULL DEFAULT TRUE,
    is_published BOOLEAN NOT NULL DEFAULT TRUE,
    public_video_url TEXT,
    public_video_mime TEXT,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_entries_platform_content
    ON public.entries(platform, platform_content_id)
    WHERE platform_content_id IS NOT NULL AND platform_content_id <> '';
CREATE INDEX IF NOT EXISTS idx_entries_published_updated ON public.entries(is_published, updated_at DESC);

CREATE TRIGGER trg_entries_bump_revision
    BEFORE UPDATE ON public.entries
    FOR EACH ROW EXECUTE FUNCTION public.vueltario_bump_revision();

-- Private notes live in a strictly admin-only table so anon public queries can NEVER read private annotations (PRD Section 3 & 13).
CREATE TABLE IF NOT EXISTS public.admin_entry_notes (
    entry_id UUID PRIMARY KEY REFERENCES public.entries(id) ON DELETE CASCADE,
    private_notes TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TRIGGER trg_admin_notes_updated_at
    BEFORE UPDATE ON public.admin_entry_notes
    FOR EACH ROW EXECUTE FUNCTION public.vueltario_touch_updated_at();

-- ----------------------------------------------------------------------------
-- 5. Move Occurrences & Time Ranges (Vueltas Demonstrated in Each Video)
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.occurrences (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_id UUID NOT NULL REFERENCES public.entries(id) ON DELETE CASCADE,
    move_id UUID REFERENCES public.moves(id) ON DELETE SET NULL,
    proposed_name TEXT,
    start_ms BIGINT CHECK (start_ms IS NULL OR start_ms >= 0),
    end_ms BIGINT CHECK (end_ms IS NULL OR end_ms >= 0),
    ordinal INTEGER NOT NULL DEFAULT 1,
    notes TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'confirmed' CHECK (state IN ('confirmed', 'suggested', 'rejected', 'curated')),
    timing_method TEXT NOT NULL DEFAULT 'manual' CHECK (timing_method IN ('player_mark', 'manual', 'suggested', 'unknown')),
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (start_ms IS NULL OR end_ms IS NULL OR end_ms > start_ms)
);
CREATE INDEX IF NOT EXISTS idx_occurrences_entry ON public.occurrences(entry_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_occurrences_move ON public.occurrences(move_id);

CREATE TRIGGER trg_occurrences_bump_revision
    BEFORE UPDATE ON public.occurrences
    FOR EACH ROW EXECUTE FUNCTION public.vueltario_bump_revision();

-- ----------------------------------------------------------------------------
-- 6. Cloud Video Assets (Cloudflare R2 / Supabase Storage) & Rights Clearance
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.media_assets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_id UUID NOT NULL REFERENCES public.entries(id) ON DELETE CASCADE,
    storage_provider TEXT NOT NULL DEFAULT 'r2' CHECK (storage_provider IN ('r2', 'supabase', 'filesystem')),
    bucket_name TEXT NOT NULL DEFAULT 'vueltario-media',
    object_key TEXT NOT NULL,
    public_or_cdn_url TEXT,
    access_scope TEXT NOT NULL DEFAULT 'private' CHECK (access_scope IN ('private', 'public_authorized', 'staging')),
    mime_type TEXT NOT NULL DEFAULT 'video/mp4',
    byte_size BIGINT NOT NULL DEFAULT 0 CHECK (byte_size >= 0),
    sha256_hex TEXT NOT NULL,
    duration_ms BIGINT,
    width INTEGER,
    height INTEGER,
    status TEXT NOT NULL DEFAULT 'ready' CHECK (status IN ('ready', 'revoked', 'deleted')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_media_assets_entry ON public.media_assets(entry_id);

CREATE TABLE IF NOT EXISTS public.rights_records (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_id UUID NOT NULL REFERENCES public.entries(id) ON DELETE CASCADE,
    asset_id UUID REFERENCES public.media_assets(id) ON DELETE CASCADE,
    grantor TEXT NOT NULL,
    basis TEXT NOT NULL,
    evidence_ref TEXT NOT NULL DEFAULT '',
    video_cleared BOOLEAN NOT NULL DEFAULT FALSE,
    music_cleared BOOLEAN NOT NULL DEFAULT FALSE,
    authorized_uses TEXT[] NOT NULL DEFAULT '{republish}',
    authorized_platforms TEXT[] NOT NULL DEFAULT '{instagram}',
    attribution_conditions TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    revocation_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_rights_entry ON public.rights_records(entry_id);

-- ----------------------------------------------------------------------------
-- 7. Instagram Publishing Queue (@cubansocial.sd — Used by CLI, Wails & iOS)
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.publication_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_id UUID NOT NULL REFERENCES public.entries(id) ON DELETE RESTRICT,
    asset_id UUID REFERENCES public.media_assets(id) ON DELETE SET NULL,
    destination_account TEXT NOT NULL DEFAULT '@cubansocial.sd',
    brand_prefix TEXT NOT NULL DEFAULT 'CubanSocial',
    origin_platform TEXT NOT NULL,
    publication_mode TEXT NOT NULL CHECK (publication_mode IN ('external_upload', 'native_repost_handoff')),
    move_tags_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    include_companion_tags BOOLEAN NOT NULL DEFAULT FALSE,
    caption TEXT NOT NULL DEFAULT '',
    first_comment TEXT NOT NULL DEFAULT '',
    first_comment_status TEXT NOT NULL DEFAULT 'none' CHECK (first_comment_status IN ('none', 'pending', 'posted', 'failed', 'skipped')),
    first_comment_error TEXT NOT NULL DEFAULT '',
    draft_hash TEXT NOT NULL,
    asset_hash TEXT NOT NULL DEFAULT '',
    approved_at TIMESTAMPTZ,
    approved_draft_hash TEXT NOT NULL DEFAULT '',
    approved_asset_hash TEXT NOT NULL DEFAULT '',
    remote_container_id TEXT NOT NULL DEFAULT '',
    remote_media_id TEXT NOT NULL DEFAULT '',
    remote_comment_id TEXT NOT NULL DEFAULT '',
    result_url TEXT NOT NULL DEFAULT '',
    execute_idempotency_key TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN (
        'draft', 'needs_rights', 'needs_asset', 'ready_for_review', 'approved',
        'manual_handoff', 'staging', 'uploading', 'processing', 'publishing',
        'published', 'retry_wait', 'blocked_auth', 'failed', 'cancelled', 'outcome_unknown'
    )),
    status_detail TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_pub_jobs_status ON public.publication_jobs(status, updated_at DESC);

CREATE TRIGGER trg_pub_jobs_updated_at
    BEFORE UPDATE ON public.publication_jobs
    FOR EACH ROW EXECUTE FUNCTION public.vueltario_touch_updated_at();

-- ----------------------------------------------------------------------------
-- 8. Row-Level Security Policies
--    * Public Landing Page (`/` via anon key): Read-only access to published catalog!
--    * Secret `/admin` Page, CLI, Wails, iOS (authenticated admin): Full CRUD!
-- ----------------------------------------------------------------------------
ALTER TABLE public.creators ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.creator_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.moves ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.move_aliases ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.combination_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.admin_entry_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.occurrences ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.media_assets ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.rights_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.publication_jobs ENABLE ROW LEVEL SECURITY;

-- Public Read-Only Policies (Landing Page `/`)
CREATE POLICY "Public read creators"
    ON public.creators FOR SELECT TO anon, authenticated USING (true);

CREATE POLICY "Public read creator identities"
    ON public.creator_identities FOR SELECT TO anon, authenticated USING (true);

CREATE POLICY "Public read curated moves"
    ON public.moves FOR SELECT TO anon, authenticated
    USING (curation_state = 'curated' OR public.is_vueltario_admin());

CREATE POLICY "Public read move aliases"
    ON public.move_aliases FOR SELECT TO anon, authenticated USING (true);

CREATE POLICY "Public read combination steps"
    ON public.combination_steps FOR SELECT TO anon, authenticated USING (true);

CREATE POLICY "Public read published entries only"
    ON public.entries FOR SELECT TO anon, authenticated
    USING (is_published = TRUE OR public.is_vueltario_admin());

CREATE POLICY "Public read confirmed occurrences of published entries"
    ON public.occurrences FOR SELECT TO anon, authenticated
    USING (
        public.is_vueltario_admin() OR (
            state IN ('confirmed', 'curated') AND EXISTS (
                SELECT 1 FROM public.entries e WHERE e.id = occurrences.entry_id AND e.is_published = TRUE
            )
        )
    );

CREATE POLICY "Public read only public_authorized assets"
    ON public.media_assets FOR SELECT TO anon, authenticated
    USING (
        public.is_vueltario_admin() OR (access_scope = 'public_authorized' AND status = 'ready')
    );

-- Strict Admin-Only Policies (`/admin`, CLI, Wails, iOS Publisher)
CREATE POLICY "Admin full access creators"
    ON public.creators FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access creator_identities"
    ON public.creator_identities FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access moves"
    ON public.moves FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access move_aliases"
    ON public.move_aliases FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access combination_steps"
    ON public.combination_steps FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access entries"
    ON public.entries FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin only private notes"
    ON public.admin_entry_notes FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access occurrences"
    ON public.occurrences FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access media_assets"
    ON public.media_assets FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access rights_records"
    ON public.rights_records FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

CREATE POLICY "Admin full access publication_jobs"
    ON public.publication_jobs FOR ALL TO authenticated
    USING (public.is_vueltario_admin()) WITH CHECK (public.is_vueltario_admin());

-- ----------------------------------------------------------------------------
-- 9. Seed Core Cuban Dance Moves & Regional Aliases
-- ----------------------------------------------------------------------------
INSERT INTO public.moves (canonical_name, normalized_search_key, portable_token, style_tags, kind, curation_state)
VALUES
    ('Dile que no', 'dile que no', 'DileQueNo', '{casino,rueda}', 'move', 'curated'),
    ('Guapea', 'guapea', 'Guapea', '{casino,rueda}', 'move', 'curated'),
    ('Enchufla', 'enchufla', 'Enchufla', '{casino,rueda}', 'move', 'curated'),
    ('Enchufla doble', 'enchufla doble', 'EnchuflaDoble', '{casino,rueda}', 'move', 'curated'),
    ('Vacilala', 'vacilala', 'Vacilala', '{casino,rueda}', 'move', 'curated'),
    ('Vacilala con la mano', 'vacilala con la mano', 'VacilalaConLaMano', '{casino,rueda}', 'move', 'curated'),
    ('Exhibela', 'exhibela', 'Exhibela', '{casino,rueda}', 'move', 'curated'),
    ('Setenta', 'setenta', 'Setenta', '{casino,rueda}', 'move', 'curated'),
    ('Setenta complicado', 'setenta complicado', 'SetentaComplicado', '{casino,rueda}', 'move', 'curated'),
    ('Sombrero', 'sombrero', 'Sombrero', '{casino,rueda}', 'move', 'curated'),
    ('Kentucky', 'kentucky', 'Kentucky', '{casino,rueda}', 'move', 'curated'),
    ('Prima', 'prima', 'Prima', '{casino,rueda}', 'move', 'curated'),
    ('Prima con la hermana', 'prima con la hermana', 'PrimaConLaHermana', '{casino,rueda}', 'move', 'curated'),
    ('Dame', 'dame', 'Dame', '{rueda}', 'move', 'curated'),
    ('Dame dos', 'dame dos', 'DameDos', '{rueda}', 'move', 'curated'),
    ('Paseala', 'paseala', 'Paseala', '{casino,rueda}', 'move', 'curated'),
    ('Montaña', 'montana', 'Montana', '{casino,rueda}', 'move', 'curated'),
    ('Evelyn', 'evelyn', 'Evelyn', '{casino}', 'move', 'curated'),
    ('Dedos', 'dedos', 'Dedos', '{casino}', 'move', 'curated'),
    ('Tornillo', 'tornillo', 'Tornillo', '{timba,casino}', 'move', 'curated'),
    ('Tembleque', 'tembleque', 'Tembleque', '{timba}', 'move', 'curated')
ON CONFLICT DO NOTHING;

INSERT INTO public.move_aliases (move_id, alias, normalized_alias, language, qualifier)
SELECT id, 'Sácala', 'sacala', 'es', 'Havana/classic' FROM public.moves WHERE portable_token = 'Exhibela'
UNION ALL
SELECT id, 'Adiós', 'adios', 'es', 'Miami/rueda' FROM public.moves WHERE portable_token = 'Prima'
UNION ALL
SELECT id, '70', '70', 'es', 'numeric' FROM public.moves WHERE portable_token = 'Setenta';
