// Forgecell local Lab UI (read-only). Every value from the API is untrusted and
// reaches the page only as a text node or a validated attribute: this file never
// parses data as markup.
'use strict';
(() => {
  const $ = (selector) => document.querySelector(selector);

  // el builds an element. Strings among children become text nodes; props are
  // fixed by this file, never taken from data.
  function el(tag, props, ...children) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (value === undefined || value === null || value === false) continue;
      if (key === 'class') node.className = value;
      else if (key === 'text') node.textContent = String(value);
      else if (key === 'dataset') Object.assign(node.dataset, value);
      else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
      else node.setAttribute(key, value === true ? '' : String(value));
    }
    for (const child of children.flat()) {
      if (child === undefined || child === null || child === false) continue;
      node.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return node;
  }

  const ISSUE_URL = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/issues\/[1-9][0-9]*$/;
  const PULL_URL = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/[1-9][0-9]*$/;
  const REPOSITORY = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

  function externalLink(url, pattern, label) {
    if (typeof url !== 'string' || !pattern.test(url)) return null;
    return el('a', { href: url, target: '_blank', rel: 'noopener noreferrer' }, label);
  }

  // One lane per Molecule, assigned by the server; Paused has no engine semantics.
  const LANES = {
    running: { label: 'Running', short: 'running', symbol: '', cls: 'working' },
    decision: { label: 'Needs my decision', short: 'need you', symbol: '◇', cls: 'decision' },
    blocked: { label: 'Blocked', short: 'blocked', symbol: '!', cls: 'failed' },
    finished: { label: 'Draft published', short: 'draft published', symbol: '✓', cls: 'finished' },
    other: { label: 'Other', short: 'other', symbol: '·', cls: 'other' },
  };
  const FILTERS = [
    ['all', 'All'], ['running', 'Running'], ['decision', 'Needs my decision'], ['paused', 'Paused'],
    ['blocked', 'Blocked'], ['finished', 'Finished · Draft published'], ['other', 'Other'],
  ];

  const state = { lab: null, list: null, etag: null, filter: 'all', query: '', selectedId: null, detail: null, detailText: null, atomIndex: null, detailToken: 0, labToken: 0, pollToken: 0, polling: false, noteKey: null, sessionLost: false };

  function notRecorded(value) { return value === undefined || value === null || value === '' ? 'Not recorded' : String(value); }
  function short(digest) { return typeof digest === 'string' && digest.length > 12 ? digest.slice(0, 12) + '…' : notRecorded(digest); }
  function durationLabel(ms) {
    const total = Math.round(ms / 1000), h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60), s = total % 60;
    return [h ? h + 'h' : '', m ? m + 'm' : '', s || !total ? s + 's' : ''].filter(Boolean).join(' ');
  }
  function elapsed(start, end) {
    const a = Date.parse(start), b = Date.parse(end);
    return Number.isFinite(a) && Number.isFinite(b) && b >= a ? b - a : null;
  }

  // ---- API ----------------------------------------------------------------
  // Every API request carries this tab's session value, kept in sessionStorage
  // by session.js at launch; the cookie alone is not enough. Without it, or once
  // the server refuses it, the page stops asking instead of looping.
  class SessionError extends Error {}
  function tabSession() {
    try { return sessionStorage.getItem('forgecell-session') || ''; } catch (error) { return ''; }
  }
  async function api(path, headers) {
    const session = tabSession();
    if (!session || state.sessionLost) throw new SessionError('no session');
    const res = await fetch(path, { credentials: 'same-origin', cache: 'no-store', headers: Object.assign({ 'X-Forgecell-Session': session }, headers || {}) });
    if (res.status === 403) throw new SessionError('forbidden');
    return res;
  }
  function showError(message) {
    const banner = $('#load-error');
    banner.textContent = message;
    banner.hidden = !message;
  }
  function failed(error) {
    if (error instanceof SessionError) state.sessionLost = true;
    showError(error instanceof SessionError
      ? 'This tab has no Lab UI session. Each launch link works once: run forgecell ui again (or restart it) and open the new link it prints.'
      : 'The Lab could not be read. The page keeps the last data it loaded and will retry.');
  }

  async function loadLab() {
    const token = ++state.labToken;
    const res = await api('/api/lab');
    if (!res.ok) throw new Error('lab ' + res.status);
    const lab = await res.json();
    if (token !== state.labToken) return;
    state.lab = lab;
    renderLab();
  }

  // One poll at a time; a response older than the newest request never
  // overwrites newer state.
  async function poll() {
    if (document.hidden || state.polling || state.sessionLost) return;
    state.polling = true;
    const token = ++state.pollToken;
    try {
      const res = await api('/api/molecules', state.etag ? { 'If-None-Match': state.etag } : undefined);
      if (res.status === 304 || token !== state.pollToken) return;
      if (!res.ok) throw new Error('molecules ' + res.status);
      const list = await res.json();
      if (token !== state.pollToken) return;
      const previous = state.etag;
      state.list = list;
      state.etag = res.headers.get('ETag') || list.etag;
      showError('');
      keepFocus(renderList);
      if (previous && previous !== state.etag) loadLab().catch(failed);
      if (state.selectedId) loadDetail(state.selectedId, false);
    } catch (error) { failed(error); } finally { state.polling = false; }
  }

  async function loadDetail(id, reset) {
    const token = ++state.detailToken;
    try {
      const res = await api('/api/molecules/' + encodeURIComponent(id));
      if (token !== state.detailToken) return;
      if (!res.ok) {
        state.detail = null;
        state.detailText = null;
        renderMissingDetail(id, res.status);
        return;
      }
      const text = await res.text();
      if (token !== state.detailToken) return;
      // An unchanged detail is not re-rendered, so focus and scroll stay put.
      if (!reset && state.detail && text === state.detailText) return;
      const detail = JSON.parse(text);
      if (reset || !state.detail || state.detail.id !== detail.id) state.atomIndex = null;
      state.detail = detail;
      state.detailText = text;
      keepFocus(renderDetail);
    } catch (error) { failed(error); }
  }

  // ---- Focus across re-renders ----------------------------------------------
  // Re-rendered controls are found again by these data attributes, or else by
  // their id, so keyboard focus survives a poll that rebuilds them.
  const FOCUS_KEYS = ['id', 'filter', 'atom', 'origin', 'review', 'evidence'];
  function keepFocus(render) {
    const active = document.activeElement;
    const key = active && active.dataset ? FOCUS_KEYS.find((k) => active.dataset[k] !== undefined) : undefined;
    const value = key && active.dataset[key];
    const id = active && active.id;
    render();
    if ((!key && !id) || document.contains(active)) return;
    const next = key
      ? [...document.querySelectorAll('[data-' + key + ']')].find((node) => node.dataset[key] === value && !node.hidden && !node.disabled)
      : document.getElementById(id);
    if (!next) return;
    if (key === 'id') { cells().forEach((b) => { b.tabIndex = -1; }); next.tabIndex = 0; }
    next.focus({ preventScroll: true });
  }

  // ---- Lab header and Formula ---------------------------------------------
  function renderLab() {
    const lab = state.lab;
    const sample = Boolean(lab.sample);
    $('#sample-badge').hidden = !sample;
    document.body.classList.toggle('sample', sample);
    document.title = sample ? 'Forgecell · Sample data' : 'Forgecell · ' + notRecorded(lab.name);
    const repo = $('#lab-repo');
    repo.replaceChildren();
    if (!sample && REPOSITORY.test(lab.repository || '')) {
      repo.append(el('a', { href: 'https://github.com/' + lab.repository, target: '_blank', rel: 'noopener noreferrer' }, lab.repository + ' ↗'));
    } else {
      repo.append(lab.repository || 'Repository not recorded');
    }
    $('#footer-version').textContent = 'Forgecell ' + notRecorded(lab.version) + ' · Lab ' + notRecorded(lab.name) + (lab.binding ? ' · binding ' + lab.binding : '') + (lab.model ? ' · model ' + lab.model : '');
    const f = lab.formula || {};
    const states = {
      approved: 'Approved for execution',
      none: 'No active Formula',
      'activation-locked': 'Activation in progress or interrupted; execution refuses it until resumed',
      unapproved: 'Not approved for execution: missing, stale or unfinished decision',
      unreadable: 'Formula unreadable',
      sample: 'Sample data · not a real Formula',
    };
    const box = $('#active-formula');
    box.replaceChildren(
      el('div', null,
        el('span', { class: 'label', text: 'NEW MOLECULES · ACTIVE FORMULA' }),
        el('strong', null, f.formulaId || 'None', f.sha256 ? el('span', { class: 'version-tag', title: f.sha256 }, 'sha256 ' + short(f.sha256)) : null),
        el('small', null, (states[f.state] || notRecorded(f.state)) + (f.decisionId ? ' · decision ' + f.decisionId : '') + '. Applies to future Molecules; existing Molecules keep their saved snapshot.')),
      el('div', { class: 'formula-actions' }, el('span', { class: 'unavailable-tag', title: 'Version history and rollback are not connected in this slice' }, 'Versions & history · Unavailable')));
  }

  // ---- Grid, filters and search -------------------------------------------
  function molecules() { return (state.list && state.list.molecules) || []; }
  function laneOf(m) { return LANES[m.lane] || LANES.other; }
  function statusLabel(m) {
    const lane = laneOf(m);
    return lane.label + ' · ' + notRecorded(m.status) + (m.phase ? ' · ' + m.phase : '');
  }

  function matches(m) {
    const inLane = state.filter === 'all' || m.lane === state.filter;
    const terms = state.query.toLowerCase().trim().split(/\s+/).filter(Boolean);
    const haystack = ('#' + m.number + ' ' + (m.title || '') + ' ' + (m.formulaId || '') + ' ' + m.id).toLowerCase();
    return inLane && terms.every((term) => haystack.includes(term));
  }

  function renderList() {
    const list = state.list;
    const all = molecules();
    const counts = list.counts || {};
    const sample = Boolean(list.sample);
    $('#shelf-count').textContent = (sample ? 'Sample data · ' : '') + all.length + ' Molecules · ' +
      Object.entries(LANES).map(([id, lane]) => (counts[id] || 0) + ' ' + lane.short).join(' · ');
    renderNote(list);
    const cubes = $('#cubes');
    cubes.replaceChildren(...all.map((m, i) => {
      const lane = laneOf(m);
      const button = el('button', {
        type: 'button', class: 'cube-button ' + lane.cls, dataset: { id: m.id },
        'aria-pressed': String(m.id === state.selectedId),
        'aria-label': 'Molecule ' + m.id + ': #' + m.number + ' ' + (m.title || 'untitled') + ', ' + statusLabel(m),
        tabindex: '-1',
      }, el('span', { class: 'micro-cube', 'aria-hidden': 'true' }, lane.symbol));
      button.style.setProperty('--delay', '-' + ((i % 9) * 0.47).toFixed(2) + 's');
      return button;
    }));
    renderFilters();
    applyFilter();
    renderOverview();
  }

  // The note is a status region: it is rebuilt, and so announced, only when its
  // content changes, and an open Diagnostics list stays open.
  function renderNote(list) {
    const diagnostics = list.complete ? [] : (list.diagnostics || []).slice(0, 50);
    const key = JSON.stringify([Boolean(list.complete), Boolean(list.truncated), (list.diagnostics || []).length, diagnostics]);
    if (key === state.noteKey) return;
    state.noteKey = key;
    const note = $('#inventory-note');
    const wasOpen = Boolean(note.querySelector('details[open]'));
    note.replaceChildren();
    if (!list.complete) {
      const count = (list.diagnostics || []).length;
      note.append(el('strong', null, list.truncated ? 'Inventory truncated by safety bounds. ' : 'Inventory incomplete. '),
        count + ' ledger problem' + (count === 1 ? '' : 's') + ' reported; those Molecules are not shown and are not counted.',
        count ? el('details', { open: wasOpen }, el('summary', null, 'Diagnostics'), el('ul', null, diagnostics.map((d) => el('li', null, d)))) : null);
    }
    note.hidden = Boolean(list.complete);
  }

  function renderFilters() {
    const counts = (state.list && state.list.counts) || {};
    $('#run-filters').replaceChildren(...FILTERS.map(([id, label]) => {
      if (id === 'paused') {
        return el('button', { type: 'button', class: 'filter-chip', disabled: true, title: 'The engine has no pause semantics', 'aria-disabled': 'true' }, label, el('span', null, 'Unavailable'));
      }
      return el('button', {
        type: 'button', class: 'filter-chip', dataset: { filter: id }, 'aria-pressed': String(state.filter === id),
        onclick: () => { hideTip(); state.filter = id; renderFilters(); applyFilter(); $('[data-filter="' + id + '"]').focus({ preventScroll: true }); },
      }, label, el('span', null, String(id === 'all' ? counts.all || 0 : counts[id] || 0)));
    }));
  }

  function cells() { return [...document.querySelectorAll('#cubes [data-id]')]; }

  function applyFilter() {
    const byId = new Map(molecules().map((m) => [m.id, m]));
    const all = cells();
    all.forEach((b) => { b.hidden = !matches(byId.get(b.dataset.id)); b.tabIndex = -1; });
    const shown = all.filter((b) => !b.hidden);
    const chosen = shown.find((b) => b.dataset.id === state.selectedId) || shown[0];
    if (chosen) chosen.tabIndex = 0;
    const empty = $('#filter-empty');
    empty.hidden = shown.length > 0;
    empty.textContent = molecules().length === 0 ? 'No Molecules are recorded in this Lab yet.' : state.query.trim() ? 'No matching Molecules in this filter. Clear search or choose All.' : 'No Molecules in this lane.';
    $('#clear-search').hidden = !state.query;
    const label = (FILTERS.find(([id]) => id === state.filter) || FILTERS[0])[1];
    $('#search-status').textContent = state.query.trim() ? shown.length + ' matching ' + (shown.length === 1 ? 'Molecule' : 'Molecules') + ' · ' + (state.filter === 'all' ? 'all lanes' : label) : '';
    const outside = $('#outside-filter');
    const selected = state.selectedId && byId.get(state.selectedId);
    outside.hidden = !(state.selectedId && !$('#selected').hidden && !(selected && matches(selected)));
    outside.textContent = selected ? 'The selected Molecule ' + selected.id + ' is outside this filter; its details stay open below.' : 'The selected Molecule is no longer in the inventory.';
  }

  $('#molecule-search').addEventListener('input', (e) => { hideTip(); state.query = e.target.value; applyFilter(); });
  function clearSearch() { state.query = ''; $('#molecule-search').value = ''; applyFilter(); $('#molecule-search').focus({ preventScroll: true }); }
  $('#clear-search').addEventListener('click', clearSearch);
  $('#molecule-search').addEventListener('keydown', (e) => { if (e.key === 'Escape' && state.query) { e.preventDefault(); clearSearch(); } });

  const cubes = $('#cubes');
  cubes.addEventListener('click', (e) => {
    const button = e.target.closest('[data-id]');
    if (button) { hideTip(); select(button.dataset.id); }
  });
  // Seven-row, column-major movement among visible cells, as in the prototype.
  cubes.addEventListener('keydown', (e) => {
    const button = e.target.closest('[data-id]');
    if (!button) return;
    const shown = cells().filter((b) => !b.hidden);
    const at = shown.indexOf(button);
    const delta = { ArrowRight: 7, ArrowLeft: -7, ArrowDown: 1, ArrowUp: -1 }[e.key];
    let next = null;
    if (delta !== undefined) next = shown[Math.max(0, Math.min(shown.length - 1, at + delta))];
    if (e.key === 'Home') next = shown[0];
    if (e.key === 'End') next = shown[shown.length - 1];
    if (next) {
      e.preventDefault();
      button.tabIndex = -1;
      next.tabIndex = 0;
      next.focus();
    }
  });
  cubes.addEventListener('pointerover', (e) => { const b = e.target.closest('[data-id]'); if (b) showTip(b); });
  cubes.addEventListener('pointerout', (e) => { if (e.target.closest('[data-id]')) queueHideTip(); });
  cubes.addEventListener('focusin', (e) => { const b = e.target.closest('[data-id]'); if (b) showTip(b); });
  cubes.addEventListener('focusout', hideTip);

  // ---- Tooltip --------------------------------------------------------------
  let tipTimer = null, tipAnchor = null;
  const tip = $('#cube-tooltip');
  function hideTip() { clearTimeout(tipTimer); tip.hidden = true; if (tipAnchor) tipAnchor.removeAttribute('aria-describedby'); tipAnchor = null; }
  function queueHideTip() { clearTimeout(tipTimer); tipTimer = setTimeout(hideTip, 180); }
  function showTip(button) {
    const m = molecules().find((x) => x.id === button.dataset.id);
    if (!m) return;
    clearTimeout(tipTimer);
    if (tipAnchor) tipAnchor.removeAttribute('aria-describedby');
    tipAnchor = button;
    tip.replaceChildren(
      el('span', { class: 'label' }, 'MOLECULE #' + m.number + ' · ' + m.id),
      el('strong', null, m.title || 'Untitled'),
      el('dl', null, el('dt', null, 'Current Atom'), el('dd', null, m.currentAtom || 'None'), el('dt', null, 'Lane'), el('dd', null, laneOf(m).label), el('dt', null, 'Status'), el('dd', null, notRecorded(m.status) + (m.phase ? ' · ' + m.phase : ''))));
    tip.hidden = false;
    button.setAttribute('aria-describedby', 'cube-tooltip');
    const box = button.getBoundingClientRect();
    const width = tip.offsetWidth;
    tip.style.left = Math.max(12, Math.min(innerWidth - width - 12, box.left + box.width / 2 - width / 2)) + 'px';
    tip.style.top = (box.top > tip.offsetHeight + 16 ? box.top - tip.offsetHeight - 8 : box.bottom + 8) + 'px';
  }
  tip.addEventListener('pointerenter', () => clearTimeout(tipTimer));
  tip.addEventListener('pointerleave', hideTip);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideTip(); });
  window.addEventListener('resize', hideTip);
  document.addEventListener('scroll', hideTip, true);

  // ---- Selection --------------------------------------------------------------
  function select(id) {
    if (id === state.selectedId && !$('#selected').hidden) { overview(); return; }
    state.selectedId = id;
    state.detail = null;
    state.atomIndex = null;
    $('#selected').hidden = false;
    $('#aggregate').hidden = true;
    cells().forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.id === id)));
    const m = molecules().find((x) => x.id === id);
    $('#selection-label').textContent = m ? 'Selected #' + m.number + ' · ' + (m.title || m.id) : 'Selected ' + id;
    renderLoadingDetail(id);
    applyFilter();
    loadDetail(id, true);
  }

  function overview() {
    state.selectedId = null;
    state.detail = null;
    state.detailToken++;
    $('#selected').hidden = true;
    $('#aggregate').hidden = false;
    cells().forEach((b) => b.setAttribute('aria-pressed', 'false'));
    $('#selection-label').textContent = 'All Molecules · select a cell to inspect';
    applyFilter();
  }

  function renderLoadingDetail(id) {
    $('#run-id').textContent = id;
    $('#run-title').textContent = 'Loading…';
    $('#run-state').textContent = '';
    for (const s of ['#run-origin', '#molecule-totals', '#atoms', '#readiness', '#attempts']) $(s).replaceChildren();
  }

  function renderMissingDetail(id, status) {
    renderLoadingDetail(id);
    $('#run-title').textContent = status === 404 ? 'This Molecule is no longer in the Lab.' : 'This ledger was refused: it is invalid, unsafe, oversized or not this Lab\'s.';
    $('#run-origin').append(el('button', { type: 'button', class: 'text-button', onclick: overview }, 'Clear selection'));
  }

  // ---- Selected Molecule -------------------------------------------------------
  function attemptsById(d) { return new Map((d.attempts || []).map((a) => [a.id, a])); }
  function costText(usage) {
    if (!usage || typeof usage.totalCostUsd !== 'number') return 'Not recorded';
    const basis = usage.costBasis === 'list' ? 'CLI list-price estimate' : usage.costBasis ? 'basis ' + usage.costBasis : 'basis unavailable';
    return '$' + usage.totalCostUsd.toFixed(4) + ' (' + basis + ')';
  }

  function renderDetail() {
    const d = state.detail;
    const lane = LANES[d.lane] || LANES.other;
    $('#run-id').textContent = d.id;
    const pill = $('#run-state');
    pill.textContent = lane.label + ' · ' + notRecorded(d.status) + (d.phase ? ' · ' + d.phase : '');
    pill.className = 'pill' + (d.lane === 'decision' ? ' current' : '');
    $('#run-title').textContent = (d.sample ? '[Sample] ' : '') + (d.issue.title || 'Untitled');
    const issueLink = externalLink(d.issue.url, ISSUE_URL, 'Issue #' + d.issue.number + ' ↗');
    const pullLink = d.delivery ? externalLink(d.delivery.url, PULL_URL, 'Pull request ↗') : null;
    $('#run-origin').replaceChildren(
      el('span', null, notRecorded(d.issue.repository) + ' #' + d.issue.number + (d.issue.state ? ' · ' + d.issue.state : '')),
      issueLink || el('span', { class: 'muted' }, 'Issue link unavailable'),
      pullLink || el('span', { class: 'muted' }, d.delivery ? 'PR link unavailable' : 'PR · not created'),
      el('button', { type: 'button', class: 'text-button', dataset: { origin: 'issue-text' }, onclick: () => show('Issue text · ' + d.id, d.issue.body || 'No Issue text recorded.') }, 'Issue text ↗'),
      d.sample ? null : el('button', { type: 'button', class: 'text-button', dataset: { origin: 'ledger' }, onclick: () => showLedger(d.id) }, 'Ledger record ↗'),
      el('button', { type: 'button', class: 'text-button', id: 'clear-selection', dataset: { origin: 'clear' }, onclick: overview }, 'Clear selection'));
    if (issueLink) issueLink.dataset.origin = 'issue';
    if (pullLink) pullLink.dataset.origin = 'pull';

    const wall = elapsed(d.startedAt, d.finishedAt);
    const attempts = d.attempts || [];
    const costs = attempts.filter((a) => a.usage && typeof a.usage.totalCostUsd === 'number');
    $('#molecule-totals').replaceChildren(
      el('div', null, el('span', { class: 'label' }, 'WALL-CLOCK DURATION'), el('strong', null, wall !== null ? durationLabel(wall) : d.startedAt ? 'Not finished' : 'Not recorded'),
        el('small', null, 'Started ' + notRecorded(d.startedAt) + (d.finishedAt ? ' · finished ' + d.finishedAt : ''))),
      el('div', null, el('span', { class: 'label' }, 'REPORTED COST · PARTIAL'),
        el('strong', null, costs.length ? '$' + costs.reduce((n, a) => n + a.usage.totalCostUsd, 0).toFixed(4) : 'Not recorded'),
        el('small', null, 'Cost reported for ' + costs.length + ' of ' + attempts.length + ' harness attempts · unknown is not zero')),
      el('div', null, el('span', { class: 'label' }, 'WORKSPACE'), el('strong', { class: 'mono-text' }, notRecorded(d.workspace.branch)),
        el('small', { title: d.workspace.baseCommit || '' }, 'Base commit ' + short(d.workspace.baseCommit))));

    $('#formula-name').textContent = notRecorded(d.formulaId) + (d.formula.approved ? ' · approved snapshot' : ' · snapshot not marked approved');
    const sha = $('#formula-sha');
    sha.textContent = 'sha256 ' + notRecorded(d.formula.sha256);
    const delivery = $('#run-delivery');
    delivery.hidden = !d.delivery;
    if (d.delivery) {
      delivery.textContent = d.delivery.label + (d.delivery.published ? ' · merge and production unverified' : '') + (d.delivery.error ? ' · ' + d.delivery.error : '');
    }

    renderAtoms(d);
    renderReadiness(d);
    renderAttempts(d);
    const diagnostics = $('#run-diagnostics');
    diagnostics.replaceChildren(...(d.diagnostics || []).map((text) => el('li', null, text)));
    diagnostics.hidden = !(d.diagnostics || []).length;
    renderAction(d);
  }

  function renderAction(d) {
    const current = d.currentAtom ? ' · ' + d.currentAtom : '';
    const copy = {
      decision: ['Needs your decision' + current, 'This Molecule waits at a human gate (phase ' + notRecorded(d.phase) + '). Review and decide with the forgecell CLI; approvals are not available in this UI.'],
      running: ['Running' + current, 'The Lab is working within the approved scope. This view updates from the ledger.'],
      blocked: ['Blocked' + current, 'Status ' + notRecorded(d.status) + '. Failure evidence stays in the ledger; a failed or blocked run is not a delivery.' + (d.delivery ? ' ' + d.delivery.label + '.' : '')],
      finished: ['Draft published', 'A publication receipt is recorded. Merge and production outcomes are not verified here.'],
      other: ['Recorded · ' + notRecorded(d.status), d.delivery ? d.delivery.label + '.' : 'No published delivery receipt. This Molecule is in no lane; it appears under All and Other.'],
    }[d.lane] || ['Recorded', ''];
    $('#action-title').textContent = copy[0];
    $('#action-copy').textContent = copy[1];
  }

  // Atom statuses the engine writes (done, failed, active, waiting, skipped,
  // blocked, pending) and the sample fixture's labels, mapped to a tab mark and
  // class; anything else shows its step number.
  const ATOM_MARKS = {
    done: ['✓', 'is-done'], completed: ['✓', 'is-done'], complete: ['✓', 'is-done'], passed: ['✓', 'is-done'], succeeded: ['✓', 'is-done'],
    approved: ['✓', 'is-done'], recorded: ['✓', 'is-done'], 'draft published': ['✓', 'is-done'],
    failed: ['✕', 'is-failed'], blocked: ['!', 'is-failed'], skipped: ['–', 'is-skipped'],
    waiting: ['◇', 'is-waiting'], 'needs you': ['◇', 'is-waiting'],
    active: [null, 'is-running'], running: [null, 'is-running'],
  };
  function atomMark(status, i) {
    const [mark, cls] = ATOM_MARKS[String(status || '').toLowerCase()] || [null, ''];
    return { mark: mark || String(i + 1).padStart(2, '0'), cls };
  }
  function defaultAtom(d) {
    const atoms = d.atoms || [];
    const current = atoms.findIndex((a) => a.id === d.currentAtom);
    if (current >= 0) return current;
    const failedAt = atoms.findIndex((a) => /fail|block/i.test(a.status || ''));
    return failedAt >= 0 ? failedAt : 0;
  }

  function renderAtoms(d) {
    const atoms = d.atoms || [];
    const box = $('#atoms');
    if (!atoms.length) {
      box.replaceChildren(el('p', { class: 'scope-note' }, 'No Atoms are recorded for this Molecule.'));
      return;
    }
    if (state.atomIndex === null || state.atomIndex >= atoms.length) state.atomIndex = defaultAtom(d);
    const flow = el('div', { class: 'atom-flow', role: 'tablist', 'aria-label': 'Molecule Atoms' }, atoms.map((a, i) => {
      const { mark, cls } = atomMark(a.status, i);
      return el('button', {
        type: 'button', role: 'tab', id: 'atom-tab-' + i, class: 'atom-tab' + (cls ? ' ' + cls : ''),
        'aria-selected': String(i === state.atomIndex), 'aria-controls': 'atom-detail', tabindex: i === state.atomIndex ? '0' : '-1', dataset: { atom: String(i) },
      }, el('span', { class: 'atom-number', 'aria-hidden': 'true' }, mark), el('strong', null, a.id), el('small', null, notRecorded(a.status)));
    }));
    flow.addEventListener('click', (e) => { const b = e.target.closest('[data-atom]'); if (b) chooseAtom(Number(b.dataset.atom)); });
    flow.addEventListener('keydown', (e) => {
      const b = e.target.closest('[data-atom]');
      if (!b) return;
      const i = Number(b.dataset.atom), n = atoms.length;
      const next = { ArrowRight: (i + 1) % n, ArrowLeft: (i + n - 1) % n, Home: 0, End: n - 1 }[e.key];
      if (next !== undefined) { e.preventDefault(); chooseAtom(next); }
    });
    box.replaceChildren(flow, atomDetail(d, atoms[state.atomIndex], state.atomIndex));
  }

  function chooseAtom(i) {
    state.atomIndex = i;
    renderAtoms(state.detail);
    const tab = $('#atom-tab-' + i);
    if (tab) tab.focus({ preventScroll: true });
  }

  function tags(values, cls) { return (values || []).filter(Boolean).map((v) => el('span', { class: 'access-tag' + (cls ? ' ' + cls : '') }, v)); }

  function atomDetail(d, a, i) {
    const p = a.provenance || {};
    const actions = p.actions || [];
    const byId = attemptsById(d);
    const attempt = actions.map((x) => byId.get(x.attemptId)).filter(Boolean).pop();
    const binding = actions.map((x) => x.executedBinding).filter(Boolean).pop() || p.declaredBinding;
    const ms = a.elapsedMs || elapsed(a.startedAt, a.finishedAt);
    const observed = actions.filter((x) => x.observed);
    return el('div', { id: 'atom-detail', class: 'atom-detail', role: 'tabpanel', 'aria-labelledby': 'atom-tab-' + i, tabindex: '0' },
      el('div', { class: 'atom-detail-head' }, el('strong', null, a.id), el('span', null, notRecorded(a.status)), el('span', { class: 'source' }, notRecorded(a.type))),
      el('dl', { class: 'atom-meta' },
        el('div', null, el('dt', null, 'Harness / binding'), el('dd', null, notRecorded(binding))),
        el('div', null, el('dt', null, 'Model'), el('dd', null, attempt && attempt.model ? attempt.model : 'Not recorded')),
        el('div', null, el('dt', null, 'Duration'), el('dd', null, ms ? durationLabel(ms) : 'Not recorded')),
        el('div', null, el('dt', null, 'Cost'), el('dd', null, costText(attempt && attempt.usage)))),
      // Planned capabilities: what the Formula and plan declared. Nothing here
      // says the Atom used them.
      el('div', { class: 'atom-access' },
        el('span', { class: 'label' }, 'PLANNED CAPABILITIES · DECLARED, NOT OBSERVED'),
        el('div', null, tags([p.declaredSource && 'source ' + p.declaredSource, p.declaredBinding && 'binding ' + p.declaredBinding]), tags(p.declaredWorkflows, 'source-tag'), (!p.declaredSource && !p.declaredBinding && !(p.declaredWorkflows || []).length) ? el('span', { class: 'muted' }, 'None declared') : null)),
      el('dl', { class: 'atom-behavior' },
        el('dt', null, 'Purpose'), el('dd', null, notRecorded(p.declaredPurpose)),
        el('dt', null, 'Planned action'), el('dd', null, notRecorded(p.plannedAction)),
        el('dt', null, 'Detail'), el('dd', null, notRecorded(a.detail)),
        el('dt', null, 'Execution'), el('dd', null, notRecorded(p.execution), p.skipReason ? el('small', null, 'Skip reason: ' + p.skipReason) : null),
        typeof a.exitCode === 'number' ? [el('dt', null, 'Exit code'), el('dd', null, String(a.exitCode))] : null),
      // Actions taken: only what an observation recorded, with its evidence.
      el('div', { class: 'atom-actions' },
        el('span', { class: 'label' }, 'ACTIONS TAKEN · OBSERVED'),
        observed.length ? observed.map((x) => el('div', { class: 'atom-action' },
          el('p', null, el('strong', null, notRecorded(x.observed.action) + ' → ' + notRecorded(x.observed.result)), el('small', null, 'Attempt ' + notRecorded(x.attemptId) + ' · ' + notRecorded(x.startedAt) + ' → ' + notRecorded(x.observed.finishedAt))),
          x.observed.externalAction ? el('p', { class: 'scope-note' }, 'External: ' + x.observed.externalAction + ' → ' + notRecorded(x.observed.externalResult)) : null,
          (x.observed.evidence || []).length
            ? el('ul', { class: 'evidence-list' }, x.observed.evidence.map((e) => evidenceItem(d, e)))
            : el('p', { class: 'scope-note' }, 'No evidence references recorded.')))
          : el('p', { class: 'scope-note' }, actions.length ? 'Actions started; no observation recorded yet.' : 'No observed actions recorded.')),
      // Context sources: what each started action resolved and ran under, as
      // recorded, kept apart from the declarations above.
      el('div', { class: 'atom-actions' },
        el('span', { class: 'label' }, 'CONTEXT SOURCES · AS RECORDED PER ACTION'),
        actions.length
          ? el('ul', { class: 'record-list' }, actions.map((x) => el('li', null,
            el('strong', null, 'Attempt ' + notRecorded(x.attemptId)),
            el('span', null, 'resolved source ' + notRecorded(x.resolvedSource)),
            el('span', null, 'binding ' + notRecorded(x.executedBinding)),
            el('span', null, 'planned ' + notRecorded(x.plannedAction)),
            el('code', { title: x.planDigest || '' }, 'plan ' + short(x.planDigest)))))
          : el('p', { class: 'scope-note' }, 'No actions started; no context source recorded.')),
      el('div', { class: 'atom-buttons' },
        el('button', { type: 'button', class: 'text-button', dataset: { origin: 'atom-record' }, onclick: () => show('Atom record · ' + a.id + ' · ' + d.id, JSON.stringify(a, null, 2)) }, 'Atom record ↗'),
        el('button', { type: 'button', class: 'text-button', dataset: { origin: 'atom-ledger' }, onclick: () => showLedger(d.id) }, 'Ledger record ↗')));
  }

  // One evidence reference. A file the ledger lists with a SHA-256 opens as
  // bounded, redacted text; the server checks the digest and the file again.
  function evidenceItem(d, e) {
    return el('li', null,
      el('code', null, e.path || e.pointer || 'reference'),
      e.sha256 ? el('small', { title: e.sha256 }, 'sha256 ' + short(e.sha256)) : null,
      e.note ? el('small', null, e.note) : null,
      e.viewable && e.path
        ? el('button', { type: 'button', class: 'text-button', dataset: { evidence: e.path }, onclick: () => showText('Evidence · ' + e.path + ' · ' + d.id, '/api/molecules/' + encodeURIComponent(d.id) + '/evidence?path=' + encodeURIComponent(e.path)) }, 'View ↗')
        : null);
  }

  function showLedger(id) { showText('Ledger record · ' + id, '/api/molecules/' + encodeURIComponent(id) + '/ledger'); }

  // showText fetches a bounded file view and shows it as text, with its size and
  // whether it was cut; a refusal shows the server's category message.
  async function showText(title, path) {
    try {
      const res = await api(path);
      const body = await res.json().catch(() => ({}));
      if (!res.ok) { show(title, 'Not served: ' + notRecorded(body && body.error), ''); return; }
      const size = typeof body.size === 'number' ? body.size : null;
      const note = body.truncated
        ? 'Truncated · showing the first part of ' + (size === null ? 'an unknown number of' : size) + ' bytes. Secrets and local paths are redacted.'
        : (size === null ? 'Size not recorded' : size + ' bytes') + ' · complete. Secrets and local paths are redacted.';
      show(title + (body.sha256 ? ' · sha256 ' + short(body.sha256) : ''), typeof body.text === 'string' ? body.text : '', note);
    } catch (error) { failed(error); }
  }

  function renderReadiness(d) {
    const r = d.readiness;
    const box = $('#readiness');
    if (!r) { box.replaceChildren(el('p', { class: 'scope-note' }, 'No readiness state recorded.')); return; }
    box.replaceChildren(
      el('p', { class: 'scope-note' }, 'Phase ' + notRecorded(r.phase)),
      el('ul', { class: 'record-list' }, (r.plans || []).map((p) => el('li', null, el('strong', null, 'Revision ' + p.revision), el('code', { title: p.digest }, 'digest ' + short(p.digest)), el('code', { title: p.parentDigest || '' }, p.parentDigest ? 'parent ' + short(p.parentDigest) : 'no parent')))),
      el('ul', { class: 'record-list events' }, (r.events || []).map((e) => el('li', null, el('span', { class: 'mono-text' }, notRecorded(e.at)), el('strong', null, notRecorded(e.kind)), e.detail ? el('span', null, e.detail) : null))));
  }

  function renderAttempts(d) {
    const attempts = d.attempts || [];
    $('#attempts').replaceChildren(attempts.length
      ? el('ul', { class: 'record-list' }, attempts.map((a) => {
        const u = a.usage, t = (u && u.total) || {};
        const tokens = u ? 'tokens in ' + notRecorded(t.input) + ' · out ' + notRecorded(t.output) : 'usage unavailable';
        return el('li', null, el('strong', null, a.kind + ' ' + a.id), el('span', null, notRecorded(a.status)), el('span', null, 'model ' + notRecorded(a.model || (u && (u.effectiveModels || []).join(', ')))), el('span', null, tokens), el('span', null, 'cost ' + costText(u)));
      }))
      : el('p', { class: 'scope-note' }, 'No harness attempts recorded.'));
  }

  $('#yaml').addEventListener('click', async () => {
    const id = state.selectedId;
    if (!id) return;
    try {
      const res = await api('/api/molecules/' + encodeURIComponent(id) + '?yaml=1');
      if (!res.ok) { show('Formula snapshot', 'The snapshot could not be read.'); return; }
      const d = await res.json();
      show('Formula snapshot · ' + notRecorded(d.formulaId) + ' · sha256 ' + short(d.formula.sha256), d.formula.yaml || 'No YAML recorded.');
    } catch (error) { failed(error); }
  });

  // ---- Overview ----------------------------------------------------------------
  function renderOverview() {
    const all = molecules();
    const counts = (state.list && state.list.counts) || {};
    const sample = Boolean(state.list && state.list.sample);
    $('#aggregate .panel-head .mono').textContent = sample ? 'LAB OVERVIEW · SAMPLE DATA' : 'LAB OVERVIEW';
    $('#aggregate-count').textContent = 'All ' + all.length + ' Molecules';
    const waiting = all.filter((m) => m.lane === 'decision');
    $('#review-queue').replaceChildren(...(waiting.length
      ? waiting.slice(0, 5).map((m) => el('button', { type: 'button', class: 'review-row', dataset: { review: m.id }, onclick: () => select(m.id) },
        el('span', { class: 'review-id' }, '#' + m.number), el('strong', null, m.title || m.id), el('span', null, notRecorded(m.phase)), el('span', { 'aria-hidden': 'true' }, '→')))
      : [el('p', { class: 'scope-note' }, 'No Molecules wait at a human gate.')]));
    if (waiting.length > 5) $('#review-queue').append(el('p', { class: 'scope-note' }, (waiting.length - 5) + ' more under Needs my decision.'));
    $('#outcome-bars').replaceChildren(...Object.entries(LANES).map(([id, lane]) => {
      const n = counts[id] || 0;
      const bar = el('span', { class: 'bar ' + lane.cls });
      bar.style.width = (all.length ? (n / all.length) * 100 : 0) + '%';
      return el('div', { class: 'outcome-row' }, el('span', null, lane.label), el('div', { class: 'bar-track' }, bar), el('strong', null, String(n)));
    }), el('div', { class: 'outcome-row' }, el('span', null, 'Paused'), el('div', { class: 'bar-track' }), el('strong', { title: 'The engine has no pause semantics' }, 'n/a')));
  }

  // ---- Dialog, motion and visibility -------------------------------------------
  let dialogReturn = null;
  function show(title, body, note) {
    dialogReturn = document.activeElement;
    $('#dialog-title').textContent = title;
    const noteBox = $('#dialog-note');
    noteBox.textContent = note || '';
    noteBox.hidden = !note;
    $('#dialog-body').textContent = body;
    $('#detail').showModal();
  }
  $('#close-dialog').addEventListener('click', () => $('#detail').close());
  $('#detail').addEventListener('close', () => { if (dialogReturn && dialogReturn.focus) dialogReturn.focus({ preventScroll: true }); });

  $('#motion-toggle').addEventListener('change', (e) => document.body.classList.toggle('motion-paused', !e.target.checked));
  document.addEventListener('visibilitychange', () => {
    document.body.classList.toggle('page-hidden', document.hidden);
    if (!document.hidden) poll();
  });
  if ('IntersectionObserver' in window) {
    new IntersectionObserver(([entry]) => document.body.classList.toggle('shelf-offscreen', !entry.isIntersecting)).observe($('.shelf'));
  }

  loadLab().then(poll).catch(failed);
  setInterval(poll, 5000);
  // The Lab header also changes without a ledger change, as when a Formula is
  // activated, so it is refreshed on its own slower interval.
  setInterval(() => { if (!document.hidden && !state.sessionLost) loadLab().catch(failed); }, 30000);
})();
