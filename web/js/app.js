import { t } from './i18n.js';
import { apiRequest, setCSRFToken, formatMs } from './api.js';
import { PlayerController } from './players.js';

const state = {
  authenticated: false,
  user: null,
  signupMode: 'invited_moderated',
  activeView: 'personal',
  moves: [],
  entries: [],
  selectedEntry: null,
  playbackDescriptor: null,
  comboDraftSteps: [],
  sse: null,
};

let playerController = null;

// --- Safe DOM Helpers (untrusted text is NEVER passed to innerHTML) ---
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'className') node.className = v;
    else if (k === 'textContent') node.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') {
      node.addEventListener(k.slice(2).toLowerCase(), v);
    } else if (v === true) {
      node.setAttribute(k, '');
    } else {
      node.setAttribute(k, String(v));
    }
  }
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    if (typeof child === 'string' || typeof child === 'number') {
      node.appendChild(document.createTextNode(String(child)));
    } else {
      node.appendChild(child);
    }
  }
  return node;
}

function showBanner(msg, isError = false) {
  const toast = document.getElementById('status-toast');
  toast.textContent = msg;
  toast.classList.toggle('error', isError);
  toast.classList.remove('hidden');
  clearTimeout(showBanner._timer);
  showBanner._timer = setTimeout(() => toast.classList.add('hidden'), 6000);
}

// --- Initialization & Auth ---

async function boot() {
  bindStaticEvents();
  await refreshSession();
  await loadMoves();
  if (state.authenticated) {
    connectSSE();
    await loadEntries();
  } else {
    await loadPublicCatalog();
  }
  renderShell();
}

async function refreshSession() {
  try {
    const me = await apiRequest('/api/v1/auth/me');
    state.authenticated = Boolean(me.authenticated);
    state.user = me.user || null;
    state.signupMode = me.signup_mode || 'invited_moderated';
    setCSRFToken(me.csrf_token || '');
  } catch {
    state.authenticated = false;
    state.user = null;
    setCSRFToken('');
  }
}

function connectSSE() {
  if (state.sse) {
    state.sse.close();
    state.sse = null;
  }
  if (!state.authenticated || typeof EventSource === 'undefined') return;
  const es = new EventSource('/api/v1/events');
  const refreshOnJob = async (ev) => {
    try {
      const payload = JSON.parse(ev.data);
      if (payload.state === 'succeeded' || payload.state === 'unsupported' || payload.state === 'blocked_auth') {
        await loadEntries(true);
        if (state.selectedEntry && (payload.target_id === state.selectedEntry.source_id || payload.target_id === state.selectedEntry.id)) {
          await selectEntry(state.selectedEntry.id, true);
        }
      }
    } catch {
      // Ignore malformed keepalive frames
    }
  };
  es.addEventListener('job.succeeded', refreshOnJob);
  es.addEventListener('job.updated', refreshOnJob);
  state.sse = es;
}

function renderShell() {
  const badge = document.getElementById('user-badge');
  const btnLogin = document.getElementById('btn-open-login');
  const btnLogout = document.getElementById('btn-logout');
  const btnExport = document.getElementById('btn-export');
  const tabAdmin = document.getElementById('tab-admin');
  const guestPrompt = document.getElementById('personal-guest-prompt');
  const workspace = document.getElementById('personal-workspace');

  if (state.authenticated && state.user) {
    badge.textContent = `${state.user.display_name} (${state.user.role})`;
    badge.classList.remove('hidden');
    btnLogin.classList.add('hidden');
    btnLogout.classList.remove('hidden');
    btnExport.classList.remove('hidden');
    guestPrompt.classList.add('hidden');
    workspace.classList.remove('hidden');
    tabAdmin.classList.toggle('hidden', state.user.role !== 'admin');
  } else {
    badge.classList.add('hidden');
    btnLogin.classList.remove('hidden');
    btnLogout.classList.add('hidden');
    btnExport.classList.add('hidden');
    tabAdmin.classList.add('hidden');
    guestPrompt.classList.remove('hidden');
    workspace.classList.add('hidden');
    if (state.activeView === 'admin') {
      switchView('community');
    }
  }
}

function switchView(viewName) {
  state.activeView = viewName;
  for (const btn of document.querySelectorAll('.nav-tab')) {
    btn.classList.toggle('active', btn.dataset.view === viewName);
  }
  for (const sec of document.querySelectorAll('.view-section')) {
    sec.classList.toggle('hidden', sec.id !== `view-${viewName}`);
  }
  if (viewName === 'community') loadPublicCatalog();
  if (viewName === 'taxonomy') renderTaxonomyView();
  if (viewName === 'admin' && state.user?.role === 'admin') loadAdminStudio();
}

// --- Moves & Taxonomy ---

async function loadMoves(query = '') {
  const qs = query ? `?q=${encodeURIComponent(query)}` : '';
  const data = await apiRequest(`/api/v1/moves${qs}`);
  state.moves = data.moves || [];
  populateMoveSelectors();
}

function populateMoveSelectors() {
  const filterSel = document.getElementById('filter-move');
  const curVal = filterSel.value;
  filterSel.replaceChildren(el('option', { value: '' }, t('allMoves')));
  for (const m of state.moves) {
    filterSel.appendChild(el('option', { value: m.id }, m.canonical_name));
  }
  filterSel.value = curVal;

  const comboPicker = document.getElementById('combo-step-picker');
  comboPicker.replaceChildren();
  for (const m of state.moves) {
    comboPicker.appendChild(el('option', { value: m.id }, m.canonical_name));
  }
}

