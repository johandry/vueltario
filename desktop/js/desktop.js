// Wails v2 Frontend Controller — binds strictly to window.go.desktop.App (the shared Go publishing core).

const bridge = () => window.go?.desktop?.App;

const uiState = {
  auth: null,
  activePanel: 'queue',
  publications: [],
  selectedPub: null,
  entries: [],
};

function notify(msg, isErr = false) {
  const b = document.getElementById('banner');
  b.textContent = msg;
  b.classList.toggle('error', isErr);
  b.classList.remove('hidden');
  clearTimeout(notify._t);
  notify._t = setTimeout(() => b.classList.add('hidden'), 6000);
}

async function init() {
  wireEvents();
  if (!bridge()) {
    notify('Wails bridge awaits native host window (window.go.desktop.App).', false);
    return;
  }
  uiState.auth = await bridge().GetAuthState();
  renderAuth();
  if (uiState.auth.authenticated) {
    await refreshQueue();
    await loadCatalogEntries();
  }
}

function renderAuth() {
  const gate = document.getElementById('login-gate');
  const lbl = document.getElementById('session-label');
  const btnOut = document.getElementById('btn-logout');

  if (uiState.auth?.authenticated && uiState.auth.user) {
    gate.classList.add('hidden');
    lbl.textContent = `${uiState.auth.user.email} (${uiState.auth.user.role})`;
    btnOut.classList.remove('hidden');
    switchPanel(uiState.activePanel);
  } else {
    gate.classList.remove('hidden');
    lbl.textContent = 'Signed out';
    btnOut.classList.add('hidden');
    for (const p of document.querySelectorAll('.panel')) p.classList.add('hidden');
  }
}

function switchPanel(name) {
  uiState.activePanel = name;
  for (const t of document.querySelectorAll('.tab')) {
    t.classList.toggle('active', t.dataset.panel === name);
  }
  if (!uiState.auth?.authenticated) return;
  for (const p of document.querySelectorAll('.panel')) {
    p.classList.toggle('hidden', p.id !== `panel-${name}`);
  }
  if (name === 'history') renderHistory();
}

async function refreshQueue() {
  uiState.publications = (await bridge().ListPublications()) || [];
  const qEl = document.getElementById('queue-list');
  qEl.replaceChildren();
  const active = uiState.publications.filter((p) => p.status !== 'published' && p.status !== 'cancelled');
  for (const pub of active) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = `queue-item ${uiState.selectedPub?.id === pub.id ? 'selected' : ''}`;
    const strong = document.createElement('strong');
    strong.textContent = `${pub.destination_account} · ${pub.origin_platform}`;
    const sub = document.createElement('div');
    sub.className = 'muted';
    sub.textContent = `${pub.status} · ${pub.publication_mode}`;
    btn.append(strong, sub);
    btn.addEventListener('click', () => selectPublication(pub.id));
    qEl.appendChild(btn);
  }
  if (uiState.selectedPub) {
    const updated = uiState.publications.find((p) => p.id === uiState.selectedPub.id);
    if (updated) populateEditor(updated);
  }
}

async function selectPublication(pubID) {
  const pub = await bridge().GetPublication(pubID);
  uiState.selectedPub = pub;
  await refreshQueue();
  populateEditor(pub);
}

async function populateEditor(pub) {
  document.getElementById('editor-empty').classList.add('hidden');
  document.getElementById('editor-body').classList.remove('hidden');
  document.getElementById('ed-title').textContent = `${pub.destination_account} (${pub.publication_mode})`;
  document.getElementById('ed-status').textContent = pub.status;
  document.getElementById('ed-hash').textContent = `SHA-256: ${pub.draft_hash.slice(0, 18)}…`;
  document.getElementById('ed-asset-info').textContent =
    pub.publication_mode === 'native_repost_handoff'
      ? 'Instagram-origin source: uses manual native repost handoff (never uploads a duplicate copy).'
      : `Asset ID: ${pub.asset_id || 'Missing'} · Asset SHA-256: ${pub.asset_hash ? pub.asset_hash.slice(0, 16) + '…' : 'N/A'}`;

  const guide = document.getElementById('ed-repost-guide');
  if (pub.native_repost_guide) {
    guide.textContent = pub.native_repost_guide;
    guide.classList.remove('hidden');
  } else {
    guide.classList.add('hidden');
  }

  document.getElementById('chk-video-rights').checked = Boolean(pub.rights_snapshot?.video_cleared);
  document.getElementById('chk-music-rights').checked = Boolean(pub.rights_snapshot?.music_cleared);
  document.getElementById('chk-companion').checked = Boolean(pub.include_companion_tags);
  document.getElementById('ed-caption').value = pub.caption || '';
  document.getElementById('ed-comment').value = pub.first_comment || '';
  document.getElementById('ed-dest-confirm').value = pub.destination_account || '@cubansocial.sd';
}

