// Render-Only Public Landing Page Controller (site/index.html)
// Strictly read-only: NO sign-in button, NO admin link, NO mutation forms.

import {
  getSupabaseClient,
  el,
  formatMs,
  normalizeSearchKey,
  buildPlaybackDescriptor,
} from './utils.js';
import { PlayerBridge } from './players.js';

const state = {
  moves: [],
  entries: [],
  filteredEntries: [],
  selectedEntry: null,
};

let player = null;

async function init() {
  wireEvents();
  await loadCatalog();
  populateMoveDropdown();
  applyFilters();
  renderDictionary();
}

async function loadCatalog() {
  const sb = await getSupabaseClient();
  if (sb) {
    const [movesRes, entriesRes] = await Promise.all([
      sb
        .from('moves')
        .select('id, canonical_name, normalized_search_key, portable_token, style_tags, kind, description, move_aliases(alias, qualifier), combination_steps!combination_steps_combination_move_id_fkey(ordinal, child_move_id, notes)')
        .eq('curation_state', 'curated')
        .order('canonical_name'),
      sb
        .from('entries')
        .select('id, platform, platform_content_id, canonical_url, original_url, title, description, public_comment, creator_role, duration_ms, availability_state, embed_eligible, public_video_url, public_video_mime, creators(display_name, creator_identities(platform, handle, profile_url, verified)), occurrences(id, move_id, proposed_name, start_ms, end_ms, ordinal, notes, state, moves(canonical_name)), media_assets(status, public_or_cdn_url, access_scope)')
        .eq('is_published', true)
        .order('updated_at', { ascending: false }),
    ]);

    if (!movesRes.error) {
      state.moves = (movesRes.data || []).map((m) => ({
        ...m,
        aliases: m.move_aliases || [],
        steps: m.combination_steps || [],
      }));
    }
    if (!entriesRes.error) {
      state.entries = (entriesRes.data || []).map(normalizeEntryRow);
      return;
    }
  }

  // Graceful static fallback when Supabase URL/anon key have not been configured yet
  try {
    const resp = await fetch('./data/fallback-catalog.json');
    if (resp.ok) {
      const bundle = await resp.json();
      state.moves = bundle.moves || [];
      state.entries = (bundle.entries || []).map(normalizeEntryRow);
    }
  } catch {
    state.moves = [];
    state.entries = [];
  }
}

function normalizeEntryRow(row) {
  const occs = (row.occurrences || [])
    .filter((o) => o.state === 'confirmed' || o.state === 'curated' || !o.state)
    .sort((a, b) => (a.ordinal || 0) - (b.ordinal || 0))
    .map((o) => ({
      ...o,
      moveName: o.moves?.canonical_name || o.move_name || o.proposed_name || 'Move',
    }));

  const creatorName = row.creators?.display_name || row.creator_name || 'Unknown creator';
  const identities = row.creators?.creator_identities || [];
  const matchIdent = identities.find((i) => i.platform === row.platform) || identities[0] || null;

  return {
    ...row,
    creatorDisplayName: creatorName,
    creatorProfileUrl: matchIdent?.profile_url || '',
    occurrences: occs,
  };
}

function populateMoveDropdown() {
  const sel = document.getElementById('filter-move');
  sel.replaceChildren(el('option', { value: '' }, 'All vueltas'));
  for (const m of state.moves) {
    sel.appendChild(el('option', { value: m.id }, m.canonical_name));
  }
}

