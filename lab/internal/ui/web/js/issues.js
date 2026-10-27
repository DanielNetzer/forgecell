// ---- Issues -----------------------------------------------------------------------
// Every open Issue of the active Formula's repository, from GET /api/issues,
// each with the one sync state the server computed (forgesync.SyncState): its
// pill is the state's label, its notes the server's fixed notes, and its one
// action Start intake only when the server says it is startable, otherwise Open
// Molecule. Nothing here derives a state. The filter, label, milestone and
// search live in the URL; polling patches rows in place, keyed by Issue number,
// and never rewrites an input being typed in.
import { $, el, fill, state, ISSUE_URL, MOLECULE_ID, moleculeHref, setText, setAttr, plainText, wholeNumber, counted, freshnessText, syncNotes, syncCountsText, dateTitle } from './core.js';

// The filters map the server's bucket only; All shows every row.
const FILTERS = [
  ['all', 'All', null],
  ['not-started', 'Not started', ['issues']],
  ['in-progress', 'In progress', ['running']],
  ['needs-you', 'Needs you', ['decision', 'blocked']],
  ['draft-pr', 'Draft PR', ['draft-pr']],
  ['merged', 'Merged', ['merged']],
];
// Tones of the state pill's dot, by server bucket; any other bucket is neutral.
const TONES = { issues: 'neutral', running: 'running', decision: 'needs', blocked: 'blocked', 'draft-pr': 'running', merged: 'merged', history: 'neutral' };
// Bounds on the URL values read back and on the labels shown per row.
const MAX_QUERY = 200, MAX_LABELS = 6;

const view = { controls: null, rows: null, announced: '' };

// query is the view's URL state: ?view=issues&filter=…&label=…&milestone=…&q=…
function query() {
  const p = new URLSearchParams(location.search);
  const text = (name) => (p.get(name) || '').slice(0, MAX_QUERY);
  const filter = p.get('filter');
  return { filter: FILTERS.some(([id]) => id === filter) ? filter : 'all', label: text('label').trim(), milestone: text('milestone').trim(), q: text('q') };
}
function queryHref(q) {
  const p = new URLSearchParams({ view: 'issues' });
  if (q.filter !== 'all') p.set('filter', q.filter);
  for (const name of ['label', 'milestone', 'q']) if (q[name].trim()) p.set(name, q[name].trim());
  if (new URLSearchParams(location.search).get('history') === '1') p.set('history', '1');
  return '?' + p.toString();
}
// setQuery moves the URL to q: a new entry for a filter or a submitted search,
// a replaced one while typing, so Back steps over keystrokes.
function setQuery(q, push) {
  const href = queryHref(q);
  if (location.search === href) return;
  if (push) history.pushState({ view: 'issues' }, '', href);
  else history.replaceState({ view: 'issues' }, '', href);
}

function rows() { return state.issues && Array.isArray(state.issues.issues) ? state.issues.issues : []; }
function stateOf(row) { const s = row && row.state; return s && typeof s === 'object' && typeof s.label === 'string' ? s : null; }
function inFilter(row, filter) {
  const f = FILTERS.find(([id]) => id === filter), s = stateOf(row);
  return !f || !f[2] || Boolean(s && f[2].includes(s.bucket));
}
function labelsOf(row) { return Array.isArray(row.labels) ? row.labels.filter((l) => typeof l === 'string' && l) : []; }
function matchesIssue(row, q) {
  if (q.label && !labelsOf(row).some((l) => l.toLowerCase() === q.label.toLowerCase())) return false;
  if (q.milestone && (typeof row.milestone !== 'string' || row.milestone.toLowerCase() !== q.milestone.toLowerCase())) return false;
  const terms = q.q.toLowerCase().trim().split(/\s+/).filter(Boolean);
  const haystack = ('#' + row.number + ' ' + plainText(row.title, '')).toLowerCase();
  return terms.every((t) => haystack.includes(t));
}

// drives reports the side panel's Start intake and Run the loop forms, which
// exist only when the Lab drives work through the engine.
function drives() { return Boolean($('#drive-run') && $('#drive-loop')); }

