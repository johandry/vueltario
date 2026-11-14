-- Vueltario PostgreSQL Initial Schema (PRD v1.0)

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    external_subject TEXT UNIQUE,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'trusted_contributor', 'member')),
    locale TEXT NOT NULL DEFAULT 'en',
    analysis_opt_out BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    csrf_token TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('web', 'cli')),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);

CREATE TABLE IF NOT EXISTS invites (
    id TEXT PRIMARY KEY,
    code_hash TEXT NOT NULL UNIQUE,
    email TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('member', 'trusted_contributor')),
    invited_by TEXT NOT NULL REFERENCES users(id),
    accepted_by TEXT REFERENCES users(id),
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS creators (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    biography TEXT NOT NULL DEFAULT '',
    notes TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_creators_normalized_name ON creators(normalized_name);

CREATE TABLE IF NOT EXISTS creator_identities (
    id TEXT PRIMARY KEY,
    creator_id TEXT NOT NULL REFERENCES creators(id) ON DELETE CASCADE,
    platform TEXT NOT NULL CHECK (platform IN ('youtube', 'tiktok', 'facebook', 'instagram')),
    platform_user_id TEXT,
    handle TEXT,
    profile_url TEXT NOT NULL DEFAULT '',
    verified BOOLEAN NOT NULL DEFAULT FALSE,
    verification_source TEXT NOT NULL DEFAULT '',
    verified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_creator_identity_platform_uid
    ON creator_identities(platform, platform_user_id)
    WHERE platform_user_id IS NOT NULL AND platform_user_id <> '';
CREATE INDEX IF NOT EXISTS idx_creator_identities_creator ON creator_identities(creator_id);

CREATE TABLE IF NOT EXISTS sources (
    id TEXT PRIMARY KEY,
    platform TEXT NOT NULL CHECK (platform IN ('youtube', 'tiktok', 'facebook', 'instagram')),
    platform_content_id TEXT,
    canonical_url TEXT NOT NULL UNIQUE,
    original_url TEXT NOT NULL,
    title TEXT,
    description TEXT,
    duration_ms BIGINT CHECK (duration_ms IS NULL OR duration_ms >= 0),
    availability_state TEXT NOT NULL DEFAULT 'unknown'
        CHECK (availability_state IN ('unknown', 'available', 'restricted', 'private', 'deleted', 'login_required')),
    embed_eligible BOOLEAN NOT NULL DEFAULT TRUE,
    provenance TEXT NOT NULL DEFAULT 'user_link',
    metadata_version INTEGER NOT NULL DEFAULT 1,
    fetched_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_sources_platform_content
    ON sources(platform, platform_content_id)
    WHERE platform_content_id IS NOT NULL AND platform_content_id <> '';
CREATE INDEX IF NOT EXISTS idx_sources_platform ON sources(platform);

CREATE TABLE IF NOT EXISTS source_creators (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    creator_id TEXT NOT NULL REFERENCES creators(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('original', 'uploader', 'collaborator')),
    provenance TEXT NOT NULL DEFAULT 'manual',
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (source_id, creator_id, role)
);
CREATE INDEX IF NOT EXISTS idx_source_creators_source ON source_creators(source_id);

CREATE TABLE IF NOT EXISTS moves (
    id TEXT PRIMARY KEY,
    canonical_name TEXT NOT NULL,
    normalized_search_key TEXT NOT NULL,
    portable_token TEXT NOT NULL,
    disambiguator TEXT NOT NULL DEFAULT '',
    style_tags_json TEXT NOT NULL DEFAULT '[]',
    kind TEXT NOT NULL CHECK (kind IN ('move', 'combination')),
    curation_state TEXT NOT NULL CHECK (curation_state IN ('proposed', 'curated', 'rejected')),
    description TEXT NOT NULL DEFAULT '',
    created_by TEXT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_moves_search_key ON moves(normalized_search_key);
CREATE INDEX IF NOT EXISTS idx_moves_portable_token ON moves(portable_token);

CREATE TABLE IF NOT EXISTS move_aliases (
    id TEXT PRIMARY KEY,
    move_id TEXT NOT NULL REFERENCES moves(id) ON DELETE CASCADE,
    alias TEXT NOT NULL,
    normalized_alias TEXT NOT NULL,
    language TEXT NOT NULL DEFAULT 'es',
    qualifier TEXT NOT NULL DEFAULT '',
    provenance TEXT NOT NULL DEFAULT 'manual',
    collision_flagged BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_move_aliases_normalized ON move_aliases(normalized_alias);
CREATE INDEX IF NOT EXISTS idx_move_aliases_move ON move_aliases(move_id);

CREATE TABLE IF NOT EXISTS combination_steps (
    id TEXT PRIMARY KEY,
    combination_move_id TEXT NOT NULL REFERENCES moves(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    child_move_id TEXT NOT NULL REFERENCES moves(id) ON DELETE RESTRICT,
    notes TEXT NOT NULL DEFAULT '',
    UNIQUE (combination_move_id, ordinal)
);

CREATE TABLE IF NOT EXISTS catalog_entries (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
    visibility_state TEXT NOT NULL DEFAULT 'private'
        CHECK (visibility_state IN ('private', 'submitted', 'published', 'rejected', 'withdrawn', 'removed')),
    private_notes TEXT NOT NULL DEFAULT '',
    personal_title_override TEXT,
    personal_creator_override TEXT,
    observed_offset_ms BIGINT CHECK (observed_offset_ms IS NULL OR observed_offset_ms >= 0),
    client_request_id TEXT,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ,
    UNIQUE (user_id, source_id)
);
CREATE INDEX IF NOT EXISTS idx_entries_user_updated ON catalog_entries(user_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_entries_source ON catalog_entries(source_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_entries_user_client_req
    ON catalog_entries(user_id, client_request_id)
    WHERE client_request_id IS NOT NULL AND client_request_id <> '';

CREATE TABLE IF NOT EXISTS occurrences (
    id TEXT PRIMARY KEY,
    entry_id TEXT NOT NULL REFERENCES catalog_entries(id) ON DELETE CASCADE,
    move_id TEXT REFERENCES moves(id) ON DELETE SET NULL,
    proposed_name TEXT,
    start_ms BIGINT CHECK (start_ms IS NULL OR start_ms >= 0),
    end_ms BIGINT CHECK (end_ms IS NULL OR end_ms >= 0),
    ordinal INTEGER NOT NULL DEFAULT 1,
    notes TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'confirmed'
        CHECK (state IN ('confirmed', 'suggested', 'rejected', 'curated')),
    timing_method TEXT NOT NULL DEFAULT 'unknown'
        CHECK (timing_method IN ('player_mark', 'manual', 'suggested', 'unknown')),
    evidence_ids_json TEXT NOT NULL DEFAULT '[]',
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (start_ms IS NULL OR end_ms IS NULL OR end_ms > start_ms)
);
CREATE INDEX IF NOT EXISTS idx_occurrences_entry ON occurrences(entry_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_occurrences_move ON occurrences(move_id);

CREATE TABLE IF NOT EXISTS evidence (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    owner_user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('title', 'description', 'transcript', 'ocr', 'comment', 'manual')),
    original_ref TEXT NOT NULL DEFAULT '',
    excerpt TEXT NOT NULL,
    observed_start_ms BIGINT,
    observed_end_ms BIGINT,
    is_announcement_ts BOOLEAN NOT NULL DEFAULT FALSE,
    author_role TEXT NOT NULL DEFAULT 'unknown',
    access_scope TEXT NOT NULL CHECK (access_scope IN ('public_factual', 'private_user', 'admin')),
    retrieved_at TIMESTAMPTZ NOT NULL,
    retention_deadline TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_evidence_source ON evidence(source_id);

CREATE TABLE IF NOT EXISTS suggestions (
    id TEXT PRIMARY KEY,
    entry_id TEXT NOT NULL REFERENCES catalog_entries(id) ON DELETE CASCADE,
    source_revision INTEGER NOT NULL,
    candidate_move_id TEXT REFERENCES moves(id) ON DELETE SET NULL,
    candidate_name TEXT NOT NULL,
    start_ms BIGINT,
    end_ms BIGINT,
    is_announcement_only BOOLEAN NOT NULL DEFAULT FALSE,
    evidence_ids_json TEXT NOT NULL DEFAULT '[]',
    processor_version TEXT NOT NULL,
    confidence_band TEXT NOT NULL CHECK (confidence_band IN ('high', 'medium', 'low', 'unverified')),
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'accepted', 'rejected')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_suggestions_entry ON suggestions(entry_id);

CREATE TABLE IF NOT EXISTS media_assets (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
    owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access_scope TEXT NOT NULL DEFAULT 'private' CHECK (access_scope IN ('private', 'public_authorized', 'staging')),
    asset_kind TEXT NOT NULL CHECK (asset_kind IN ('original', 'clip_derivative', 'staging')),
    storage_backend TEXT NOT NULL DEFAULT 'filesystem',
    storage_key TEXT NOT NULL,
    device_id TEXT,
    mime_type TEXT NOT NULL,
    byte_size BIGINT NOT NULL CHECK (byte_size >= 0),
    sha256_hex TEXT NOT NULL,
    duration_ms BIGINT,
    width INTEGER,
    height INTEGER,
    status TEXT NOT NULL CHECK (status IN ('quarantine', 'ready', 'quarantined_orphan', 'revoked', 'deleted')),
    derivation_parent_id TEXT REFERENCES media_assets(id) ON DELETE SET NULL,
    source_time_map_json TEXT NOT NULL DEFAULT '{}',
    expires_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_media_assets_source ON media_assets(source_id);
CREATE INDEX IF NOT EXISTS idx_media_assets_owner ON media_assets(owner_user_id);

CREATE TABLE IF NOT EXISTS rights_records (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    asset_id TEXT REFERENCES media_assets(id) ON DELETE CASCADE,
    grantor TEXT NOT NULL,
    basis TEXT NOT NULL,
    evidence_ref TEXT NOT NULL DEFAULT '',
    video_cleared BOOLEAN NOT NULL DEFAULT FALSE,
    music_cleared BOOLEAN NOT NULL DEFAULT FALSE,
    authorized_uses_json TEXT NOT NULL DEFAULT '[]',
    authorized_platforms_json TEXT NOT NULL DEFAULT '[]',
    attribution_conditions TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    revocation_reason TEXT NOT NULL DEFAULT '',
    reviewer_id TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rights_source ON rights_records(source_id);
CREATE INDEX IF NOT EXISTS idx_rights_asset ON rights_records(asset_id);

CREATE TABLE IF NOT EXISTS public_submissions (
    id TEXT PRIMARY KEY,
    entry_id TEXT NOT NULL REFERENCES catalog_entries(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    submitter_id TEXT NOT NULL REFERENCES users(id),
    snapshot_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'changes_requested', 'rejected', 'withdrawn')),
    reviewer_id TEXT REFERENCES users(id),
    decision_reason TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_submissions_status ON public_submissions(status, created_at DESC);

CREATE TABLE IF NOT EXISTS public_revisions (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    submission_id TEXT REFERENCES public_submissions(id),
    revision_number INTEGER NOT NULL,
    curated_payload_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active', 'superseded', 'hidden', 'removed')),
    hidden_reason TEXT NOT NULL DEFAULT '',
    approved_by TEXT NOT NULL REFERENCES users(id),
    approved_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (source_id, revision_number)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_public_revisions_active
    ON public_revisions(source_id)
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS public_corrections (
    id TEXT PRIMARY KEY,
    public_revision_id TEXT NOT NULL REFERENCES public_revisions(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    proposer_id TEXT NOT NULL REFERENCES users(id),
    proposed_changes_json TEXT NOT NULL,
    reason TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    reviewer_id TEXT REFERENCES users(id),
    review_note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS reports (
    id TEXT PRIMARY KEY,
    public_revision_id TEXT REFERENCES public_revisions(id) ON DELETE SET NULL,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    reporter_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    reporter_contact TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL CHECK (category IN ('incorrect_naming', 'misattribution', 'unavailable_source', 'rights_concern')),
    details TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'dismissed')),
    resolved_by TEXT REFERENCES users(id),
    resolution_note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS processing_jobs (
    id TEXT PRIMARY KEY,
    owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    job_type TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    target_revision INTEGER NOT NULL DEFAULT 1,
    payload_json TEXT NOT NULL DEFAULT '{}',
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'retry_wait', 'blocked_auth', 'needs_input', 'unsupported', 'failed', 'cancelled')),
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    sanitized_error TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL UNIQUE,
    correlation_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_runnable ON processing_jobs(state, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_jobs_target ON processing_jobs(target_type, target_id);

CREATE TABLE IF NOT EXISTS publication_jobs (
    id TEXT PRIMARY KEY,
    destination_account TEXT NOT NULL,
    brand_prefix TEXT NOT NULL,
    origin_platform TEXT NOT NULL,
    publication_mode TEXT NOT NULL CHECK (publication_mode IN ('external_upload', 'native_repost_handoff')),
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
    public_revision_id TEXT REFERENCES public_revisions(id) ON DELETE SET NULL,
    private_entry_id TEXT REFERENCES catalog_entries(id) ON DELETE SET NULL,
    private_entry_revision INTEGER,
    asset_id TEXT REFERENCES media_assets(id) ON DELETE SET NULL,
    selected_segments_json TEXT NOT NULL DEFAULT '[]',
    move_tags_json TEXT NOT NULL DEFAULT '[]',
    include_companion_tags BOOLEAN NOT NULL DEFAULT FALSE,
    caption TEXT NOT NULL DEFAULT '',
    first_comment TEXT NOT NULL DEFAULT '',
    first_comment_status TEXT NOT NULL DEFAULT 'none'
        CHECK (first_comment_status IN ('none', 'pending', 'posted', 'failed', 'skipped')),
    first_comment_error TEXT NOT NULL DEFAULT '',
    draft_hash TEXT NOT NULL,
    asset_hash TEXT NOT NULL DEFAULT '',
    approved_by TEXT REFERENCES users(id),
    approved_at TIMESTAMPTZ,
    approved_draft_hash TEXT NOT NULL DEFAULT '',
    approved_asset_hash TEXT NOT NULL DEFAULT '',
    rights_snapshot_json TEXT NOT NULL DEFAULT '{}',
    staging_token TEXT NOT NULL DEFAULT '',
    staging_expires_at TIMESTAMPTZ,
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
    created_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_publication_jobs_status ON publication_jobs(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_publication_jobs_source ON publication_jobs(source_id);

CREATE TABLE IF NOT EXISTS audit_events (
    id TEXT PRIMARY KEY,
    actor_id TEXT,
    actor_role TEXT NOT NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    target_revision INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    correlation_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_events(target_type, target_id, created_at DESC);

CREATE TABLE IF NOT EXISTS sync_events (
    seq BIGSERIAL PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    entity_type TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('upsert', 'delete')),
    revision INTEGER NOT NULL,
    payload_json TEXT NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sync_events_user_seq ON sync_events(user_id, seq);

CREATE TABLE IF NOT EXISTS analysis_usage_daily (
    usage_date TEXT PRIMARY KEY,
    runs_count INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL
);
