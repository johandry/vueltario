// Secret /admin Studio Controller (site/admin/index.html)
// Reached ONLY by navigating directly to .../admin. Authenticates via Supabase Auth and enforces admin_users RLS.
// Supports full CRUD: ingest/deduplicate links, update title/description/public comment/private notes, creator attribution,
// add/update/delete move occurrences + start/end timing (player mark or MM:SS.mmm), video/music rights, R2/Supabase video URLs, and taxonomy.

import { isSupabaseConfigured } from './config.js';
import { buildAIExtractionPrompt, parseAIExtractionJSON } from './ai-assist.js';
import {
  getSupabaseClient,
  el,
  formatMs,
  parseTimestampMs,
  validateBounds,
  normalizeSearchKey,
  portableMoveToken,
  normalizeSocialUrl,
  tryFetchBrowserOEmbed,
  buildPlaybackDescriptor,
} from './utils.js';
import { PlayerBridge } from './players.js';

const state = {
  sb: null,
  session: null,
  adminRecord: null,
  moves: [],
  entries: [],
  filteredEntries: [],
  selectedEntry: null,
  comboStepsDraft: [],
};

let player = null;

function toast(msg, isError = false) {
  const b = document.getElementById('admin-toast');
  b.textContent = msg;
  b.classList.toggle('error', isError);
  b.classList.remove('hidden');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => b.classList.add('hidden'), 6000);
}

async function init() {
  wireStaticEvents();
  if (!isSupabaseConfigured()) {
    toast('Supabase project URL/publishable key missing in site/js/config.js.', true);
    return;
  }
  state.sb = await getSupabaseClient();
  const { data } = await state.sb.auth.getSession();
  state.session = data?.session || null;
  if (state.session) {
    await verifyAdminAndBoot();
  } else {
    showViewState('login');
  }
}

function showViewState(mode) {
  document.getElementById('admin-login-gate').classList.toggle('hidden', mode !== 'login');
  document.getElementById('admin-workspace').classList.toggle('hidden', mode !== 'workspace');
  document.getElementById('admin-nav').classList.toggle('hidden', mode !== 'workspace');
  document.getElementById('admin-user-pill').classList.toggle('hidden', mode !== 'workspace');
  document.getElementById('btn-admin-signout').classList.toggle('hidden', mode !== 'workspace');
}

async function verifyAdminAndBoot() {
  const uid = state.session?.user?.id;
  if (!uid) {
    showViewState('login');
    return;
  }
  const { data: adminRow, error } = await state.sb
    .from('admin_users')
    .select('user_id, email, display_name')
    .eq('user_id', uid)
    .maybeSingle();

  if (error || !adminRow) {
    await state.sb.auth.signOut();
    state.session = null;
    showViewState('login');
    toast(
      `Signed-in user (${uid}) is not listed in public.admin_users. Add your auth.users UUID to public.admin_users in Supabase SQL Editor.`,
      true
    );
    return;
  }

  state.adminRecord = adminRow;
  document.getElementById('admin-user-pill').textContent = `${adminRow.display_name} (${adminRow.email})`;
  showViewState('workspace');
  await refreshAll();
}

async function refreshAll(preserveSelectedId = null) {
  const [movesRes, entriesRes] = await Promise.all([
    state.sb
      .from('moves')
      .select('*, move_aliases(*), combination_steps!combination_steps_combination_move_id_fkey(*)')
      .order('canonical_name'),
    state.sb
      .from('entries')
      .select('*, admin_entry_notes(private_notes), creators(id, display_name, creator_identities(*)), occurrences(*, moves(canonical_name)), media_assets(*), rights_records(*)')
      .order('updated_at', { ascending: false }),
  ]);

  if (movesRes.error) toast(movesRes.error.message, true);
  if (entriesRes.error) toast(entriesRes.error.message, true);

  state.moves = (movesRes.data || []).map((m) => ({
    ...m,
    aliases: m.move_aliases || [],
    steps: m.combination_steps || [],
  }));

  state.entries = (entriesRes.data || []).map((e) => ({
    ...e,
    private_notes: e.admin_entry_notes?.private_notes ?? e.admin_entry_notes?.[0]?.private_notes ?? '',
    occurrences: (e.occurrences || [])
      .sort((a, b) => (a.ordinal || 0) - (b.ordinal || 0))
      .map((o) => ({
        ...o,
        moveName: o.moves?.canonical_name || o.proposed_name || 'Move',
      })),
  }));

  populateComboStepPicker();
  applyCatalogFilters();
  renderTaxonomyList();

  const targetId = preserveSelectedId || state.selectedEntry?.id;
  if (targetId) {
    const found = state.entries.find((x) => x.id === targetId);
    if (found) {
      state.selectedEntry = found;
      renderDetail(true);
    }
  }
}

// --- Catalog Ingestion & Filtering ---