// startIntake submits the side panel's Start intake form with this Issue, so the
// one request flow (one in flight, errors inline, read back) is reused. The
// server checks GitHub again and refuses an Issue closed meanwhile.
function startIntake(number) {
  const form = $('#drive-run'), input = $('#drive-issue');
  if (!form || !input || !Number.isInteger(number) || number < 1) return;
  input.value = String(number);
  form.requestSubmit();
  input.focus({ preventScroll: true });
}

// runLoopOnThese fills the side panel's Run-the-loop selector with the current
// milestone or label filter. It never starts the loop: Start loop does.
function runLoopOnThese() {
  const q = query(), mode = q.milestone ? 'milestone' : q.label ? 'label' : 'oldest';
  const choose = $('#drive-loop [data-drive="mode-' + mode + '"]'), value = $('#drive-value');
  if (!choose || !value) return;
  choose.click();
  value.value = mode === 'milestone' ? q.milestone : mode === 'label' ? q.label : '';
  (mode === 'oldest' ? choose : value).focus();
}

function slot(node, name) { return node.querySelector('[data-slot="' + name + '"]'); }

function createIssueRow() {
  const start = el('button', { type: 'button', class: 'btn btn-primary issue-action', dataset: { slot: 'start' }, hidden: true });
  start.addEventListener('click', () => startIntake(Number(start.dataset.issue)));
  return el('div', { class: 'issue-row', role: 'row' },
    el('span', { class: 'issue-number', role: 'cell', dataset: { slot: 'number' } }),
    el('div', { class: 'issue-main', role: 'cell' },
      el('span', { class: 'issue-title', dataset: { slot: 'title' } }),
      el('span', { class: 'issue-labels', dataset: { slot: 'labels' } })),
    el('span', { class: 'issue-milestone', role: 'cell', dataset: { slot: 'milestone' } }),
    el('div', { class: 'issue-state', role: 'cell' },
      el('span', { class: 'issue-pill', dataset: { slot: 'pill' } }, el('span', { class: 'state-dot', 'aria-hidden': 'true' }), el('span', { dataset: { slot: 'label' } })),
      el('span', { class: 'issue-notes', dataset: { slot: 'notes' } })),
    el('div', { class: 'issue-act', role: 'cell' }, start,
      el('a', { class: 'btn btn-secondary issue-action', dataset: { slot: 'open' }, hidden: true }, 'Open Molecule'),
      el('span', { class: 'issue-reason', dataset: { slot: 'reason' }, hidden: true })));
}

// reasonText is the one plain line a row shows when it has neither Start intake
// nor a Molecule to open, so no row is left without a status. It reads only the
// server's freshness and completeness and this Lab UI's drive forms.
function reasonText(s) {
  const v = state.issues;
  if (!v || !v.checked) return 'Can’t start until GitHub state is checked';
  if (v.stale) return 'Can’t start until GitHub state is current';
  if (v.complete === false) return 'Can’t start while some ledgers are unreadable';
  if (s.startable === true && !drives()) return 'Start intake from the command line; this Lab UI doesn’t start work';
  return 'Nothing to start here · ' + s.label;
}