function renderTaxonomyView() {
  const q = document.getElementById('taxonomy-search').value.trim().toLowerCase();
  const listEl = document.getElementById('taxonomy-list');
  listEl.replaceChildren();

  const filtered = state.moves.filter((m) => {
    if (!q) return true;
    if (m.canonical_name.toLowerCase().includes(q)) return true;
    return (m.aliases || []).some((a) => a.alias.toLowerCase().includes(q));
  });

  for (const m of filtered) {
    const aliasRow = el('div', { className: 'chip-list' });
    for (const al of m.aliases || []) {
      const label = al.qualifier ? `${al.alias} (${al.qualifier})` : al.alias;
      const chip = el(
        'span',
        { className: al.collision_flagged ? 'badge badge-warning' : 'badge' },
        al.collision_flagged ? `⚠ Alias collision: ${label}` : `Alias: ${label}`
      );
      aliasRow.appendChild(chip);
    }

    let stepsLine = null;
    if (m.kind === 'combination' && m.steps?.length) {
      stepsLine = el(
        'p',
        { className: 'helper-text' },
        'Combination steps: ' + m.steps.map((s) => `${s.ordinal}. ${s.child_move_name}`).join(' → ')
      );
    }

    let addAliasForm = null;
    if (state.authenticated) {
      const aliasIn = el('input', { placeholder: 'Add regional/school alias…', 'aria-label': `Add alias for ${m.canonical_name}` });
      const qualIn = el('input', { placeholder: 'School/region (optional)', 'aria-label': `Qualifier for ${m.canonical_name}` });
      addAliasForm = el(
        'form',
        {
          className: 'inline-actions',
          onsubmit: async (ev) => {
            ev.preventDefault();
            if (!aliasIn.value.trim()) return;
            try {
              const created = await apiRequest(`/api/v1/moves/${m.id}/aliases`, {
                method: 'POST',
                body: { alias: aliasIn.value.trim(), language: 'es', qualifier: qualIn.value.trim() },
              });
              await loadMoves();
              renderTaxonomyView();
              if (created.collision_flagged) {
                showBanner(`Alias "${created.alias}" saved and flagged for collision review (no silent merge).`);
              } else {
                showBanner(`Alias "${created.alias}" added to ${m.canonical_name}.`);
              }
            } catch (err) {
              showBanner(err.message, true);
            }
          },
        },
        aliasIn,
        qualIn,
        el('button', { type: 'submit', className: 'btn btn-secondary btn-sm' }, '+ Alias')
      );
    }

    listEl.appendChild(
      el(
        'div',
        { className: 'occ-row' },
        el(
          'div',
          { style: 'width:100%' },
          el(
            'div',
            { className: 'entry-card-top' },
            el('strong', {}, m.canonical_name),
            el(
              'div',
              { className: 'inline-actions' },
              el('span', { className: 'badge badge-platform' }, m.kind),
              el('span', { className: 'badge' }, m.curation_state),
              el('code', { style: 'font-size:0.78rem' }, `#CubanSocial-${m.portable_token}`)
            )
          ),
          m.description ? el('p', { className: 'helper-text' }, m.description) : null,
          stepsLine,
          aliasRow,
          addAliasForm
        )
      )
    );
  }
}

// --- Personal Entries & Detail View ---

async function loadEntries(silent = false) {
  if (!state.authenticated) return;
  const params = new URLSearchParams();
  const q = document.getElementById('filter-q').value.trim();
  const plat = document.getElementById('filter-platform').value;
  const moveID = document.getElementById('filter-move').value;
  const vis = document.getElementById('filter-visibility').value;
  const hasAsset = document.getElementById('filter-asset').value;
  if (q) params.set('q', q);
  if (plat) params.set('platform', plat);
  if (moveID) params.set('move_id', moveID);
  if (vis) params.set('visibility', vis);
  if (hasAsset) params.set('has_asset', hasAsset);

  try {
    const data = await apiRequest(`/api/v1/entries?${params.toString()}`);
    state.entries = data.entries || [];
    renderEntryList();
  } catch (err) {
    if (!silent) showBanner(err.message, true);
  }
}

function entryDisplayTitle(entry) {
  return entry.personal_title_override || entry.source?.title || entry.source?.canonical_url || 'Untitled video';
}

function entryDisplayCreator(entry) {
  if (entry.personal_creator_override) return entry.personal_creator_override;
  const first = entry.source?.creators?.[0];
  return first?.creator?.display_name || t('unknownCreator');
}

function renderEntryList() {
  const container = document.getElementById('entry-list');
  container.replaceChildren();
  if (state.entries.length === 0) {
    container.appendChild(el('div', { className: 'card helper-text' }, 'No entries match the current filter.'));
    return;
  }

  for (const entry of state.entries) {
    const chips = el('div', { className: 'chip-list' });
    for (const occ of entry.occurrences || []) {
      if (occ.state === 'rejected') continue;
      const label = occ.move_name || occ.proposed_name || 'Move';
      const timeStr = occ.start_ms !== null ? ` (${formatMs(occ.start_ms)})` : '';
      chips.appendChild(el('span', { className: 'move-chip' }, `${label}${timeStr}`));
    }
    const pendingSuggs = (entry.suggestions || []).filter((s) => s.state === 'pending').length;
    if (pendingSuggs > 0) {
      chips.appendChild(el('span', { className: 'badge badge-suggested' }, `${pendingSuggs} suggested`));
    }

    const card = el(
      'button',
      {
        type: 'button',
        className: `entry-card ${state.selectedEntry?.id === entry.id ? 'selected' : ''}`,
        onclick: () => selectEntry(entry.id),
      },
      el(
        'div',
        { className: 'entry-card-top' },
        el('span', { className: 'badge badge-platform' }, entry.source.platform),
        el('span', { className: 'badge' }, entry.visibility_state)
      ),
      el('div', { className: 'entry-card-title' }, entryDisplayTitle(entry)),
      el('div', { className: 'helper-text' }, entryDisplayCreator(entry)),
      chips
    );
    container.appendChild(card);
  }
}

async function selectEntry(entryID, preservePlayer = false) {
  try {
    const entry = await apiRequest(`/api/v1/entries/${entryID}`);
    const playback = await apiRequest(`/api/v1/sources/${entry.source_id}/playback`);
    const prevSourceID = state.selectedEntry?.source_id;
    const prevMode = state.playbackDescriptor?.mode;
    state.selectedEntry = entry;
    state.playbackDescriptor = playback;
    renderEntryList();
    renderEntryDetail(preservePlayer && prevSourceID === entry.source_id && prevMode === playback.mode);
  } catch (err) {
    showBanner(err.message, true);
  }
}