async function ingestSingleUrl(rawInput) {
  const norm = normalizeSocialUrl(rawInput);

  // Deduplicate by (platform, platform_content_id) first, then canonical_url (ING-03)
  let existing = null;
  if (norm.platformContentId) {
    const { data } = await state.sb
      .from('entries')
      .select('id')
      .eq('platform', norm.platform)
      .eq('platform_content_id', norm.platformContentId)
      .maybeSingle();
    existing = data;
  }
  if (!existing) {
    const { data } = await state.sb.from('entries').select('id').eq('canonical_url', norm.canonicalUrl).maybeSingle();
    existing = data;
  }
  if (existing) {
    await refreshAll(existing.id);
    selectAdminEntry(existing.id);
    return { status: 'duplicate', id: existing.id, platform: norm.platform };
  }

  // Save immediately before optional oEmbed enrichment (ING-04)
  const insertPayload = {
    platform: norm.platform,
    platform_content_id: norm.platformContentId,
    canonical_url: norm.canonicalUrl,
    original_url: norm.originalUrl,
    observed_offset_ms: norm.observedOffsetMs,
    is_published: true,
    availability_state: 'available',
    embed_eligible: true,
  };

  const { data: created, error } = await state.sb.from('entries').insert(insertPayload).select('id').single();
  if (error) throw new Error(error.message);

  // Progressive browser oEmbed enrichment for YouTube / TikTok
  const meta = await tryFetchBrowserOEmbed(norm.platform, norm.canonicalUrl);
  if (meta) {
    let creatorId = null;
    if (meta.creatorName) {
      creatorId = await upsertCreatorWithIdentity({
        displayName: meta.creatorName,
        platform: norm.platform,
        handle: meta.creatorHandle || '',
        profileUrl: meta.creatorProfileUrl || '',
        verified: false,
      });
    }
    await state.sb
      .from('entries')
      .update({
        title: meta.title,
        creator_id: creatorId,
        creator_role: 'uploader',
      })
      .eq('id', created.id);
  }

  await refreshAll(created.id);
  selectAdminEntry(created.id);
  return { status: 'created', id: created.id, platform: norm.platform };
}

async function upsertCreatorWithIdentity({ displayName, platform, handle, profileUrl, verified }) {
  const normName = normalizeSearchKey(displayName);
  if (!normName) return null;

  let creatorId = null;
  const { data: found } = await state.sb.from('creators').select('id').eq('normalized_name', normName).limit(1).maybeSingle();
  if (found) {
    creatorId = found.id;
    await state.sb.from('creators').update({ display_name: displayName.trim() }).eq('id', creatorId);
  } else {
    const { data: ins, error } = await state.sb
      .from('creators')
      .insert({ display_name: displayName.trim(), normalized_name: normName })
      .select('id')
      .single();
    if (error) throw new Error(error.message);
    creatorId = ins.id;
  }

  const cleanHandle = (handle || '').trim().replace(/^@/, '');
  if (cleanHandle || profileUrl) {
    const { data: ident } = await state.sb
      .from('creator_identities')
      .select('id')
      .eq('creator_id', creatorId)
      .eq('platform', platform)
      .limit(1)
      .maybeSingle();

    if (ident) {
      await state.sb
        .from('creator_identities')
        .update({
          handle: cleanHandle || null,
          profile_url: (profileUrl || '').trim(),
          verified: Boolean(verified),
          verified_at: verified ? new Date().toISOString() : null,
        })
        .eq('id', ident.id);
    } else {
      await state.sb.from('creator_identities').insert({
        creator_id: creatorId,
        platform,
        handle: cleanHandle || null,
        profile_url: (profileUrl || '').trim(),
        verified: Boolean(verified),
        verified_at: verified ? new Date().toISOString() : null,
      });
    }
  }
  return creatorId;
}

function applyCatalogFilters() {
  const q = normalizeSearchKey(document.getElementById('adm-q').value);
  const plat = document.getElementById('adm-platform').value;
  const vis = document.getElementById('adm-vis').value;

  state.filteredEntries = state.entries.filter((e) => {
    if (plat && e.platform !== plat) return false;
    if (vis === 'published' && !e.is_published) return false;
    if (vis === 'hidden' && e.is_published) return false;
    if (!q) return true;
    const hay = [
      normalizeSearchKey(e.title || ''),
      normalizeSearchKey(e.public_comment || ''),
      normalizeSearchKey(e.private_notes || ''),
      normalizeSearchKey(e.creators?.display_name || ''),
      e.canonical_url.toLowerCase(),
      ...e.occurrences.map((o) => normalizeSearchKey(o.moveName)),
    ].join(' ');
    return hay.includes(q);
  });

  const list = document.getElementById('adm-entry-list');
  list.replaceChildren();
  for (const e of state.filteredEntries) {
    const chips = el('div', { className: 'chip-list' });
    for (const occ of e.occurrences) {
      chips.appendChild(
        el('span', { className: 'move-chip' }, `${occ.moveName}${occ.start_ms !== null ? ` (${formatMs(occ.start_ms)})` : ''}`)
      );
    }
    list.appendChild(
      el(
        'button',
        {
          type: 'button',
          className: `entry-card ${state.selectedEntry?.id === e.id ? 'selected' : ''}`,
          onclick: () => selectAdminEntry(e.id),
        },
        el(
          'div',
          { className: 'entry-card-top' },
          el('span', { className: 'badge badge-platform' }, e.platform),
          el('span', { className: e.is_published ? 'badge' : 'badge badge-warning' }, e.is_published ? 'Published' : 'Hidden')
        ),
        el('div', { className: 'entry-card-title' }, e.title || e.canonical_url),
        el('div', { className: 'helper-text' }, e.creators?.display_name || 'Unknown creator'),
        chips
      )
    );
  }
}

function selectAdminEntry(id) {
  if (state.selectedEntry?.id === id) return;
  state.selectedEntry = state.entries.find((x) => x.id === id) || null;
  applyCatalogFilters();
  renderDetail(false);
}

// --- Full Entry & Occurrence Editor ---