// updateIssueRow patches one row from its served IssueRow. Untrusted Issue text
// is set only as text; links only from the validated URL patterns.
function updateIssueRow(node, row) {
  const s = stateOf(row) || { label: 'Not checked', bucket: 'issues', notes: [] };
  const number = '#' + row.number, title = plainText(row.title, 'Untitled');
  const cell = slot(node, 'number');
  const url = typeof row.url === 'string' && ISSUE_URL.test(row.url) ? row.url : '';
  const link = cell.firstElementChild;
  if (url && (!link || link.getAttribute('href') !== url)) fill(cell, el('a', { href: url, target: '_blank', rel: 'noopener noreferrer', 'aria-label': 'Issue ' + number + ' on GitHub' }, number));
  else if (!url) setText(cell, number);
  setText(slot(node, 'title'), title);
  const labels = labelsOf(row).slice(0, MAX_LABELS), shown = slot(node, 'labels');
  if (shown.dataset.key !== JSON.stringify(labels)) { shown.dataset.key = JSON.stringify(labels); fill(shown, labels.map((l) => el('span', { class: 'issue-chip' }, l))); }
  setText(slot(node, 'milestone'), plainText(row.milestone, '—'));
  const pill = slot(node, 'pill');
  setAttr(pill, 'data-tone', TONES[s.bucket] || 'neutral');
  setText(slot(node, 'label'), s.label);
  setText(slot(node, 'notes'), syncNotes(s).join(' · '));
  // One primary per row: Start intake only when the server says startable and
  // the Lab drives work; otherwise Open Molecule when one decides the state.
  const start = slot(node, 'start'), open = slot(node, 'open');
  const startable = row.startable === true && s.startable === true && drives();
  // A Start intake that hides under the user hands its focus to the row's next
  // control, never to the page.
  const refocus = !startable && !start.hidden && start === document.activeElement;
  start.hidden = !startable;
  if (start.dataset.issue !== String(row.number)) start.dataset.issue = String(row.number);
  setText(start, 'Start intake');
  setAttr(start, 'aria-label', 'Start intake: ' + number + ' ' + title);
  const id = !startable && typeof row.latestMolecule === 'string' && MOLECULE_ID.test(row.latestMolecule) ? row.latestMolecule : '';
  open.hidden = !id;
  if (id) {
    setAttr(open, 'href', moleculeHref(id));
    if (open.dataset.molecule !== id) open.dataset.molecule = id;
    setAttr(open, 'aria-label', 'Open Molecule: ' + number + ' ' + title);
  }
  const reason = slot(node, 'reason'), why = !startable && !id ? reasonText(s) : '';
  setText(reason, why);
  reason.hidden = !why;
  if (refocus) (id ? open : $('#issues-title')).focus({ preventScroll: true });
}

// patchRows keeps the table body in order, keyed by Issue number: a shown row
// keeps its node, so focus on its action survives a poll. Rows no longer shown
// go first, so every node left is placed below; the node holding focus is never
// moved: the rows before it in the new order are moved past it instead.
function patchRows(container, items) {
  const keys = new Set(items.map((row) => String(row.number)));
  for (const node of [...container.children]) {
    if (keys.has(node.dataset.issue)) continue;
    if (node.contains(document.activeElement)) $('#issues-title').focus({ preventScroll: true });
    node.remove();
  }
  const existing = new Map([...container.children].map((node) => [node.dataset.issue, node]));
  let at = container.firstElementChild;
  for (const row of items) {
    const key = String(row.number);
    let node = existing.get(key);
    existing.delete(key);
    if (!node) { node = createIssueRow(); node.dataset.issue = key; }
    updateIssueRow(node, row);
    if (node === at) { at = at.nextElementSibling; continue; }
    if (node.parentNode === container && node.contains(document.activeElement)) {
      // Advance past the focused node: the rows ahead of it move to the end,
      // where a later item places them.
      while (at && at !== node) { const next = at.nextElementSibling; container.appendChild(at); at = next; }
      at = node.nextElementSibling;
      continue;
    }
    container.insertBefore(node, at);
  }
}

function renderIssueFilters(q, all) {
  const c = view.controls;
  for (const b of c.filters) {
    const id = b.dataset.filter;
    setAttr(b, 'aria-pressed', String(q.filter === id));
    setText(b.querySelector('.seg-count'), wholeNumber.format(all.filter((r) => inFilter(r, id)).length));
  }
}

// syncInputs shows the URL's label, milestone and search, but never rewrites the
// input that has focus.
function syncInputs(q) {
  const c = view.controls;
  for (const name of ['label', 'milestone', 'q']) {
    const input = c[name];
    if (document.activeElement !== input && input.value !== q[name]) input.value = q[name];
  }
}