function renderEntryDetail(keepMountedPlayer = false) {
  const emptyMsg = document.getElementById('empty-detail-msg');
  const pane = document.getElementById('entry-detail-content');
  const entry = state.selectedEntry;
  const desc = state.playbackDescriptor;
  if (!entry) {
    emptyMsg.classList.remove('hidden');
    pane.classList.add('hidden');
    return;
  }
  emptyMsg.classList.add('hidden');
  pane.classList.remove('hidden');

  if (!keepMountedPlayer) {
    if (playerController) playerController.destroy();
    pane.replaceChildren();
    const stage = el('div', { className: 'player-stage', id: 'player-stage' });
    pane.appendChild(stage);
    playerController = new PlayerController(stage, (ms) => {
      const clockEl = document.getElementById('live-player-clock');
      if (clockEl) clockEl.textContent = formatMs(ms);
    });
    playerController.mount(desc);
  } else {
    // Keep #player-stage intact and re-render controls below it
    while (pane.children.length > 1) {
      pane.removeChild(pane.lastChild);
    }
  }

  // Creator Attribution Bar (META-03)
  const sc = entry.source.creators?.[0];
  const creatorName = entryDisplayCreator(entry);
  const creatorProfile = sc?.creator?.identities?.[0]?.profile_url || '';
  const creatorRole = sc?.role ? ` (${sc.role})` : '';
  const durationLabel = entry.source.duration_ms !== null ? `Duration: ${formatMs(entry.source.duration_ms)}` : t('unknownDuration');

  pane.appendChild(
    el(
      'div',
      { className: 'attribution-bar' },
      el(
        'div',
        {},
        el('strong', {}, entryDisplayTitle(entry)),
        el(
          'div',
          { className: 'helper-text' },
          `Creator: ${creatorName}${creatorRole} · ${durationLabel} · Rev v${entry.revision}`
        ),
        desc?.precision_note ? el('div', { className: 'helper-text' }, desc.precision_note) : null
      ),
      el(
        'div',
        { className: 'inline-actions' },
        creatorProfile
          ? el('a', { href: creatorProfile, target: '_blank', rel: 'noopener noreferrer', className: 'btn btn-secondary btn-sm' }, 'Creator Profile ↗')
          : null,
        el('a', { href: entry.source.original_url, target: '_blank', rel: 'noopener noreferrer', className: 'btn btn-ghost btn-sm' }, t('openOriginal'))
      )
    )
  );

  // Timing & Add Occurrence Form (TIME-01..TIME-03)
  const startInput = el('input', { id: 'occ-start-input', placeholder: 'MM:SS.mmm or sec (optional)', 'aria-label': 'Start timestamp' });
  const endInput = el('input', { id: 'occ-end-input', placeholder: 'MM:SS.mmm or sec (optional)', 'aria-label': 'End timestamp' });
  const methodInput = el('input', { type: 'hidden', id: 'occ-timing-method', value: 'manual' });

  if (entry.observed_offset_ms !== null && (entry.occurrences || []).length === 0) {
    startInput.value = formatMs(entry.observed_offset_ms);
  }

  const canCaptureTime = playerController?.supportsTimeCapture();
  const canSeek = playerController?.supportsSeek();

  const btnMarkStart = el(
    'button',
    {
      type: 'button',
      id: 'btn-mark-start',
      className: 'btn btn-secondary btn-sm',
      disabled: !canCaptureTime,
      onclick: () => captureMarkToInput(startInput, methodInput),
    },
    t('markStart')
  );
  const btnMarkEnd = el(
    'button',
    {
      type: 'button',
      id: 'btn-mark-end',
      className: 'btn btn-secondary btn-sm',
      disabled: !canCaptureTime,
      onclick: () => captureMarkToInput(endInput, methodInput),
    },
    t('markEnd')
  );
  const btnPreview = el(
    'button',
    {
      type: 'button',
      id: 'btn-preview-seg',
      className: 'btn btn-secondary btn-sm',
      disabled: !canSeek,
      onclick: () => triggerPreviewFromInputs(startInput.value, endInput.value),
    },
    t('previewSegment')
  );

  pane.appendChild(
    el(
      'div',
      { className: 'timing-toolbar' },
      el('span', { className: 'clock-pill', id: 'live-player-clock', title: 'Current player clock' }, canCaptureTime ? '00:00' : 'Manual timing'),
      btnMarkStart,
      btnMarkEnd,
      btnPreview
    )
  );

  // Add Move Occurrence Form
  const moveSelect = el('select', { 'aria-label': 'Select canonical move' }, el('option', { value: '' }, '— Choose canonical move —'));
  for (const m of state.moves) {
    moveSelect.appendChild(el('option', { value: m.id }, m.canonical_name));
  }
  const proposedInput = el('input', { placeholder: 'Or propose new vuelta name…', 'aria-label': 'Proposed move name' });
  const occNotesInput = el('input', { placeholder: 'Practice note for this occurrence (optional)', 'aria-label': 'Occurrence note' });

  pane.appendChild(
    el(
      'form',
      {
        className: 'stack-form',
        onsubmit: async (ev) => {
          ev.preventDefault();
          try {
            await apiRequest(`/api/v1/entries/${entry.id}/occurrences`, {
              method: 'POST',
              body: {
                move_id: moveSelect.value || null,
                proposed_name: proposedInput.value.trim() || null,
                start_text: startInput.value.trim() || null,
                end_text: endInput.value.trim() || null,
                notes: occNotesInput.value.trim(),
                timing_method: methodInput.value || 'manual',
              },
            });
            await loadMoves();
            await selectEntry(entry.id, true);
            await loadEntries(true);
            showBanner('Move occurrence saved.');
          } catch (err) {
            showBanner(err.message, true);
          }
        },
      },
      el('h3', { style: 'margin:0 0 0.4rem' }, 'Identified Moves (Vueltas)'),
      el('div', { className: 'grid-2' }, moveSelect, proposedInput),
      el('div', { className: 'grid-2', style: 'margin-top:0.5rem' }, startInput, endInput),
      el('div', { className: 'inline-actions' }, occNotesInput, methodInput, el('button', { type: 'submit', className: 'btn btn-primary' }, '+ Save Occurrence'))
    )
  );

  // Occurrence List
  const occList = el('div', { style: 'margin-top:0.75rem' });
  for (const occ of entry.occurrences || []) {
    const rangeLabel =
      occ.start_ms !== null || occ.end_ms !== null
        ? `${formatMs(occ.start_ms)} → ${formatMs(occ.end_ms)}`
        : 'Time unknown';
    occList.appendChild(
      el(
        'div',
        { className: 'occ-row' },
        el(
          'div',
          {},
          el('strong', {}, occ.move_name || occ.proposed_name || 'Unnamed'),
          ' ',
          el('span', { className: 'occ-time' }, rangeLabel),
          ' ',
          el('span', { className: 'badge' }, occ.timing_method),
          occ.notes ? el('div', { className: 'helper-text' }, occ.notes) : null
        ),
        el(
          'div',
          { className: 'inline-actions' },
          canSeek && occ.start_ms !== null
            ? el(
                'button',
                {
                  type: 'button',
                  className: 'btn btn-secondary btn-sm',
                  onclick: () => playerController.previewSegment(occ.start_ms, occ.end_ms),
                },
                '▶ Play'
              )
            : null,
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-danger btn-sm',
              onclick: async () => {
                await apiRequest(`/api/v1/entries/${entry.id}/occurrences/${occ.id}`, { method: 'DELETE' });
                await selectEntry(entry.id, true);
                await loadEntries(true);
              },
            },
            'Delete'
          )
        )
      )
    );
  }
  pane.appendChild(occList);

  // Collapsed-by-default Evidence & Suggestions Panel (ANA-01, ANA-02, Section 11)
  const pendingSuggestions = (entry.suggestions || []).filter((s) => s.state === 'pending');
  const detailsEl = el('details', { className: 'sub-section' });
  detailsEl.appendChild(
    el(
      'summary',
      { style: 'cursor:pointer;font-weight:700' },
      `${t('evidenceSummary')} (${pendingSuggestions.length} pending, ${(entry.evidence || []).length} evidence items)`
    )
  );
  const evidBody = el('div', { style: 'margin-top:0.65rem' });
  evidBody.appendChild(
    el(
      'button',
      {
        type: 'button',
        className: 'btn btn-secondary btn-sm',
        onclick: async () => {
          try {
            const res = await apiRequest(`/api/v1/entries/${entry.id}/analysis`, {
              method: 'POST',
              body: { sync: true, force: true },
            });
            await selectEntry(entry.id, true);
            showBanner(`Analysis finished: ${res.suggestion_count} suggestion(s) evaluated.`);
          } catch (err) {
            showBanner(err.message, true);
          }
        },
      },
      'Run Assisted Analysis'
    )
  );

  for (const sg of pendingSuggestions) {
    evidBody.appendChild(
      el(
        'div',
        { className: 'occ-row', style: 'margin-top:0.5rem' },
        el(
          'div',
          {},
          el('span', { className: 'badge badge-suggested' }, `${t('suggestedBadge')} · ${sg.confidence_band}`),
          ' ',
          el('strong', {}, sg.candidate_name),
          sg.start_ms !== null ? el('span', { className: 'occ-time' }, ` @ ${formatMs(sg.start_ms)}${sg.end_ms !== null ? '–' + formatMs(sg.end_ms) : ''}`) : null,
          sg.is_announcement_only ? el('div', { className: 'helper-text' }, t('announcementNote')) : null,
          el('div', { className: 'helper-text' }, `Processor: ${sg.processor_version}`)
        ),
        el(
          'div',
          { className: 'inline-actions' },
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-primary btn-sm',
              onclick: async () => {
                await apiRequest(`/api/v1/entries/${entry.id}/suggestions/${sg.id}/decision`, {
                  method: 'POST',
                  body: { decision: 'accept' },
                });
                await selectEntry(entry.id, true);
                await loadEntries(true);
              },
            },
            'Confirm'
          ),
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-ghost btn-sm',
              onclick: async () => {
                await apiRequest(`/api/v1/entries/${entry.id}/suggestions/${sg.id}/decision`, {
                  method: 'POST',
                  body: { decision: 'reject' },
                });
                await selectEntry(entry.id, true);
              },
            },
            'Reject'
          )
        )
      )
    );
  }
  for (const ev of entry.evidence || []) {
    evidBody.appendChild(
      el(
        'div',
        { className: 'helper-text', style: 'padding:0.35rem 0;border-bottom:1px dashed var(--border)' },
        `[${ev.kind} · ${ev.author_role}] ${ev.excerpt}`
      )
    );
  }
  detailsEl.appendChild(evidBody);
  pane.appendChild(detailsEl);

  // Personal Metadata, Creator Attribution & Private Notes (with If-Match optimistic concurrency)
  const titleOvIn = el('input', { value: entry.personal_title_override || '', placeholder: entry.source.title || 'Custom title override' });
  const creatorNameIn = el('input', { value: sc?.creator?.display_name || '', placeholder: 'Creator name' });
  const creatorHandleIn = el('input', { value: sc?.creator?.identities?.[0]?.handle || '', placeholder: '@handle (optional)' });
  const creatorURLIn = el('input', { value: sc?.creator?.identities?.[0]?.profile_url || '', placeholder: 'https://... profile URL' });
  const creatorRoleSel = el(
    'select',
    {},
    el('option', { value: 'uploader', selected: sc?.role !== 'original' }, 'Uploader account'),
    el('option', { value: 'original', selected: sc?.role === 'original' }, 'Original choreographer / dancer')
  );
  const creatorVerifiedChk = el('input', {
    type: 'checkbox',
    checked: Boolean(sc?.creator?.identities?.[0]?.verified),
    style: 'width:auto',
  });
  const notesArea = el('textarea', { rows: 3, placeholder: 'Private practice cues (never shared publicly)' }, entry.private_notes || '');

  pane.appendChild(
    el(
      'form',
      {
        className: 'sub-section stack-form',
        onsubmit: async (ev) => {
          ev.preventDefault();
          try {
            const payload = {
              revision: entry.revision,
              private_notes: notesArea.value,
              personal_title_override: titleOvIn.value.trim(),
              clear_title_override: titleOvIn.value.trim() === '',
            };
            if (creatorNameIn.value.trim()) {
              payload.creator = {
                display_name: creatorNameIn.value.trim(),
                role: creatorRoleSel.value,
                handle: creatorHandleIn.value.trim(),
                profile_url: creatorURLIn.value.trim(),
                verified: creatorVerifiedChk.checked,
              };
            }
            await apiRequest(`/api/v1/entries/${entry.id}`, {
              method: 'PATCH',
              ifMatch: entry.revision,
              body: payload,
            });
            await selectEntry(entry.id, true);
            await loadEntries(true);
            showBanner('Metadata & private notes saved.');
          } catch (err) {
            if (err.status === 409) {
              showBanner('Revision conflict (409): another edit occurred. Reloaded latest version so you can review.', true);
              await selectEntry(entry.id, true);
            } else {
              showBanner(err.message, true);
            }
          }
        },
      },
      el('h3', { style: 'margin:0 0 0.4rem' }, 'Creator Attribution & Private Notes'),
      el('label', { className: 'field-label' }, 'Title Override'),
      titleOvIn,
      el('div', { className: 'grid-2' },
        el('div', {}, el('label', { className: 'field-label' }, 'Creator Name'), creatorNameIn),
        el('div', {}, el('label', { className: 'field-label' }, 'Attribution Role'), creatorRoleSel)
      ),
      el('div', { className: 'grid-2' },
        el('div', {}, el('label', { className: 'field-label' }, 'Creator Handle'), creatorHandleIn),
        el('div', {}, el('label', { className: 'field-label' }, 'Profile URL'), creatorURLIn)
      ),
      state.user?.role === 'admin'
        ? el('label', { className: 'inline-actions helper-text' }, creatorVerifiedChk, ' Verified creator handle (eligible for Instagram @mention)')
        : null,
      el('label', { className: 'field-label' }, 'Private Practice Notes (Private only)'),
      notesArea,
      el('div', { className: 'inline-actions' },
        el('button', { type: 'submit', className: 'btn btn-secondary' }, 'Save Metadata & Notes'),
        el(
          'button',
          {
            type: 'button',
            className: 'btn btn-danger',
            onclick: async () => {
              await apiRequest(`/api/v1/entries/${entry.id}`, { method: 'DELETE' });
              state.selectedEntry = null;
              renderEntryDetail();
              await loadEntries();
              showBanner('Entry removed from personal catalog.');
            },
          },
          'Delete Entry'
        )
      )
    )
  );

  // Authorized Media Import & Rights Management (MED-01, MED-02)
  const fileIn = el('input', { type: 'file', accept: 'video/mp4,video/quicktime,video/webm', 'aria-label': 'Authorized video file' });
  const mediaSection = el(
    'div',
    { className: 'sub-section' },
    el('h3', { style: 'margin:0 0 0.35rem' }, 'Authorized Media Assets'),
    el('p', { className: 'helper-text' }, 'Import creator-authorized MP4/MOV/WebM files only. Imported files default to private access.'),
    el(
      'form',
      {
        className: 'inline-actions',
        onsubmit: async (ev) => {
          ev.preventDefault();
          if (!fileIn.files?.[0]) return;
          const fd = new FormData();
          fd.append('source_id', entry.source_id);
          fd.append('file', fileIn.files[0]);
          try {
            await apiRequest('/api/v1/assets/imports', { method: 'POST', body: fd });
            await selectEntry(entry.id, false);
            showBanner('Authorized video validated, checksummed, and imported.');
          } catch (err) {
            showBanner(err.message, true);
          }
        },
      },
      fileIn,
      el('button', { type: 'submit', className: 'btn btn-secondary' }, 'Import Authorized File')
    )
  );

  for (const asset of entry.assets || []) {
    mediaSection.appendChild(
      el(
        'div',
        { className: 'occ-row', style: 'margin-top:0.5rem' },
        el(
          'div',
          {},
          el('span', { className: 'badge badge-ready' }, `${asset.mime_type} · ${asset.access_scope}`),
          ' ',
          el('code', { style: 'font-size:0.78rem' }, `SHA-256: ${asset.sha256_hex.slice(0, 16)}…`),
          el('div', { className: 'helper-text' }, `${Math.round(asset.byte_size / 1024)} KiB`)
        ),
        el(
          'button',
          {
            type: 'button',
            className: 'btn btn-danger btn-sm',
            onclick: async () => {
              await apiRequest(`/api/v1/assets/${asset.id}`, { method: 'DELETE' });
              await selectEntry(entry.id, false);
              showBanner('Asset revoked and deleted.');
            },
          },
          'Revoke & Delete'
        )
      )
    );
  }
  pane.appendChild(mediaSection);

  // Community Submission & Admin Instagram Draft Actions (4.3 & 4.4)
  const publishBar = el(
    'div',
    { className: 'sub-section' },
    el('h3', { style: 'margin:0 0 0.35rem' }, 'Community & Publishing Actions'),
    el('p', { className: 'helper-text' }, t('privateExcludedNotice')),
    el(
      'div',
      { className: 'inline-actions' },
      el(
        'button',
        {
          type: 'button',
          className: 'btn btn-secondary',
          onclick: async () => {
            try {
              await apiRequest(`/api/v1/entries/${entry.id}/submissions`, { method: 'POST', body: {} });
              await selectEntry(entry.id, true);
              await loadEntries(true);
              showBanner('Submitted versioned public snapshot for administrator review.');
            } catch (err) {
              showBanner(err.message, true);
            }
          },
        },
        'Submit to Community Catalog'
      ),
      state.user?.role === 'admin'
        ? el(
            'button',
            {
              type: 'button',
              className: 'btn btn-primary',
              onclick: () => openAdminPublishDraftFromEntry(entry),
            },
            'Prepare Instagram Draft (@cubansocial.sd)'
          )
        : null
    )
  );

  // Admin Rights Record Quick Form (Section 8 & 13: separate video & music clearance)
  if (state.user?.role === 'admin') {
    const grantorIn = el('input', { placeholder: 'Grantor (Creator / Rights holder)', required: true });
    const basisIn = el('input', { placeholder: 'Written permission / License ref', required: true });
    const vidChk = el('input', { type: 'checkbox', style: 'width:auto', checked: true });
    const musChk = el('input', { type: 'checkbox', style: 'width:auto', checked: true });
    publishBar.appendChild(
      el(
        'form',
        {
          className: 'stack-form',
          style: 'margin-top:0.85rem;padding:0.75rem;background:var(--surface-alt);border-radius:8px',
          onsubmit: async (ev) => {
            ev.preventDefault();
            try {
              await apiRequest('/api/v1/admin/rights', {
                method: 'POST',
                body: {
                  source_id: entry.source_id,
                  asset_id: entry.assets?.[0]?.id || null,
                  grantor: grantorIn.value.trim(),
                  basis: basisIn.value.trim(),
                  video_cleared: vidChk.checked,
                  music_cleared: musChk.checked,
                  authorized_uses: ['republish'],
                  authorized_platforms: ['instagram'],
                },
              });
              showBanner('Explicit rights record stored for source.');
            } catch (err) {
              showBanner(err.message, true);
            }
          },
        },
        el('strong', {}, 'Admin Rights Clearance (Required before external Instagram upload)'),
        el('div', { className: 'grid-2' }, grantorIn, basisIn),
        el(
          'div',
          { className: 'inline-actions' },
          el('label', { className: 'inline-actions helper-text' }, vidChk, ' Video rights cleared'),
          el('label', { className: 'inline-actions helper-text' }, musChk, ' Music rights cleared'),
          el('button', { type: 'submit', className: 'btn btn-secondary btn-sm' }, 'Record Rights')
        )
      )
    );
  }
  pane.appendChild(publishBar);
}