function renderDetail(preservePlayer = false) {
  const emptyEl = document.getElementById('adm-empty-detail');
  const pane = document.getElementById('adm-detail-card');
  const entry = state.selectedEntry;

  if (!entry) {
    if (player) player.destroy();
    emptyEl.classList.remove('hidden');
    pane.classList.add('hidden');
    return;
  }

  emptyEl.classList.add('hidden');
  pane.classList.remove('hidden');

  const desc = buildPlaybackDescriptor(entry);
  if (!preservePlayer || !player) {
    if (player) player.destroy();
    pane.replaceChildren();
    const stage = el('div', { className: 'player-stage', id: 'adm-player-stage' });
    pane.appendChild(stage);
    player = new PlayerBridge(stage, (ms) => {
      const clk = document.getElementById('adm-live-clock');
      if (clk) clk.textContent = formatMs(ms, false);
    });
    player.mount(desc);
  } else {
    while (pane.children.length > 1) pane.removeChild(pane.lastChild);
  }

  // Timing Toolbar & Add New Occurrence
  const startIn = el('input', { id: 'new-occ-start', placeholder: 'Start (MM:SS.mmm or sec)', 'aria-label': 'Start timestamp' });
  const endIn = el('input', { id: 'new-occ-end', placeholder: 'End (MM:SS.mmm or sec)', 'aria-label': 'End timestamp' });
  const methodIn = el('input', { type: 'hidden', value: 'manual' });

  if (entry.observed_offset_ms !== null && entry.occurrences.length === 0) {
    startIn.value = formatMs(entry.observed_offset_ms);
  }

  const canMark = player.supportsTimeCapture();
  const canSeek = player.supportsSeek();

  pane.appendChild(
    el(
      'div',
      { className: 'timing-toolbar' },
      el('span', { className: 'clock-pill', id: 'adm-live-clock' }, canMark ? '00:00.000' : 'Manual time'),
      el(
        'button',
        {
          type: 'button',
          id: 'adm-mark-start',
          className: 'btn btn-secondary btn-sm',
          disabled: !canMark,
          onclick: () => {
            const ms = player.getCurrentTimeMs();
            if (ms === null) return toast('Start playback first to capture current timestamp.', true);
            startIn.value = formatMs(ms, false);
            methodIn.value = 'player_mark';
          },
        },
        'Mark Start ['
      ),
      el(
        'button',
        {
          type: 'button',
          id: 'adm-mark-end',
          className: 'btn btn-secondary btn-sm',
          disabled: !canMark,
          onclick: () => {
            const ms = player.getCurrentTimeMs();
            if (ms === null) return toast('Start playback first to capture current timestamp.', true);
            endIn.value = formatMs(ms, false);
            methodIn.value = 'player_mark';
          },
        },
        'Mark End ]'
      ),
      el(
        'button',
        {
          type: 'button',
          id: 'adm-preview-seg',
          className: 'btn btn-secondary btn-sm',
          disabled: !canSeek,
          onclick: () => {
            try {
              const sMs = parseTimestampMs(startIn.value);
              const eMs = parseTimestampMs(endIn.value);
              if (sMs === null) return toast('Enter or mark a start time first.', true);
              player.seekSegment(sMs, eMs);
            } catch (err) {
              toast(err.message, true);
            }
          },
        },
        'Preview Segment (P)'
      ),
      el('a', { href: entry.original_url, target: '_blank', rel: 'noopener noreferrer', className: 'btn btn-ghost btn-sm' }, 'Open Original ↗')
    )
  );

  // AI Metadata & Vuelta Extraction Assistant (Copy Prompt -> ChatGPT/Claude/Local Agent -> Paste JSON & Save)
  pane.appendChild(renderAIExtractionPanel(entry));

  // Add Occurrence Form
  const moveSelect = el('select', { 'aria-label': 'Canonical move' }, el('option', { value: '' }, '— Select canonical move —'));
  for (const m of state.moves) {
    moveSelect.appendChild(el('option', { value: m.id }, m.canonical_name));
  }
  const propMoveIn = el('input', { placeholder: 'Or enter new Spanish move name…' });
  const occNoteIn = el('input', { placeholder: 'Segment note / comment (optional)' });

  pane.appendChild(
    el(
      'form',
      {
        onsubmit: async (ev) => {
          ev.preventDefault();
          try {
            const sMs = parseTimestampMs(startIn.value);
            const eMs = parseTimestampMs(endIn.value);
            validateBounds(sMs, eMs, entry.duration_ms);
            let moveId = moveSelect.value || null;
            const propText = propMoveIn.value.trim();
            if (!moveId && !propText) throw new Error('Choose a canonical move or enter a new move name.');

            if (!moveId && propText) {
              moveId = await ensureMoveByName(propText);
            }
            const { error } = await state.sb.from('occurrences').insert({
              entry_id: entry.id,
              move_id: moveId,
              proposed_name: propText || null,
              start_ms: sMs,
              end_ms: eMs,
              ordinal: entry.occurrences.length + 1,
              notes: occNoteIn.value.trim(),
              state: 'confirmed',
              timing_method: methodIn.value,
            });
            if (error) throw new Error(error.message);
            await refreshAll(entry.id);
            toast('Move occurrence added.');
          } catch (err) {
            toast(err.message, true);
          }
        },
      },
      el('h3', { style: 'margin:0 0 0.45rem' }, 'Add / Update Demonstrated Moves & Time Ranges'),
      el('div', { className: 'grid-2' }, moveSelect, propMoveIn),
      el('div', { className: 'grid-2', style: 'margin-top:0.5rem' }, startIn, endIn),
      el('div', { className: 'inline-actions' }, occNoteIn, el('button', { type: 'submit', className: 'btn btn-primary' }, '+ Add Move Segment'))
    )
  );

  // Editable Existing Occurrences List (Edit move, start, end, notes inline + Delete)
  const occBox = el('div', { style: 'margin-top:0.8rem' });
  for (const occ of entry.occurrences) {
    const editMoveSel = el('select', {});
    for (const m of state.moves) {
      editMoveSel.appendChild(el('option', { value: m.id, selected: m.id === occ.move_id }, m.canonical_name));
    }
    const editStart = el('input', { value: occ.start_ms !== null ? formatMs(occ.start_ms, false) : '', placeholder: 'Start MM:SS.mmm', style: 'width:130px' });
    const editEnd = el('input', { value: occ.end_ms !== null ? formatMs(occ.end_ms, false) : '', placeholder: 'End MM:SS.mmm', style: 'width:130px' });
    const editNote = el('input', { value: occ.notes || '', placeholder: 'Segment comment / cue', style: 'flex:1 1 180px' });

    occBox.appendChild(
      el(
        'div',
        { className: 'occ-row' },
        el(
          'div',
          { className: 'inline-actions', style: 'width:100%;margin-top:0' },
          editMoveSel,
          editStart,
          editEnd,
          editNote,
          canSeek && occ.start_ms !== null
            ? el('button', { type: 'button', className: 'btn btn-secondary btn-sm', onclick: () => player.seekSegment(occ.start_ms, occ.end_ms) }, '▶')
            : null,
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-secondary btn-sm',
              onclick: async () => {
                try {
                  const sMs = parseTimestampMs(editStart.value);
                  const eMs = parseTimestampMs(editEnd.value);
                  validateBounds(sMs, eMs, entry.duration_ms);
                  const { error } = await state.sb
                    .from('occurrences')
                    .update({
                      move_id: editMoveSel.value || null,
                      start_ms: sMs,
                      end_ms: eMs,
                      notes: editNote.value.trim(),
                    })
                    .eq('id', occ.id);
                  if (error) throw new Error(error.message);
                  await refreshAll(entry.id);
                  toast('Occurrence updated.');
                } catch (err) {
                  toast(err.message, true);
                }
              },
            },
            'Update'
          ),
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-danger btn-sm',
              onclick: async () => {
                const { error } = await state.sb.from('occurrences').delete().eq('id', occ.id);
                if (error) return toast(error.message, true);
                await refreshAll(entry.id);
                toast('Occurrence deleted.');
              },
            },
            '✕'
          )
        )
      )
    );
  }
  pane.appendChild(occBox);

  // Full Video Metadata, Comments, Creator Attribution, Cloud Video URL & Visibility Form
  const curIdent = (entry.creators?.creator_identities || []).find((i) => i.platform === entry.platform) || entry.creators?.creator_identities?.[0] || {};
  const titleIn = el('input', { value: entry.title || '', placeholder: 'Video title' });
  const pubCommentIn = el('textarea', { rows: 2, placeholder: 'Public comment / curator note shown on the landing page' }, entry.public_comment || '');
  const privNotesIn = el('textarea', { rows: 2, placeholder: 'Private admin notes (stored in admin_entry_notes — NEVER exposed publicly)' }, entry.private_notes || '');
  const creatorNameIn = el('input', { value: entry.creators?.display_name || '', placeholder: 'Creator display name' });
  const creatorRoleSel = el(
    'select',
    {},
    el('option', { value: 'uploader', selected: entry.creator_role !== 'original' }, 'Uploader account'),
    el('option', { value: 'original', selected: entry.creator_role === 'original' }, 'Original choreographer / dancer')
  );
  const creatorHandleIn = el('input', { value: curIdent.handle || '', placeholder: '@handle' });
  const creatorUrlIn = el('input', { value: curIdent.profile_url || '', placeholder: 'https://... creator profile URL' });
  const creatorVerifiedChk = el('input', { type: 'checkbox', checked: Boolean(curIdent.verified) });
  const durationIn = el('input', { value: entry.duration_ms ? formatMs(entry.duration_ms, false) : '', placeholder: 'Duration MM:SS (optional)' });
  const availSel = el(
    'select',
    {},
    ...['available', 'restricted', 'private', 'login_required', 'deleted'].map((st) =>
      el('option', { value: st, selected: entry.availability_state === st }, st)
    )
  );
  const cloudVideoUrlIn = el('input', { value: entry.public_video_url || '', placeholder: 'Optional Cloudflare R2 / Supabase Storage MP4 URL for direct HTML5 playback' });
  const publishedChk = el('input', { type: 'checkbox', checked: Boolean(entry.is_published) });

  pane.appendChild(
    el(
      'form',
      {
        className: 'sub-section',
        onsubmit: async (ev) => {
          ev.preventDefault();
          try {
            const durMs = parseTimestampMs(durationIn.value);
            let creatorId = entry.creator_id;
            if (creatorNameIn.value.trim()) {
              creatorId = await upsertCreatorWithIdentity({
                displayName: creatorNameIn.value.trim(),
                platform: entry.platform,
                handle: creatorHandleIn.value.trim(),
                profileUrl: creatorUrlIn.value.trim(),
                verified: creatorVerifiedChk.checked,
              });
            }

            // Optimistic concurrency check on revision
            const { data: updatedRows, error: upErr } = await state.sb
              .from('entries')
              .update({
                title: titleIn.value.trim() || null,
                public_comment: pubCommentIn.value.trim(),
                creator_id: creatorId,
                creator_role: creatorRoleSel.value,
                duration_ms: durMs,
                availability_state: availSel.value,
                embed_eligible: availSel.value === 'available',
                public_video_url: cloudVideoUrlIn.value.trim() || null,
                is_published: publishedChk.checked,
              })
              .eq('id', entry.id)
              .eq('revision', entry.revision)
              .select('id');

            if (upErr) throw new Error(upErr.message);
            if (!updatedRows || updatedRows.length === 0) {
              await refreshAll(entry.id);
              throw new Error('Revision conflict: this entry was modified elsewhere. Reloaded latest version.');
            }

            const { error: noteErr } = await state.sb.from('admin_entry_notes').upsert({
              entry_id: entry.id,
              private_notes: privNotesIn.value,
            });
            if (noteErr) throw new Error(noteErr.message);

            await refreshAll(entry.id);
            toast(`Entry saved (v${entry.revision + 1}).`);
          } catch (err) {
            toast(err.message, true);
          }
        },
      },
      el('h3', { style: 'margin:0 0 0.5rem' }, `Video Details, Comments & Attribution (Rev v${entry.revision})`),
      el('label', { className: 'field-label' }, 'Title'),
      titleIn,
      el('label', { className: 'field-label' }, 'Public Comment / Description (Visible on Landing Page)'),
      pubCommentIn,
      el('label', { className: 'field-label' }, 'Private Practice / Curation Notes (Admin-Only)'),
      privNotesIn,
      el('div', { className: 'grid-2' },
        el('div', {}, el('label', { className: 'field-label' }, 'Creator Name'), creatorNameIn),
        el('div', {}, el('label', { className: 'field-label' }, 'Creator Role'), creatorRoleSel)
      ),
      el('div', { className: 'grid-2' },
        el('div', {}, el('label', { className: 'field-label' }, 'Creator @Handle'), creatorHandleIn),
        el('div', {}, el('label', { className: 'field-label' }, 'Creator Profile URL'), creatorUrlIn)
      ),
      el('label', { className: 'inline-actions helper-text' }, creatorVerifiedChk, ' Verified creator identity (permits @mention when CLI/Wails/iOS publishes to @cubansocial.sd)'),
      el('div', { className: 'grid-2' },
        el('div', {}, el('label', { className: 'field-label' }, 'Duration (MM:SS)'), durationIn),
        el('div', {}, el('label', { className: 'field-label' }, 'Source Availability'), availSel)
      ),
      el('label', { className: 'field-label' }, 'Cloudflare R2 / Supabase Authorized Video URL (Optional)'),
      cloudVideoUrlIn,
      el('label', { className: 'inline-actions helper-text' }, publishedChk, ' Published on public landing page'),
      el(
        'div',
        { className: 'inline-actions', style: 'margin-top:0.85rem' },
        el('button', { type: 'submit', className: 'btn btn-primary' }, 'Save Video Updates'),
        el(
          'button',
          {
            type: 'button',
            className: 'btn btn-danger',
            onclick: async () => {
              if (!window.confirm('Permanently delete this video entry and its occurrences?')) return;
              const { error } = await state.sb.from('entries').delete().eq('id', entry.id);
              if (error) return toast(error.message, true);
              state.selectedEntry = null;
              await refreshAll();
              renderDetail(false);
              toast('Video entry deleted.');
            },
          },
          'Delete Video Entry'
        )
      )
    )
  );

  // Rights Clearance Section (Used by CLI / Wails / VueltariOS when publishing to @cubansocial.sd)
  const existingRights = entry.rights_records?.[0] || null;
  const rGrantor = el('input', { value: existingRights?.grantor || '', placeholder: 'Grantor (Creator / Rights holder)', required: true });
  const rBasis = el('input', { value: existingRights?.basis || '', placeholder: 'Written permission / DM / License ref', required: true });
  const rVid = el('input', { type: 'checkbox', checked: Boolean(existingRights?.video_cleared) });
  const rMus = el('input', { type: 'checkbox', checked: Boolean(existingRights?.music_cleared) });

  pane.appendChild(
    el(
      'form',
      {
        className: 'sub-section',
        onsubmit: async (ev) => {
          ev.preventDefault();
          const payload = {
            entry_id: entry.id,
            grantor: rGrantor.value.trim(),
            basis: rBasis.value.trim(),
            video_cleared: rVid.checked,
            music_cleared: rMus.checked,
            authorized_uses: ['republish'],
            authorized_platforms: ['instagram'],
          };
          const { error } = existingRights
            ? await state.sb.from('rights_records').update(payload).eq('id', existingRights.id)
            : await state.sb.from('rights_records').insert(payload);
          if (error) return toast(error.message, true);
          await refreshAll(entry.id);
          toast('Rights clearance updated for CLI / Wails / iOS publishing.');
        },
      },
      el('h3', { style: 'margin:0 0 0.4rem' }, 'Rights Clearance (For CLI / Wails / iOS Instagram Publishing)'),
      el('div', { className: 'grid-2' }, rGrantor, rBasis),
      el(
        'div',
        { className: 'inline-actions' },
        el('label', { className: 'inline-actions helper-text' }, rVid, ' Video cleared'),
        el('label', { className: 'inline-actions helper-text' }, rMus, ' Music cleared'),
        el('button', { type: 'submit', className: 'btn btn-secondary btn-sm' }, 'Save Rights Record')
      )
    )
  );
}