function applyFilters() {
  const qNorm = normalizeSearchKey(document.getElementById('filter-q').value);
  const plat = document.getElementById('filter-platform').value;
  const moveId = document.getElementById('filter-move').value;
  const style = document.getElementById('filter-style').value;

  const moveAliasLookup = new Map();
  const moveStyleLookup = new Map();
  for (const m of state.moves) {
    const terms = [m.normalized_search_key || normalizeSearchKey(m.canonical_name)];
    for (const al of m.aliases || []) {
      terms.push(normalizeSearchKey(al.alias));
    }
    moveAliasLookup.set(m.id, terms.join(' '));
    moveStyleLookup.set(m.id, m.style_tags || []);
  }

  state.filteredEntries = state.entries.filter((e) => {
    if (plat && e.platform !== plat) return false;
    if (moveId && !e.occurrences.some((o) => o.move_id === moveId)) return false;
    if (style && !e.occurrences.some((o) => (moveStyleLookup.get(o.move_id) || []).includes(style))) return false;
    if (!qNorm) return true;

    const hayParts = [
      normalizeSearchKey(e.title || ''),
      normalizeSearchKey(e.public_comment || ''),
      normalizeSearchKey(e.creatorDisplayName || ''),
    ];
    for (const o of e.occurrences) {
      hayParts.push(normalizeSearchKey(o.moveName));
      if (o.move_id && moveAliasLookup.has(o.move_id)) {
        hayParts.push(moveAliasLookup.get(o.move_id));
      }
    }
    return hayParts.join(' ').includes(qNorm);
  });

  renderEntryList();
  if (!state.selectedEntry && state.filteredEntries.length > 0) {
    selectEntry(state.filteredEntries[0]);
  } else if (state.selectedEntry && !state.filteredEntries.some((x) => x.id === state.selectedEntry.id)) {
    state.selectedEntry = state.filteredEntries[0] || null;
    renderSelectedDetail();
  }
}

function renderEntryList() {
  const list = document.getElementById('public-entry-list');
  list.replaceChildren();

  if (state.filteredEntries.length === 0) {
    list.appendChild(
      el('div', { className: 'card helper-text' }, 'No published videos match your filter yet.')
    );
    return;
  }

  for (const entry of state.filteredEntries) {
    const chips = el('div', { className: 'chip-list' });
    for (const occ of entry.occurrences) {
      const t = occ.start_ms !== null && occ.start_ms !== undefined ? ` (${formatMs(occ.start_ms)})` : '';
      chips.appendChild(el('span', { className: 'move-chip' }, `${occ.moveName}${t}`));
    }

    const card = el(
      'button',
      {
        type: 'button',
        className: `entry-card ${state.selectedEntry?.id === entry.id ? 'selected' : ''}`,
        onclick: () => selectEntry(entry),
      },
      el(
        'div',
        { className: 'entry-card-top' },
        el('span', { className: 'badge badge-platform' }, entry.platform),
        entry.duration_ms ? el('span', { className: 'helper-text' }, formatMs(entry.duration_ms)) : null
      ),
      el('div', { className: 'entry-card-title' }, entry.title || entry.canonical_url),
      el('div', { className: 'helper-text' }, entry.creatorDisplayName),
      chips
    );
    list.appendChild(card);
  }
}

function selectEntry(entry) {
  state.selectedEntry = entry;
  renderEntryList();
  renderSelectedDetail();
}