function captureMarkToInput(inputEl, methodEl) {
  if (!playerController) return;
  const ms = playerController.getCurrentTimeMs();
  if (ms === null || ms === undefined) {
    showBanner('Start playback first so the player reports its current timestamp.', true);
    return;
  }
  inputEl.value = formatMs(ms);
  if (methodEl) methodEl.value = 'player_mark';
}

function parseClientTimeMs(str) {
  const s = (str || '').trim();
  if (!s) return null;
  const parts = s.split(':');
  if (parts.length === 1) {
    const v = Number(parts[0]);
    return Number.isFinite(v) && v >= 0 ? Math.round(v * 1000) : null;
  }
  if (parts.length === 2) {
    return Math.round((Number(parts[0]) * 60 + Number(parts[1])) * 1000);
  }
  if (parts.length === 3) {
    return Math.round((Number(parts[0]) * 3600 + Number(parts[1]) * 60 + Number(parts[2])) * 1000);
  }
  return null;
}

function triggerPreviewFromInputs(startStr, endStr) {
  const sMs = parseClientTimeMs(startStr);
  const eMs = parseClientTimeMs(endStr);
  if (sMs === null) {
    showBanner('Enter or mark a start timestamp first to preview a segment.', true);
    return;
  }
  playerController?.previewSegment(sMs, eMs);
}