// --- AI Extraction Assistant Panel (Option 1: Prompt Generator + Reviewed JSON Importer) ---

function renderAIExtractionPanel(entry) {
  const details = el('details', {
    className: 'sub-section',
    style: 'background:var(--surface-alt);padding:0.75rem;border-radius:8px;margin-bottom:0.95rem;border-top:0;margin-top:0',
  });
  const summary = el(
    'summary',
    { style: 'cursor:pointer;font-weight:700;color:var(--accent)' },
    '✨ AI Metadata & Vuelta Extractor (ChatGPT / Claude / Local Agent)'
  );
  details.appendChild(summary);

  const extraCtxIn = el('textarea', {
    rows: 2,
    placeholder: 'Optional: paste raw post caption, comments, or notes here to include in the prompt…',
  });
  const promptOut = el(
    'textarea',
    {
      rows: 6,
      readOnly: true,
      style: 'font-family:var(--font-mono);font-size:0.78rem;margin-top:0.4rem',
      'aria-label': 'Generated AI prompt',
    },
    buildAIExtractionPrompt(entry, state.moves, '')
  );

  extraCtxIn.addEventListener('input', () => {
    promptOut.value = buildAIExtractionPrompt(entry, state.moves, extraCtxIn.value);
  });

  const btnCopyPrompt = el(
    'button',
    {
      type: 'button',
      className: 'btn btn-secondary btn-sm',
      onclick: async () => {
        promptOut.value = buildAIExtractionPrompt(entry, state.moves, extraCtxIn.value);
        try {
          await navigator.clipboard.writeText(promptOut.value);
          toast('AI prompt copied! Paste it into ChatGPT, Claude, or your local agent (+ attach screenshot if on-screen text is visible).');
        } catch {
          promptOut.select();
          toast('Prompt selected — press Cmd+C / Ctrl+C to copy.');
        }
      },
    },
    '📋 Copy AI Prompt'
  );

  const jsonInput = el('textarea', {
    rows: 5,
    placeholder: 'Paste the JSON response from ChatGPT / Claude / local agent here…',
    style: 'font-family:var(--font-mono);font-size:0.8rem',
    'aria-label': 'AI JSON response',
  });
  const replaceOccsChk = el('input', { type: 'checkbox', checked: false });
  const previewBox = el('div', { className: 'helper-text', style: 'margin-top:0.45rem' });

  let parsedPreview = null;

  const renderPreview = () => {
    previewBox.replaceChildren();
    if (!jsonInput.value.trim()) {
      parsedPreview = null;
      return;
    }
    try {
      parsedPreview = parseAIExtractionJSON(jsonInput.value, state.moves);
      const summaryLines = [];
      if (parsedPreview.title) summaryLines.push(`Title: ${parsedPreview.title}`);
      if (parsedPreview.creatorName) {
        summaryLines.push(
          `Creator: ${parsedPreview.creatorName}${parsedPreview.creatorHandle ? ` (@${parsedPreview.creatorHandle})` : ''} [${parsedPreview.creatorRole}]`
        );
      }
      if (parsedPreview.durationMs !== null) summaryLines.push(`Duration: ${formatMs(parsedPreview.durationMs)}`);

      const moveBadges = el('div', { className: 'chip-list' });
      for (const occ of parsedPreview.occurrences) {
        const timePart = occ.startMs !== null ? ` (${formatMs(occ.startMs)}${occ.endMs !== null ? '–' + formatMs(occ.endMs) : ''})` : '';
        moveBadges.appendChild(
          el(
            'span',
            { className: occ.isNewMove ? 'badge badge-warning' : 'move-chip' },
            `${occ.isNewMove ? '+ New Move: ' : '✓ '}${occ.moveName}${timePart}`
          )
        );
      }

      previewBox.appendChild(
        el(
          'div',
          { style: 'padding:0.5rem;background:#fff;border:1px solid var(--border);border-radius:6px' },
          el('strong', {}, `Parsed OK — ${parsedPreview.occurrences.length} vuelta(s) ready to apply:`),
          summaryLines.length ? el('div', {}, summaryLines.join(' · ')) : null,
          moveBadges
        )
      );
    } catch (err) {
      parsedPreview = null;
      previewBox.appendChild(el('span', { style: 'color:var(--danger);font-weight:600' }, err.message));
    }
  };

  jsonInput.addEventListener('input', renderPreview);

  const btnApplyAI = el(
    'button',
    {
      type: 'button',
      className: 'btn btn-primary btn-sm',
      onclick: async () => {
        try {
          const result = parsedPreview || parseAIExtractionJSON(jsonInput.value, state.moves);
          await applyAIExtractionToSupabase(entry, result, replaceOccsChk.checked);
          jsonInput.value = '';
          previewBox.replaceChildren();
          toast(`Applied AI metadata & ${result.occurrences.length} vuelta(s) to Supabase.`);
        } catch (err) {
          toast(err.message, true);
        }
      },
    },
    '✓ Apply AI JSON & Save to DB'
  );

  details.appendChild(
    el(
      'div',
      { style: 'margin-top:0.65rem' },
      el('p', { className: 'helper-text' }, '1. Copy this taxonomy-aware prompt and send it to ChatGPT, Claude, or a local vision agent (attach a screenshot of the video if moves/creator appear as on-screen text).'),
      extraCtxIn,
      promptOut,
      el('div', { className: 'inline-actions' }, btnCopyPrompt),
      el('p', { className: 'helper-text', style: 'margin-top:0.75rem' }, '2. Paste the returned JSON below, review the matched/new moves, and save directly to Supabase:'),
      jsonInput,
      previewBox,
      el(
        'div',
        { className: 'inline-actions' },
        el('label', { className: 'inline-actions helper-text' }, replaceOccsChk, ' Replace existing occurrences on this video (instead of appending)'),
        btnApplyAI
      )
    )
  );

  return details;
}