function noteText(v) {
  if (!v) return '';
  const parts = [];
  if (typeof v.note === 'string' && v.truncated) parts.push(v.note.charAt(0).toUpperCase() + v.note.slice(1) + ' open Issues; the rest are not shown.');
  if (v.stale) parts.push('Couldn’t check GitHub just now, so nothing can be started until GitHub state is current.');
  if (v.checked && v.complete === false) parts.push('Some ledgers could not be read, so no Issue can be started right now.');
  if (!v.checked) parts.push('GitHub state was not checked, so no Issue is listed. Use Refresh to try again.');
  return parts.join(' ');
}

function renderIssues() {
  const c = view.controls;
  if (!c) return;
  const v = state.issues, q = query(), all = rows();
  syncInputs(q);
  renderIssueFilters(q, all);
  const shown = all.filter((r) => Number.isInteger(r.number) && r.number > 0 && inFilter(r, q.filter) && matchesIssue(r, q));
  patchRows(c.body, shown);
  const repo = v && typeof v.repository === 'string' && v.repository ? 'Open Issues in ' + v.repository + ' · ' : '';
  setText(c.fresh, repo + freshnessText(v));
  setAttr(c.fresh, 'title', v && v.fetchedAt ? dateTitle(v.fetchedAt) : '');
  setText(c.counts, syncCountsText());
  const note = noteText(v);
  setText(c.note, note);
  c.note.hidden = !note;
  // The Refresh button shows the one load in flight. It is never disabled, which
  // would drop its focus: aria-disabled and the click's early return hold it.
  setAttr(c.refresh, 'aria-disabled', String(state.syncPending));
  setText(c.refresh, state.syncPending ? 'Refreshing…' : 'Refresh');
  const empty = v && all.length ? (shown.length ? '' : 'No open Issue matches these filters.') : v && v.checked ? 'No open Issues.' : '';
  setText(c.empty, empty);
  c.empty.hidden = !empty;
  c.loop.hidden = !drives() || !(q.label || q.milestone);
  setText(c.loop, 'Run the loop on these' + (q.milestone ? ' · milestone ' + q.milestone : q.label ? ' · label ' + q.label : ''));
  // One persistent live region, changed only when its text changes.
  const live = v ? counted(shown.length, 'Issue shown', 'Issues shown') + (v.truncated ? ' of ' + wholeNumber.format(v.total) + ' open' : '') : '';
  if (view.announced !== live) { view.announced = live; setText(c.live, live); }
}

function readForm() {
  const c = view.controls, q = query();
  return { filter: q.filter, label: c.label.value.slice(0, MAX_QUERY), milestone: c.milestone.value.slice(0, MAX_QUERY), q: c.q.value.slice(0, MAX_QUERY) };
}

// The Issues view mounts on its data-view-panel: the filters, the search form,
// Refresh and Run the loop on these. Its markup stays in index.html.
export function mount(root, ctx) {
  const c = {
    body: $('#issues-rows'), fresh: $('#issues-fresh'), counts: $('#issues-counts'), note: $('#issues-note'), empty: $('#issues-empty'),
    refresh: $('#issues-refresh'), loop: $('#issues-loop'), live: $('#issues-live'), form: $('#issues-form'),
    label: $('#issues-label'), milestone: $('#issues-milestone'), q: $('#issues-q'), filters: [],
  };
  c.filters = FILTERS.map(([id, label]) => el('button', { type: 'button', class: 'seg', 'aria-pressed': 'false', dataset: { filter: id },
    onclick: () => { setQuery(Object.assign(readForm(), { filter: id }), true); renderIssues(); } },
  label, ' ', el('span', { class: 'seg-count' })));
  fill($('#issues-filters'), c.filters);
  view.controls = c;
  c.form.addEventListener('submit', (e) => { e.preventDefault(); setQuery(readForm(), true); renderIssues(); });
  for (const name of ['label', 'milestone', 'q']) c[name].addEventListener('input', () => { setQuery(readForm(), false); renderIssues(); });
  c.refresh.addEventListener('click', () => { if (state.syncPending) return; ctx.loadSync(true); });
  c.loop.addEventListener('click', runLoopOnThese);
}

// update patches the view from state.issues and the URL.
export function update(state) { renderIssues(); }