function renderSelectedDetail() {
  const emptyEl = document.getElementById('public-empty-state');
  const cardEl = document.getElementById('public-detail-card');
  const entry = state.selectedEntry;

  if (!entry) {
    if (player) player.destroy();
    emptyEl.classList.remove('hidden');
    cardEl.classList.add('hidden');
    return;
  }

  emptyEl.classList.add('hidden');
  cardEl.classList.remove('hidden');
  cardEl.replaceChildren();

  const stage = el('div', { className: 'player-stage' });
  cardEl.appendChild(stage);

  if (player) player.destroy();
  player = new PlayerBridge(stage);
  const desc = buildPlaybackDescriptor(entry);
  player.mount(desc);

  // Attribution & Original Link (META-03)
  cardEl.appendChild(
    el(
      'div',
      { className: 'attribution-bar' },
      el(
        'div',
        {},
        el('strong', {}, entry.title || entry.canonical_url),
        el(
          'div',
          { className: 'helper-text' },
          `Creator: ${entry.creatorDisplayName}${entry.creator_role === 'original' ? ' (Original choreographer)' : ''}`
        ),
        desc.precisionNote ? el('div', { className: 'helper-text' }, desc.precisionNote) : null
      ),
      el(
        'div',
        { className: 'inline-actions' },
        entry.creatorProfileUrl
          ? el('a', { href: entry.creatorProfileUrl, target: '_blank', rel: 'noopener noreferrer', className: 'btn btn-secondary btn-sm' }, 'Follow Creator ↗')
          : null,
        el('a', { href: entry.original_url || entry.canonical_url, target: '_blank', rel: 'noopener noreferrer', className: 'btn btn-ghost btn-sm' }, 'Open Original ↗')
      )
    )
  );

  if (entry.public_comment) {
    cardEl.appendChild(el('p', { className: 'helper-text' }, entry.public_comment));
  }

  cardEl.appendChild(el('h3', { style: 'margin:0.8rem 0 0.45rem' }, 'Demonstrated Vueltas'));
  if (entry.occurrences.length === 0) {
    cardEl.appendChild(el('p', { className: 'helper-text' }, 'No move segments listed yet.'));
  }

  const canSeek = player.supportsSeek();
  for (const occ of entry.occurrences) {
    const timeText =
      occ.start_ms !== null && occ.start_ms !== undefined
        ? `${formatMs(occ.start_ms)}${occ.end_ms !== null && occ.end_ms !== undefined ? ' → ' + formatMs(occ.end_ms) : ''}`
        : 'Full clip / untimed';

    cardEl.appendChild(
      el(
        'div',
        { className: 'occ-row' },
        el(
          'div',
          {},
          el('strong', {}, occ.moveName),
          ' ',
          el('span', { className: 'occ-time' }, timeText),
          occ.notes ? el('div', { className: 'helper-text' }, occ.notes) : null
        ),
        canSeek && occ.start_ms !== null && occ.start_ms !== undefined
          ? el(
              'button',
              {
                type: 'button',
                className: 'btn btn-secondary btn-sm',
                onclick: () => player.seekSegment(occ.start_ms, occ.end_ms),
              },
              '▶ Play Segment'
            )
          : null
      )
    );
  }
}

function renderDictionary() {
  const q = normalizeSearchKey(document.getElementById('dict-search').value);
  const box = document.getElementById('public-dictionary-list');
  box.replaceChildren();

  const moveById = new Map(state.moves.map((m) => [m.id, m.canonical_name]));
  const matches = state.moves.filter((m) => {
    if (!q) return true;
    if (normalizeSearchKey(m.canonical_name).includes(q)) return true;
    return (m.aliases || []).some((a) => normalizeSearchKey(a.alias).includes(q));
  });

  for (const m of matches) {
    const aliases = (m.aliases || []).map((a) => (a.qualifier ? `${a.alias} (${a.qualifier})` : a.alias));
    const stepText =
      m.kind === 'combination' && m.steps?.length
        ? 'Combination: ' +
          m.steps
            .sort((a, b) => a.ordinal - b.ordinal)
            .map((s) => moveById.get(s.child_move_id) || 'Move')
            .join(' → ')
        : '';

    box.appendChild(
      el(
        'div',
        { className: 'occ-row' },
        el(
          'div',
          {},
          el('strong', {}, m.canonical_name),
          ' ',
          el('span', { className: 'badge badge-platform' }, m.kind),
          aliases.length ? el('div', { className: 'helper-text' }, `Also called: ${aliases.join(', ')}`) : null,
          stepText ? el('div', { className: 'helper-text' }, stepText) : null,
          m.description ? el('div', { className: 'helper-text' }, m.description) : null
        ),
        el('code', { style: 'font-size:0.8rem' }, `#CubanSocial-${m.portable_token}`)
      )
    );
  }
}

function wireEvents() {
  for (const btn of document.querySelectorAll('.nav-tab')) {
    btn.addEventListener('click', () => {
      for (const b of document.querySelectorAll('.nav-tab')) b.classList.toggle('active', b === btn);
      document.getElementById('view-videos').classList.toggle('hidden', btn.dataset.view !== 'videos');
      document.getElementById('view-dictionary').classList.toggle('hidden', btn.dataset.view !== 'dictionary');
    });
  }

  document.getElementById('public-filter-form').addEventListener('submit', (e) => e.preventDefault());
  document.getElementById('filter-q').addEventListener('input', () => applyFilters());
  document.getElementById('filter-platform').addEventListener('change', () => applyFilters());
  document.getElementById('filter-move').addEventListener('change', () => applyFilters());
  document.getElementById('filter-style').addEventListener('change', () => applyFilters());
  document.getElementById('dict-search').addEventListener('input', () => renderDictionary());
}

init();