async function applyAIExtractionToSupabase(entry, aiData, replaceExistingOccurrences) {
  let creatorId = entry.creator_id;
  if (aiData.creatorName) {
    creatorId = await upsertCreatorWithIdentity({
      displayName: aiData.creatorName,
      platform: entry.platform,
      handle: aiData.creatorHandle || '',
      profileUrl: aiData.creatorProfileUrl || '',
      verified: false,
    });
  }

  const updateFields = {};
  if (aiData.title) updateFields.title = aiData.title;
  if (creatorId) {
    updateFields.creator_id = creatorId;
    updateFields.creator_role = aiData.creatorRole || 'uploader';
  }
  if (aiData.durationMs !== null) updateFields.duration_ms = aiData.durationMs;
  if (aiData.publicComment !== null) updateFields.public_comment = aiData.publicComment;

  if (Object.keys(updateFields).length > 0) {
    const { error: upErr } = await state.sb.from('entries').update(updateFields).eq('id', entry.id);
    if (upErr) throw new Error(upErr.message);
  }

  if (aiData.privateNotes) {
    const mergedNotes = entry.private_notes
      ? `${entry.private_notes}\n[AI]: ${aiData.privateNotes}`
      : aiData.privateNotes;
    const { error: nErr } = await state.sb.from('admin_entry_notes').upsert({
      entry_id: entry.id,
      private_notes: mergedNotes,
    });
    if (nErr) throw new Error(nErr.message);
  }

  if (replaceExistingOccurrences && aiData.occurrences.length > 0) {
    const { error: delErr } = await state.sb.from('occurrences').delete().eq('entry_id', entry.id);
    if (delErr) throw new Error(delErr.message);
  }

  let baseOrdinal = replaceExistingOccurrences ? 0 : entry.occurrences.length;
  for (const occ of aiData.occurrences) {
    baseOrdinal += 1;
    const moveId = await ensureMoveByName(occ.moveName);
    const { error: occErr } = await state.sb.from('occurrences').insert({
      entry_id: entry.id,
      move_id: moveId,
      proposed_name: occ.isNewMove ? occ.moveName : null,
      start_ms: occ.startMs,
      end_ms: occ.endMs,
      ordinal: baseOrdinal,
      notes: occ.notes || '',
      state: 'confirmed',
      timing_method: occ.startMs !== null ? 'suggested' : 'unknown',
    });
    if (occErr) throw new Error(occErr.message);
  }

  await refreshAll(entry.id);
}