async function loadCatalogEntries() {
  uiState.entries = (await bridge().ListCatalogEntries('', '')) || [];
  const box = document.getElementById('catalog-batch-list');
  const aiSel = document.getElementById('ai-entry-select');
  box.replaceChildren();
  aiSel.replaceChildren();
  for (const ent of uiState.entries) {
    const labelText = `[${ent.source.platform}] ${ent.personal_title_override || ent.source.title || ent.source.canonical_url}`;
    const row = document.createElement('label');
    row.className = 'queue-item row';
    const chk = document.createElement('input');
    chk.type = 'checkbox';
    chk.dataset.entryId = ent.id;
    chk.dataset.assetId = ent.assets?.[0]?.id || '';
    const txt = document.createElement('span');
    txt.textContent = labelText;
    row.append(chk, txt);
    box.appendChild(row);

    const opt = document.createElement('option');
    opt.value = ent.id;
    opt.textContent = labelText;
    aiSel.appendChild(opt);
  }
}

function renderHistory() {
  const box = document.getElementById('history-list');
  box.replaceChildren();
  for (const pub of uiState.publications) {
    const card = document.createElement('div');
    card.className = 'subcard';
    const top = document.createElement('div');
    top.className = 'row-between';
    const title = document.createElement('strong');
    title.textContent = `${pub.destination_account} · ${pub.origin_platform} (${pub.publication_mode})`;
    const st = document.createElement('span');
    st.className = 'pill';
    st.textContent = pub.status;
    top.append(title, st);
    const detail = document.createElement('p');
    detail.className = 'muted';
    detail.textContent = `${pub.status_detail || ''} ${pub.result_url ? '→ ' + pub.result_url : ''}`;
    card.append(top, detail);
    box.appendChild(card);
  }
}