// --- Public Community Catalog ---

async function loadPublicCatalog() {
  const q = document.getElementById('public-q')?.value.trim() || '';
  const plat = document.getElementById('public-platform')?.value || '';
  const params = new URLSearchParams();
  if (q) params.set('q', q);
  if (plat) params.set('platform', plat);

  const data = await apiRequest(`/api/v1/public/entries?${params.toString()}`);
  const grid = document.getElementById('public-catalog-list');
  grid.replaceChildren();
  const entries = data.entries || [];
  if (entries.length === 0) {
    grid.appendChild(el('div', { className: 'card helper-text' }, 'No approved public entries yet.'));
    return;
  }

  for (const rev of entries) {
    const p = rev.payload;
    const chips = el('div', { className: 'chip-list' });
    for (const o of p.occurrences || []) {
      chips.appendChild(
        el('span', { className: 'move-chip' }, `${o.move_name}${o.start_ms !== null ? ` (${formatMs(o.start_ms)})` : ''}`)
      );
    }
    grid.appendChild(
      el(
        'article',
        { className: 'card' },
        el(
          'div',
          { className: 'entry-card-top' },
          el('span', { className: 'badge badge-platform' }, p.platform),
          el('span', { className: 'badge badge-published' }, `Public v${rev.revision_number}`)
        ),
        el('h3', { style: 'margin:0.35rem 0' }, p.title || 'Cuban Dance Clip'),
        el('p', { className: 'helper-text' }, `Creator: ${p.creator_name || t('unknownCreator')}`),
        chips,
        el(
          'div',
          { className: 'inline-actions', style: 'margin-top:0.8rem' },
          el('a', { href: p.canonical_url, target: '_blank', rel: 'noopener noreferrer', className: 'btn btn-secondary btn-sm' }, t('openOriginal')),
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-ghost btn-sm',
              onclick: async () => {
                const details = window.prompt('Describe issue (incorrect naming, misattribution, unavailable source, or rights concern):');
                if (!details) return;
                await apiRequest(`/api/v1/public/entries/${rev.id}/reports`, {
                  method: 'POST',
                  body: { category: 'incorrect_naming', details },
                });
                showBanner('Report submitted for administrator review.');
              },
            },
            'Report Issue'
          )
        )
      )
    );
  }
}