// --- Taxonomy Helpers & Editor ---

async function ensureMoveByName(rawName) {
  const norm = normalizeSearchKey(rawName);
  const existing = state.moves.find(
    (m) => m.normalized_search_key === norm || (m.aliases || []).some((a) => a.normalized_alias === norm)
  );
  if (existing) return existing.id;

  const token = portableMoveToken(rawName);
  const { data, error } = await state.sb
    .from('moves')
    .insert({
      canonical_name: rawName.trim(),
      normalized_search_key: norm,
      portable_token: token,
      style_tags: ['casino'],
      kind: 'move',
      curation_state: 'curated',
    })
    .select('id')
    .single();
  if (error) throw new Error(error.message);
  return data.id;
}

function populateComboStepPicker() {
  const picker = document.getElementById('nm-step-picker');
  picker.replaceChildren();
  for (const m of state.moves) {
    picker.appendChild(el('option', { value: m.id }, m.canonical_name));
  }
}

function renderTaxonomyList() {
  const q = normalizeSearchKey(document.getElementById('tax-filter').value);
  const box = document.getElementById('tax-list');
  box.replaceChildren();

  for (const m of state.moves) {
    if (q && !normalizeSearchKey(m.canonical_name).includes(q) && !(m.aliases || []).some((a) => a.normalized_alias.includes(q))) {
      continue;
    }
    const aliasChips = el('div', { className: 'chip-list' });
    for (const al of m.aliases || []) {
      aliasChips.appendChild(
        el(
          'span',
          { className: al.collision_flagged ? 'badge badge-warning' : 'badge' },
          `${al.collision_flagged ? '⚠ ' : ''}${al.alias}${al.qualifier ? ` (${al.qualifier})` : ''}`
        )
      );
    }

    const aliasIn = el('input', { placeholder: 'New alias…', style: 'width:150px' });
    const qualIn = el('input', { placeholder: 'Region/school', style: 'width:130px' });

    box.appendChild(
      el(
        'div',
        { className: 'occ-row' },
        el(
          'div',
          { style: 'width:100%' },
          el(
            'div',
            { className: 'entry-card-top' },
            el('strong', {}, `${m.canonical_name} (#CubanSocial-${m.portable_token})`),
            el(
              'button',
              {
                type: 'button',
                className: 'btn btn-danger btn-sm',
                onclick: async () => {
                  if (!window.confirm(`Delete move "${m.canonical_name}"?`)) return;
                  const { error } = await state.sb.from('moves').delete().eq('id', m.id);
                  if (error) return toast(error.message, true);
                  await refreshAll();
                },
              },
              'Delete'
            )
          ),
          aliasChips,
          el(
            'form',
            {
              className: 'inline-actions',
              onsubmit: async (ev) => {
                ev.preventDefault();
                const normAl = normalizeSearchKey(aliasIn.value);
                if (!normAl) return;
                const collides = state.moves.some(
                  (other) =>
                    other.id !== m.id &&
                    (other.normalized_search_key === normAl || (other.aliases || []).some((a) => a.normalized_alias === normAl))
                );
                const { error } = await state.sb.from('move_aliases').insert({
                  move_id: m.id,
                  alias: aliasIn.value.trim(),
                  normalized_alias: normAl,
                  language: 'es',
                  qualifier: qualIn.value.trim(),
                  collision_flagged: collides,
                });
                if (error) return toast(error.message, true);
                await refreshAll();
                toast(collides ? 'Alias added and flagged for collision review.' : 'Alias saved.');
              },
            },
            aliasIn,
            qualIn,
            el('button', { type: 'submit', className: 'btn btn-secondary btn-sm' }, '+ Alias')
          )
        )
      )
    );
  }
}