function wireEvents() {
  for (const tab of document.querySelectorAll('.tab')) {
    tab.addEventListener('click', () => switchPanel(tab.dataset.panel));
  }

  document.getElementById('form-login').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    try {
      uiState.auth = await bridge().LoginPKCE(
        document.getElementById('login-email').value.trim(),
        document.getElementById('login-pass').value
      );
      document.getElementById('login-pass').value = '';
      renderAuth();
      await refreshQueue();
      await loadCatalogEntries();
      notify('Signed in via S256 PKCE; token saved in OS Keychain.');
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-logout').addEventListener('click', async () => {
    await bridge().Logout();
    uiState.auth = await bridge().GetAuthState();
    uiState.selectedPub = null;
    renderAuth();
  });

  document.getElementById('btn-reload-queue').addEventListener('click', () => refreshQueue());
  document.getElementById('btn-load-entries').addEventListener('click', () => loadCatalogEntries());

  document.getElementById('form-rights').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!uiState.selectedPub) return;
    try {
      await bridge().RecordRights(
        uiState.selectedPub.source_id,
        uiState.selectedPub.asset_id || '',
        document.getElementById('rights-grantor').value.trim(),
        document.getElementById('rights-basis').value.trim(),
        document.getElementById('rights-vid').checked,
        document.getElementById('rights-mus').checked
      );
      await bridge().UpdatePublicationDraft(
        uiState.selectedPub.id,
        document.getElementById('ed-caption').value,
        document.getElementById('ed-comment').value,
        document.getElementById('chk-companion').checked,
        uiState.selectedPub.asset_id || ''
      );
      await selectPublication(uiState.selectedPub.id);
      notify('Rights recorded and draft status refreshed.');
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-save-draft').addEventListener('click', async () => {
    if (!uiState.selectedPub) return;
    try {
      const updated = await bridge().UpdatePublicationDraft(
        uiState.selectedPub.id,
        document.getElementById('ed-caption').value,
        document.getElementById('ed-comment').value,
        document.getElementById('chk-companion').checked,
        uiState.selectedPub.asset_id || ''
      );
      uiState.selectedPub = updated;
      await refreshQueue();
      notify('Draft updated (prior approval invalidated per PUB-03).');
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-approve-draft').addEventListener('click', async () => {
    if (!uiState.selectedPub) return;
    try {
      const dest = document.getElementById('ed-dest-confirm').value.trim();
      const approved = await bridge().ApprovePublication(uiState.selectedPub.id, uiState.selectedPub.draft_hash, dest);
      uiState.selectedPub = approved;
      await refreshQueue();
      notify(`Approved exact SHA-256 hash for ${approved.destination_account}.`);
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-dry-run').addEventListener('click', async () => {
    if (!uiState.selectedPub) return;
    try {
      const res = await bridge().ExecutePublication(uiState.selectedPub.id, '', true);
      notify(res.status_detail || 'Dry-run validated.');
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-execute').addEventListener('click', async () => {
    if (!uiState.selectedPub) return;
    try {
      const idem = `wails-${uiState.selectedPub.id}-${uiState.selectedPub.draft_hash.slice(0, 8)}`;
      const res = await bridge().ExecutePublication(uiState.selectedPub.id, idem, false);
      uiState.selectedPub = res;
      await refreshQueue();
      notify(`Publication state: ${res.status}`);
    } catch (err) {
      await refreshQueue();
      notify(String(err), true);
    }
  });

  document.getElementById('btn-reconcile').addEventListener('click', async () => {
    if (!uiState.selectedPub) return;
    try {
      let manualURL = '';
      if (uiState.selectedPub.publication_mode === 'native_repost_handoff') {
        manualURL = window.prompt('Paste confirmed Instagram native repost URL (or leave blank):') || '';
      }
      const res = await bridge().ReconcilePublication(uiState.selectedPub.id, manualURL, true);
      uiState.selectedPub = res;
      await refreshQueue();
      notify(`Reconciled: ${res.status}`);
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-batch-ingest').addEventListener('click', async () => {
    const lines = document.getElementById('batch-urls').value.split('\n');
    try {
      const res = await bridge().BatchIngestURLs(lines, '');
      const out = document.getElementById('batch-ingest-out');
      out.textContent = JSON.stringify(res, null, 2);
      out.classList.remove('hidden');
      await loadCatalogEntries();
      notify(`Batch complete: ${res.created} created, ${res.duplicates} duplicates, ${res.failed} failed.`);
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-batch-drafts').addEventListener('click', async () => {
    const chks = document.querySelectorAll('#catalog-batch-list input[type="checkbox"]:checked');
    const inputs = Array.from(chks).map((c) => ({
      private_entry_id: c.dataset.entryId,
      asset_id: c.dataset.assetId || '',
      destination_account: uiState.auth?.ig_destination || '@cubansocial.sd',
      brand_prefix: uiState.auth?.ig_brand_prefix || 'CubanSocial',
      include_companion_tags: false,
    }));
    if (inputs.length === 0) {
      notify('Select at least one catalog entry first.', true);
      return;
    }
    try {
      const res = await bridge().BatchCreateDrafts(inputs);
      await refreshQueue();
      switchPanel('queue');
      notify(`Batch drafts created: ${res.created} succeeded, ${res.failed} isolated failures.`);
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-gen-ai-prompt').addEventListener('click', async () => {
    const entryId = document.getElementById('ai-entry-select').value;
    if (!entryId) return notify('Select a catalog entry first.', true);
    try {
      const prompt = await bridge().GenerateAIExtractionPrompt(
        entryId,
        document.getElementById('ai-extra-ctx').value,
        document.getElementById('ai-video-path').value
      );
      document.getElementById('ai-prompt-out').value = prompt;
      await navigator.clipboard?.writeText(prompt).catch(() => {});
      notify('AI prompt generated and copied to clipboard.');
    } catch (err) {
      notify(String(err), true);
    }
  });

  document.getElementById('btn-apply-ai-json').addEventListener('click', async () => {
    const entryId = document.getElementById('ai-entry-select').value;
    if (!entryId) return notify('Select a catalog entry first.', true);
    try {
      await bridge().ApplyAIExtractionJSON(
        entryId,
        document.getElementById('ai-json-in').value,
        document.getElementById('ai-replace-occs').checked
      );
      document.getElementById('ai-json-in').value = '';
      await loadCatalogEntries();
      notify('AI metadata & vueltas validated and saved.');
    } catch (err) {
      notify(String(err), true);
    }
  });
}

init();