// --- Admin Moderation & Instagram Studio ---

async function openAdminPublishDraftFromEntry(entry) {
  try {
    const assetID = entry.assets?.[0]?.id || null;
    await apiRequest('/api/v1/admin/publications', {
      method: 'POST',
      body: {
        private_entry_id: entry.id,
        asset_id: assetID,
        include_companion_tags: false,
      },
    });
    switchView('admin');
    showBanner('Instagram publication draft created. Review text, tags, and rights below.');
  } catch (err) {
    showBanner(err.message, true);
  }
}

async function loadAdminStudio() {
  if (state.user?.role !== 'admin') return;
  const [subsData, pubsData] = await Promise.all([
    apiRequest('/api/v1/admin/submissions?status=pending'),
    apiRequest('/api/v1/admin/publications'),
  ]);

  const subsBox = document.getElementById('admin-submissions-list');
  subsBox.replaceChildren();
  const subs = subsData.submissions || [];
  if (subs.length === 0) {
    subsBox.appendChild(el('p', { className: 'helper-text' }, 'No pending community submissions.'));
  }
  for (const sub of subs) {
    const reasonIn = el('input', { placeholder: 'Review note / rejection reason', 'aria-label': 'Moderation note' });
    subsBox.appendChild(
      el(
        'div',
        { className: 'occ-row' },
        el(
          'div',
          { style: 'width:100%' },
          el('strong', {}, `${sub.snapshot.title || sub.snapshot.canonical_url} (${sub.snapshot.platform})`),
          el('div', { className: 'helper-text' }, `Creator: ${sub.snapshot.creator_name} · Moves: ${(sub.snapshot.occurrences || []).map((o) => o.move_name).join(', ') || 'None'}`),
          el(
            'div',
            { className: 'inline-actions' },
            reasonIn,
            el(
              'button',
              {
                type: 'button',
                className: 'btn btn-primary btn-sm',
                onclick: async () => {
                  await apiRequest(`/api/v1/admin/submissions/${sub.id}/decision`, {
                    method: 'POST',
                    body: { decision: 'approved', reason: reasonIn.value },
                  });
                  await loadAdminStudio();
                  showBanner('Submission approved and published to Community Catalog.');
                },
              },
              'Approve'
            ),
            el(
              'button',
              {
                type: 'button',
                className: 'btn btn-danger btn-sm',
                onclick: async () => {
                  await apiRequest(`/api/v1/admin/submissions/${sub.id}/decision`, {
                    method: 'POST',
                    body: { decision: 'rejected', reason: reasonIn.value || 'Rejected by moderator' },
                  });
                  await loadAdminStudio();
                  showBanner('Submission rejected.');
                },
              },
              'Reject'
            )
          )
        )
      )
    );
  }

  const pubsBox = document.getElementById('admin-publications-list');
  pubsBox.replaceChildren();
  const pubs = pubsData.publications || [];
  if (pubs.length === 0) {
    pubsBox.appendChild(el('p', { className: 'helper-text' }, 'No Instagram publication drafts yet. Open any entry and click "Prepare Instagram Draft".'));
    return;
  }

  for (const pub of pubs) {
    const captionArea = el('textarea', { rows: 5, 'aria-label': 'Instagram caption' }, pub.caption || '');
    const commentIn = el('input', { value: pub.first_comment || '', placeholder: 'Optional first comment (separate status/retry)' });
    const companionChk = el('input', { type: 'checkbox', checked: pub.include_companion_tags, style: 'width:auto' });

    const tagSummary = (pub.move_tags || [])
      .map((mt) => (mt.platform_hashtag ? `${mt.display_label} + ${mt.platform_hashtag}` : `${mt.display_label} (label only)`))
      .join(' · ');

    pubsBox.appendChild(
      el(
        'div',
        { className: 'card', style: 'background:#fcfbf9' },
        el(
          'div',
          { className: 'entry-card-top' },
          el('strong', {}, `${pub.destination_account} · ${pub.publication_mode} (${pub.origin_platform})`),
          el('span', { className: 'badge badge-platform' }, pub.status)
        ),
        el('p', { className: 'helper-text' }, pub.status_detail || ''),
        tagSummary ? el('p', { className: 'helper-text' }, `Labels: ${tagSummary}`) : null,
        el('code', { className: 'code-box' }, `Draft SHA-256: ${pub.draft_hash}\nApproved Hash: ${pub.approved_draft_hash || '(unapproved)'}`),
        pub.native_repost_guide ? el('div', { className: 'code-box' }, pub.native_repost_guide) : null,
        pub.result_url
          ? el('p', {}, el('a', { href: pub.result_url, target: '_blank', rel: 'noopener noreferrer' }, `Published URL: ${pub.result_url} ↗`))
          : null,
        el('label', { className: 'field-label' }, 'Editable Caption'),
        captionArea,
        el('label', { className: 'field-label' }, 'Optional First Comment'),
        commentIn,
        el(
          'label',
          { className: 'inline-actions helper-text' },
          companionChk,
          ' Explicitly include compatible underscore companion hashtags (e.g. #CubanSocial_Move)'
        ),
        el(
          'div',
          { className: 'inline-actions' },
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-secondary btn-sm',
              onclick: async () => {
                try {
                  await apiRequest(`/api/v1/admin/publications/${pub.id}`, {
                    method: 'PATCH',
                    body: {
                      caption: captionArea.value,
                      first_comment: commentIn.value,
                      include_companion_tags: companionChk.checked,
                    },
                  });
                  await loadAdminStudio();
                  showBanner('Draft updated (prior approval invalidated per PUB-03).');
                } catch (err) {
                  showBanner(err.message, true);
                }
              },
            },
            'Save Draft Edits'
          ),
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-secondary btn-sm',
              onclick: async () => {
                try {
                  await apiRequest(`/api/v1/admin/publications/${pub.id}/approve`, {
                    method: 'POST',
                    body: { draft_hash: pub.draft_hash, destination_account: pub.destination_account },
                  });
                  await loadAdminStudio();
                  showBanner('Exact draft hash approved.');
                } catch (err) {
                  showBanner(err.message, true);
                }
              },
            },
            'Approve Exact Hash'
          ),
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-ghost btn-sm',
              onclick: async () => {
                try {
                  const res = await apiRequest(`/api/v1/admin/publications/${pub.id}/execute`, {
                    method: 'POST',
                    body: { dry_run: true },
                  });
                  showBanner(res.status_detail || 'Dry-run passed.');
                } catch (err) {
                  showBanner(err.message, true);
                }
              },
            },
            'Dry-Run'
          ),
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-primary btn-sm',
              onclick: async () => {
                try {
                  await apiRequest(`/api/v1/admin/publications/${pub.id}/execute`, {
                    method: 'POST',
                    body: { dry_run: false, idempotency_key: `web-${pub.id}-${pub.draft_hash.slice(0, 8)}` },
                  });
                  await loadAdminStudio();
                  showBanner('Publication executed.');
                } catch (err) {
                  await loadAdminStudio();
                  showBanner(err.message, true);
                }
              },
            },
            pub.publication_mode === 'native_repost_handoff' ? 'Generate Repost Handoff' : 'Publish to Instagram'
          ),
          el(
            'button',
            {
              type: 'button',
              className: 'btn btn-ghost btn-sm',
              onclick: async () => {
                let manualURL = '';
                if (pub.publication_mode === 'native_repost_handoff') {
                  manualURL = window.prompt('Paste confirmed native repost URL on Instagram (or leave blank):') || '';
                }
                try {
                  await apiRequest(`/api/v1/admin/publications/${pub.id}/reconcile`, {
                    method: 'POST',
                    body: { manual_result_url: manualURL, retry_first_comment: true },
                  });
                  await loadAdminStudio();
                  showBanner('Publication reconciled.');
                } catch (err) {
                  showBanner(err.message, true);
                }
              },
            },
            'Reconcile'
          )
        )
      )
    );
  }
}