// --- Static Event Wiring ---

function wireStaticEvents() {
  document.getElementById('form-admin-login').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    try {
      state.sb = await getSupabaseClient();
      const { data, error } = await state.sb.auth.signInWithPassword({
        email: document.getElementById('admin-email').value.trim(),
        password: document.getElementById('admin-password').value,
      });
      if (error) throw error;
      state.session = data.session;
      document.getElementById('admin-password').value = '';
      await verifyAdminAndBoot();
    } catch (err) {
      toast(err.message || 'Sign-in failed', true);
    }
  });

  document.getElementById('btn-admin-signout').addEventListener('click', async () => {
    if (state.sb) await state.sb.auth.signOut();
    state.session = null;
    state.adminRecord = null;
    showViewState('login');
  });

  for (const tab of document.querySelectorAll('#admin-nav .nav-tab')) {
    tab.addEventListener('click', () => {
      for (const t of document.querySelectorAll('#admin-nav .nav-tab')) t.classList.toggle('active', t === tab);
      for (const name of ['catalog', 'taxonomy', 'backup']) {
        document.getElementById(`section-${name}`).classList.toggle('hidden', tab.dataset.section !== name);
      }
    });
  }

  document.getElementById('form-ingest-link').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const input = document.getElementById('ingest-url-input');
    try {
      const res = await ingestSingleUrl(input.value);
      input.value = '';
      toast(res.status === 'duplicate' ? 'Opened existing video entry.' : `Saved ${res.platform} video entry.`);
    } catch (err) {
      // Preserve input value on unsupported/invalid URL (ING-01)
      toast(err.message, true);
    }
  });

  document.getElementById('adm-q').addEventListener('input', () => applyCatalogFilters());
  document.getElementById('adm-platform').addEventListener('change', () => applyCatalogFilters());
  document.getElementById('adm-vis').addEventListener('change', () => applyCatalogFilters());
  document.getElementById('btn-adm-reload').addEventListener('click', () => refreshAll());
  document.getElementById('tax-filter').addEventListener('input', () => renderTaxonomyList());

  document.getElementById('nm-kind').addEventListener('change', (ev) => {
    document.getElementById('nm-combo-box').classList.toggle('hidden', ev.target.value !== 'combination');
  });

  document.getElementById('btn-nm-add-step').addEventListener('click', () => {
    const picker = document.getElementById('nm-step-picker');
    const mv = state.moves.find((m) => m.id === picker.value);
    if (!mv) return;
    state.comboStepsDraft.push(mv);
    document.getElementById('nm-step-list').replaceChildren(...state.comboStepsDraft.map((s) => el('li', {}, s.canonical_name)));
  });

  document.getElementById('form-new-move').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    try {
      const name = document.getElementById('nm-name').value.trim();
      const norm = normalizeSearchKey(name);
      const baseTok = portableMoveToken(name);
      const collisionCount = state.moves.filter((m) => m.portable_token === baseTok).length;
      const disamb = collisionCount > 0 ? `V${collisionCount + 1}` : '';
      const token = portableMoveToken(name, disamb);
      const kind = document.getElementById('nm-kind').value;
      const styles = document.getElementById('nm-styles').value.split(',').map((s) => s.trim()).filter(Boolean);

      const { data: created, error } = await state.sb
        .from('moves')
        .insert({
          canonical_name: name,
          normalized_search_key: norm,
          portable_token: token,
          disambiguator: disamb,
          style_tags: styles,
          kind,
          curation_state: 'curated',
          description: document.getElementById('nm-desc').value.trim(),
        })
        .select('id')
        .single();
      if (error) throw new Error(error.message);

      if (kind === 'combination' && state.comboStepsDraft.length > 0) {
        const stepsPayload = state.comboStepsDraft.map((st, idx) => ({
          combination_move_id: created.id,
          ordinal: idx + 1,
          child_move_id: st.id,
        }));
        const { error: stErr } = await state.sb.from('combination_steps').insert(stepsPayload);
        if (stErr) throw new Error(stErr.message);
      }

      document.getElementById('nm-name').value = '';
      document.getElementById('nm-desc').value = '';
      state.comboStepsDraft = [];
      document.getElementById('nm-step-list').replaceChildren();
      await refreshAll();
      toast(`Saved move "${name}".`);
    } catch (err) {
      toast(err.message, true);
    }
  });

  document.getElementById('btn-export-json').addEventListener('click', () => {
    const bundle = {
      schema_version: 'vueltario.supabase.export.v1',
      exported_at: new Date().toISOString(),
      moves: state.moves,
      entries: state.entries,
    };
    const blob = new Blob([JSON.stringify(bundle, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `vueltario-backup-${new Date().toISOString().slice(0, 10)}.json`;
    a.click();
    URL.revokeObjectURL(url);
  });

  document.getElementById('btn-run-batch').addEventListener('click', async () => {
    const lines = document
      .getElementById('batch-links-text')
      .value.split('\n')
      .map((l) => l.trim())
      .filter(Boolean);
    const report = [];
    let ok = 0, dup = 0, fail = 0;
    for (const line of lines) {
      try {
        const res = await ingestSingleUrl(line);
        if (res.status === 'duplicate') dup++;
        else ok++;
        report.push(`[${res.status.toUpperCase()}] ${line}`);
      } catch (err) {
        fail++;
        report.push(`[FAILED] ${line} — ${err.message}`);
      }
    }
    document.getElementById('batch-report').textContent =
      `Created: ${ok} · Duplicates: ${dup} · Failed: ${fail}\n` + report.join('\n');
  });

  // Keyboard timing shortcuts outside form inputs
  document.addEventListener('keydown', (ev) => {
    if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
    const tag = document.activeElement?.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    if (!state.selectedEntry) return;
    if (ev.key === '[') {
      ev.preventDefault();
      document.getElementById('adm-mark-start')?.click();
    } else if (ev.key === ']') {
      ev.preventDefault();
      document.getElementById('adm-mark-end')?.click();
    } else if (ev.key === 'p' || ev.key === 'P') {
      ev.preventDefault();
      document.getElementById('adm-preview-seg')?.click();
    }
  });
}

init();