// --- Static DOM Event Wiring & Keyboard Shortcuts ---

function bindStaticEvents() {
  for (const tab of document.querySelectorAll('.nav-tab')) {
    tab.addEventListener('click', () => switchView(tab.dataset.view));
  }

  document.getElementById('btn-guest-community').addEventListener('click', () => switchView('community'));

  // Auth modal
  const dialog = document.getElementById('auth-dialog');
  let authMode = 'login';
  const openAuth = (mode = 'login') => {
    authMode = mode;
    document.getElementById('register-only-fields').classList.toggle('hidden', mode !== 'register');
    document.getElementById('auth-mode-login').classList.toggle('active', mode === 'login');
    document.getElementById('auth-mode-register').classList.toggle('active', mode === 'register');
    document.getElementById('auth-error').classList.add('hidden');
    dialog.showModal();
  };

  document.getElementById('btn-open-login').addEventListener('click', () => openAuth('login'));
  document.getElementById('btn-guest-signin').addEventListener('click', () => openAuth('login'));
  document.getElementById('btn-close-auth').addEventListener('click', () => dialog.close());
  document.getElementById('auth-mode-login').addEventListener('click', () => openAuth('login'));
  document.getElementById('auth-mode-register').addEventListener('click', () => openAuth('register'));

  document.getElementById('form-auth').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const errEl = document.getElementById('auth-error');
    errEl.classList.add('hidden');
    const email = document.getElementById('auth-email').value.trim();
    const password = document.getElementById('auth-password').value;
    try {
      if (authMode === 'register') {
        const res = await apiRequest('/api/v1/auth/register', {
          method: 'POST',
          body: {
            invite_code: document.getElementById('auth-invite').value.trim(),
            display_name: document.getElementById('auth-display-name').value.trim(),
            email,
            password,
          },
        });
        state.authenticated = true;
        state.user = res.user;
        setCSRFToken(res.csrf_token);
      } else {
        const res = await apiRequest('/api/v1/auth/login', {
          method: 'POST',
          body: { email, password, client: 'web' },
        });
        state.authenticated = true;
        state.user = res.user;
        setCSRFToken(res.csrf_token);
      }
      dialog.close();
      connectSSE();
      renderShell();
      await loadMoves();
      await loadEntries();
      switchView('personal');
    } catch (err) {
      errEl.textContent = err.message;
      errEl.classList.remove('hidden');
    }
  });

  document.getElementById('btn-logout').addEventListener('click', async () => {
    try {
      await apiRequest('/api/v1/auth/logout', { method: 'POST', body: {} });
    } catch {
      // Clear local session regardless
    }
    if (state.sse) state.sse.close();
    state.authenticated = false;
    state.user = null;
    state.selectedEntry = null;
    setCSRFToken('');
    renderShell();
    switchView('community');
  });

  document.getElementById('btn-export').addEventListener('click', () => {
    window.location.href = '/api/v1/export';
  });

  // Immediate Link Save (ING-01..ING-04: unsupported hosts preserve user input!)
  document.getElementById('form-save-link').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const input = document.getElementById('input-save-url');
    const feedback = document.getElementById('ingest-feedback');
    const rawURL = input.value.trim();
    if (!rawURL) return;

    try {
      const res = await apiRequest('/api/v1/entries', {
        method: 'POST',
        body: { url: rawURL },
      });
      input.value = '';
      feedback.textContent = res.deduplicated
        ? `Already in your catalog (${res.source.platform}) — opened existing entry.`
        : `Saved ${res.source.platform} entry immediately. Background metadata enrichment ${res.job ? 'queued' : 'ready'}.`;
      await loadEntries(true);
      await selectEntry(res.entry.id);
    } catch (err) {
      // Do NOT clear input.value so user never loses unsupported/mistyped URLs (ING-01)!
      feedback.textContent = err.message;
      showBanner(err.message, true);
    }
  });

  document.getElementById('form-search').addEventListener('submit', (ev) => {
    ev.preventDefault();
    loadEntries();
  });

  document.getElementById('form-public-search').addEventListener('submit', (ev) => {
    ev.preventDefault();
    loadPublicCatalog();
  });

  document.getElementById('taxonomy-search').addEventListener('input', () => renderTaxonomyView());

  document.getElementById('move-kind').addEventListener('change', (ev) => {
    document.getElementById('combo-steps-box').classList.toggle('hidden', ev.target.value !== 'combination');
  });

  document.getElementById('btn-append-step').addEventListener('click', () => {
    const picker = document.getElementById('combo-step-picker');
    if (!picker.value) return;
    const mv = state.moves.find((m) => m.id === picker.value);
    if (!mv) return;
    state.comboDraftSteps.push({ child_move_id: mv.id, name: mv.canonical_name });
    const list = document.getElementById('combo-step-list');
    list.replaceChildren(...state.comboDraftSteps.map((s) => el('li', {}, s.name)));
  });

  document.getElementById('form-create-move').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!state.authenticated) {
      showBanner('Sign in first to propose or curate moves.', true);
      return;
    }
    const kind = document.getElementById('move-kind').value;
    const styles = document
      .getElementById('move-styles')
      .value.split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    try {
      await apiRequest('/api/v1/moves', {
        method: 'POST',
        body: {
          canonical_name: document.getElementById('move-name').value.trim(),
          kind,
          style_tags: styles,
          description: document.getElementById('move-desc').value.trim(),
          steps: kind === 'combination' ? state.comboDraftSteps.map((s) => ({ child_move_id: s.child_move_id })) : [],
        },
      });
      document.getElementById('move-name').value = '';
      document.getElementById('move-desc').value = '';
      state.comboDraftSteps = [];
      document.getElementById('combo-step-list').replaceChildren();
      await loadMoves();
      renderTaxonomyView();
      showBanner('Move saved to taxonomy.');
    } catch (err) {
      showBanner(err.message, true);
    }
  });

  document.getElementById('form-invite').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const out = document.getElementById('invite-output');
    try {
      const inv = await apiRequest('/api/v1/admin/invites', {
        method: 'POST',
        body: {
          email: document.getElementById('invite-email').value.trim(),
          role: document.getElementById('invite-role').value,
        },
      });
      out.textContent = `Invite code for ${inv.email} (${inv.role}): ${inv.code}`;
      out.classList.remove('hidden');
    } catch (err) {
      showBanner(err.message, true);
    }
  });

  document.getElementById('btn-refresh-pubs').addEventListener('click', () => loadAdminStudio());

  // Discoverable keyboard shortcuts that NEVER fire during text entry (Section 11)
  document.addEventListener('keydown', (ev) => {
    if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
    const tag = document.activeElement?.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || document.activeElement?.isContentEditable) {
      return;
    }
    if (state.activeView !== 'personal' || !state.selectedEntry) return;

    if (ev.key === '[') {
      ev.preventDefault();
      document.getElementById('btn-mark-start')?.click();
    } else if (ev.key === ']') {
      ev.preventDefault();
      document.getElementById('btn-mark-end')?.click();
    } else if (ev.key === 'p' || ev.key === 'P') {
      ev.preventDefault();
      document.getElementById('btn-preview-seg')?.click();
    }
  });
}

boot();

