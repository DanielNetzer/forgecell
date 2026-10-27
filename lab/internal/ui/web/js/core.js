// Forgecell local Lab UI: the core every view imports. It holds the page state,
// the router and URL state, the API session, the DOM patch helpers, the Intl
// formatters and the text dialog. Every value from the API is untrusted and
// reaches the page only as a text node or a validated attribute: no module
// parses data as markup.

export const $ = (selector) => document.querySelector(selector);

// el builds an element. Strings among children become text nodes; props are
// fixed by the calling module, never taken from data.
export function el(tag, props, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key === 'class') node.className = value;
    else if (key === 'text') node.textContent = String(value);
    else if (key === 'dataset') Object.assign(node.dataset, value);
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? '' : String(value));
  }
  return add(node, ...children);
}

// add appends children to node: nodes as they are, strings as text nodes, and
// null, undefined and false not at all, so no child ever renders as "null".
export function add(node, ...children) {
  for (const child of children.flat()) {
    if (child === undefined || child === null || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

// fill replaces node's children, skipping null children as add does. Every
// render goes through fill, never through replaceChildren with arguments.
export function fill(node, ...children) {
  node.replaceChildren();
  return add(node, ...children);
}

export const ISSUE_URL = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/issues\/[1-9][0-9]*$/;
export const PULL_URL = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/[1-9][0-9]*$/;
export const REPOSITORY = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

export function externalLink(url, pattern, label) {
  if (typeof url !== 'string' || !pattern.test(url)) return null;
  return el('a', { href: url, target: '_blank', rel: 'noopener noreferrer' }, label);
}

// One lane per Molecule, assigned by the server; Paused has no engine semantics.
export const LANES = {
  running: { label: 'Running', short: 'running', symbol: '', cls: 'working' },
  decision: { label: 'Needs my decision', short: 'need you', symbol: '◇', cls: 'decision' },
  blocked: { label: 'Blocked', short: 'blocked', symbol: '!', cls: 'failed' },
  finished: { label: 'Draft published', short: 'draft published', symbol: '✓', cls: 'finished' },
  other: { label: 'Other', short: 'other', symbol: '·', cls: 'other' },
};
// Retired is finished history: its own lane and filter, never part of All,
// Needs my decision or the headline counts, but shown in analytics.
export const RETIRED = 'retired';
export const ALL_LANES = Object.assign({}, LANES, { retired: { label: 'Retired', short: 'retired', symbol: '–', cls: 'other' } });

export const state = { lab: null, list: null, etag: null, sync: null, issues: null, syncPending: false, syncToken: 0, analytics: null, analyticsEtag: null, analyticsToken: 0, filter: 'all', query: '', selectedId: null, detail: null, detailText: null, atomIndex: null, detailToken: 0, labToken: 0, pollToken: 0, polling: false, noteKey: null, sessionLost: false, gate: newGate(null) };

// gate is the selected Molecule's gate state: one request in flight at most,
// the last preview shown, and the last job read back from the server.
export function newGate(id) { return { id: id, pending: false, preview: null, job: null, stop: null, retire: null, amend: null, reason: '', message: '', where: 'decision', announce: false, title: '', accept: false, timer: null }; }

// ctx is the one bridge between modules: each view's mount registers the calls
// another module makes into it (openMolecule, hideTip, renderAnalytics, …), so
// no view imports another. The entry passes this same object to every mount.
export const ctx = {};

export function notRecorded(value) { return value === undefined || value === null || value === '' ? 'Not recorded' : String(value); }
export function short(digest) { return typeof digest === 'string' && digest.length > 12 ? digest.slice(0, 12) + '…' : notRecorded(digest); }
export function durationLabel(ms) {
  const total = Math.round(ms / 1000), h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60), s = total % 60;
  return [h ? h + 'h' : '', m ? m + 'm' : '', s || !total ? s + 's' : ''].filter(Boolean).join(' ');
}
export function elapsed(start, end) {
  const a = Date.parse(start), b = Date.parse(end);
  return Number.isFinite(a) && Number.isFinite(b) && b >= a ? b - a : null;
}

// ---- API ----------------------------------------------------------------
// Every API request carries this tab's session value, kept in sessionStorage
// by session.js at launch; the cookie alone is not enough. Without it, or once
// the server refuses it, the page stops asking instead of looping.
export class SessionError extends Error {}
export function tabSession() {
  try { return sessionStorage.getItem('forgecell-session') || ''; } catch (error) { return ''; }
}
export async function api(path, headers) {
  const session = tabSession();
  if (!session || state.sessionLost) throw new SessionError('no session');
  const res = await fetch(path, { credentials: 'same-origin', cache: 'no-store', headers: Object.assign({ 'X-Forgecell-Session': session }, headers || {}) });
  if (res.status === 403) throw new SessionError('forbidden');
  return res;
}
export function showError(message) {
  const banner = $('#load-error');
  banner.textContent = message;
  banner.hidden = !message;
}
export function failed(error) {
  if (error instanceof SessionError) state.sessionLost = true;
  showError(error instanceof SessionError
    ? 'This tab has no Lab UI session. Each launch link works once: run forgecell ui again (or restart it) and open the new link it prints.'
    : 'The Lab could not be read. The page keeps the last data it loaded and will retry.');
}

// ---- Focus across re-renders ----------------------------------------------
// Re-rendered controls are found again by these data attributes, or else by
// their id, so keyboard focus survives a poll that rebuilds them.
const FOCUS_KEYS = ['id', 'filter', 'atom', 'origin', 'review', 'evidence', 'gate', 'drive'];
export function keepFocus(render) {
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

// cells are the Work grid's Molecule cells.
export function cells() { return [...document.querySelectorAll('#cubes [data-id]')]; }

// ---- Molecules in the list ----------------------------------------------------
export function molecules() { return (state.list && state.list.molecules) || []; }
export function laneOf(m) { return ALL_LANES[m.lane] || LANES.other; }
export function live(m) { return m.lane !== RETIRED; }
// A running Molecule waiting for the Lab's verification slot reads as queued,
// not running. Only the one known activity is shown; any other value is ignored.
const QUEUED = 'queued for checks';
function queued(m) { return m.lane === 'running' && m.activity === QUEUED; }
export function laneLabel(m) { return queued(m) ? 'Queued for checks' : laneOf(m).label; }

const NEEDS_KINDS = ['scope', 'publish', 'blocked'];
export function inboxOf(m) {
  const v = m && m.inbox;
  return v && typeof v === 'object' && typeof v.kind === 'string' && v.kind && MOLECULE_ID.test(m.id) ? v : null;
}
function timeOf(m) { const t = Date.parse(m.finishedAt || m.startedAt); return Number.isFinite(t) ? t : 0; }
export function newestFirst(a, b) { return timeOf(b) - timeOf(a) || (a.id < b.id ? -1 : 1); }
// needsYou is the one definition of what needs you: the Molecules shown under
// Needs you (scope gate, ready to publish, blocked), newest first. The headline,
// the live region, the nav badge and the resident all count it. Once /api/sync
// is loaded its needsYou flags decide (row 14 included); the ledger kinds stand
// in only until then.
export function needsYou() {
  const served = servedNeedsYou();
  return molecules().filter((m) => {
    const v = live(m) && inboxOf(m);
    if (served) return Boolean(v) && Object.prototype.hasOwnProperty.call(served, m.id) && served[m.id] === true;
    return Boolean(v) && NEEDS_KINDS.includes(v.kind);
  }).sort(newestFirst);
}
// servedNeedsYou is /api/sync's needsYou map, or null before it is loaded.
function servedNeedsYou() {
  const n = state.sync && state.sync.needsYou;
  return n && typeof n === 'object' && !Array.isArray(n) ? n : null;
}
// syncCounts is /api/sync's bucket counts, or null before it is loaded.
export function syncCounts() {
  const c = state.sync && state.sync.counts;
  return c && typeof c === 'object' && !Array.isArray(c) ? c : null;
}

// ---- GitHub sync (#242) -------------------------------------------------------
// state.sync is GET /api/sync and state.issues GET /api/issues. Every label,
// bucket, note and startable flag is the server's forgesync.DisplayState: no
// module derives a sync state. These helpers only look a state up and map its
// server bucket to the surfaces that show it.
const CLOSED_IN_PROGRESS = 'issue-closed-in-progress';
const HISTORY_BUCKETS = ['merged', 'history'];
// syncOf is the DisplayState of the Molecule id, or null when none was served.
export function syncOf(id) {
  const all = state.sync && state.sync.molecules;
  const d = all && typeof all === 'object' && MOLECULE_ID.test(id || '') && Object.prototype.hasOwnProperty.call(all, id) ? all[id] : null;
  return d && typeof d === 'object' && typeof d.label === 'string' && typeof d.bucket === 'string' ? d : null;
}
// syncNotes are a state's fixed server notes, bounded.
export function syncNotes(d) { return d && Array.isArray(d.notes) ? d.notes.filter((n) => typeof n === 'string' && n).slice(0, 8) : []; }
// closedInProgress is row 14: the Issue closed while its Molecule was live. It
// shows under Needs you on every surface.
export function closedInProgress(m) { const d = live(m) && syncOf(m.id); return Boolean(d) && d.kind === CLOSED_IN_PROGRESS; }
// inHistory is a Molecule the sync files as merged or closed history: Inbox and
// Work collapse these into one row unless ?history=1.
export function inHistory(m) { const d = live(m) && syncOf(m.id); return Boolean(d) && HISTORY_BUCKETS.includes(d.bucket); }
export function historyFromURL() { return new URLSearchParams(location.search).get('history') === '1'; }
// historyHref is this URL with the history toggle set or cleared, every other
// parameter kept.
export function historyHref(on) {
  const q = new URLSearchParams(location.search);
  if (on) q.set('history', '1'); else q.delete('history');
  const s = q.toString();
  return s ? '?' + s : '?view=' + (state.view || 'inbox');
}
// followHistory is the click handler of a history toggle link: a plain click
// moves through history.pushState, so Back and reload keep the choice.
export function followHistory(e) {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  e.preventDefault();
  history.pushState(history.state, '', e.currentTarget.getAttribute('href'));
  ctx.renderSync();
}
// historyRow patches a collapsed history toggle in place: "38 merged · 2 closed ·
// show", or "Hide merged and closed" while shown.
export function historyRow(link, list) {
  const merged = list.filter((m) => syncOf(m.id).bucket === 'merged').length, closed = list.length - merged;
  const on = historyFromURL();
  const parts = [merged ? wholeNumber.format(merged) + ' merged' : '', closed ? wholeNumber.format(closed) + ' closed' : ''].filter(Boolean);
  setText(link, on ? 'Hide merged and closed' : parts.concat('show').join(' · '));
  setAttr(link, 'href', historyHref(!on));
  setAttr(link, 'aria-expanded', String(on));
  link.hidden = list.length === 0;
}
// syncCountsText is the one counts split every surface shows: "3 running · 0
// need you · 38 merged", from /api/sync's bucket counts.
// The "need you" part is needsYou(), the server's flags, as everywhere else.
export function syncCountsText() {
  const c = syncCounts();
  if (!c) return '';
  const n = (k) => count(c[k]);
  return [wholeNumber.format(n('running')) + ' running', wholeNumber.format(needsYou().length) + ' need you', wholeNumber.format(n('merged')) + ' merged'].join(' · ');
}
// freshnessText is the age of the GitHub state a view shows, in words, and why
// it is not current; the reason is one of the server's fixed texts.
export function freshnessText(f) {
  if (!f || typeof f !== 'object') return 'GitHub state not loaded yet';
  const reason = typeof f.reason === 'string' && f.reason.length <= 300 ? f.reason : '';
  if (!f.checked) return 'GitHub state not checked' + (reason ? ' · ' + reason : '');
  const when = ago(f.fetchedAt);
  return 'GitHub state from ' + (when || 'an unknown time') + (f.stale ? ' · couldn’t check GitHub just now' + (reason ? ': ' + reason : '') : '');
}

// ---- Router: views and the selected Molecule in the URL ------------------------
// The view lives in the URL (?view=…), set through history.pushState by real
// <a href> links, so Back, reload and copied links restore it.
export const VIEWS = ['inbox', 'issues', 'work', 'insights', 'formula'];
export function viewFromURL() {
  const view = new URLSearchParams(location.search).get('view');
  // A bare ?m=<id> link opens the Molecule view under Work.
  if (!view && moleculeFromURL()) return 'work';
  return VIEWS.includes(view) ? view : 'inbox';
}
export function showView(view) {
  state.view = view;
  document.querySelectorAll('[data-view-panel]').forEach((panel) => { panel.hidden = panel.dataset.viewPanel !== view; });
  document.querySelectorAll('a[data-view]').forEach((link) => {
    if (link.dataset.view === view && !link.closest('main')) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  });
  const more = $('#tab-more');
  more.classList.toggle('is-current', view === 'insights' || view === 'formula');
  more.open = false;
  ctx.hideTip();
}
export function navigate(view, focus) {
  // Leaving a Molecule view, even for Work, is a new entry, so Back reopens it.
  if (moleculeFromURL()) history.pushState({ view: view }, '', '?view=' + view);
  else if (view !== viewFromURL()) history.pushState({ view: view }, '', '?view=' + view);
  showView(view);
  ctx.restoreSelection();
  if (focus) $('#main').focus({ preventScroll: true });
}

// A Molecule link (?view=work&m=…) is a real link: a plain click opens the
// Molecule under Work through history.pushState, so Back returns here.
export const MOLECULE_ID = /^[A-Za-z0-9_-]{1,200}$/;
export function moleculeHref(id) { return '?view=work&m=' + encodeURIComponent(id); }
export function moleculeFromURL() {
  const id = new URLSearchParams(location.search).get('m');
  return id && MOLECULE_ID.test(id) ? id : null;
}

// ---- Readable values and in-place patches -------------------------------------
// Counts read as words through Intl.PluralRules.
export const plural = new Intl.PluralRules('en-US');
export function needsYouText(n) { return n + (plural.select(n) === 'one' ? ' thing needs you' : ' things need you'); }
// setText changes a node's text only when it differs, so a poll never
// re-announces or disturbs what is already shown.
export function setText(node, text) { if (node.textContent !== text) node.textContent = text; }
export function setAttr(node, name, value) { if (node.getAttribute(name) !== value) node.setAttribute(name, value); }
export function plainText(value, fallback) { return typeof value === 'string' && value ? value : fallback; }
// count is a served count, or 0 when it was omitted as zero; callers show it
// only when its recorded flag (planned, verified, captured) is set.
export function count(n) { return Number.isInteger(n) && n >= 0 ? n : 0; }
export function counted(n, one, many) { return n + ' ' + (plural.select(n) === 'one' ? one : many); }

const timeOnly = new Intl.DateTimeFormat('en-US', { timeStyle: 'short' });
const dayOnly = new Intl.DateTimeFormat('en-US', { dateStyle: 'medium' });
export const wholeNumber = new Intl.NumberFormat('en-US');
// One money rule: two decimals, and an amount under a cent says so instead of
// rounding to $0.00.
const dollars = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 });
export function money(value) {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return '';
  return value > 0 && value < 0.01 ? 'under $0.01' : dollars.format(value);
}

// whenText is a recorded time in words: "today at 6:46 PM", "on Oct 24, 2026
// at 9:00 AM"; dateTitle gives the full date for a tooltip.
export function whenText(iso) {
  const t = Date.parse(iso || '');
  if (!Number.isFinite(t)) return '';
  const day = (ms) => new Date(ms).toDateString();
  const time = timeOnly.format(t);
  if (day(t) === day(Date.now())) return 'today at ' + time;
  if (day(t) === day(Date.now() - 86400000)) return 'yesterday at ' + time;
  return 'on ' + dayOnly.format(t) + ' at ' + time;
}

// dayText is a recorded time as a date only: "Oct 18, 2026".
export function dayText(iso) { const t = Date.parse(iso || ''); return Number.isFinite(t) ? dayOnly.format(t) : ''; }

const relativeTime = new Intl.RelativeTimeFormat('en-US', { numeric: 'auto' });
const fullDate = new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'short' });
// ago is a recorded time as relative words ("12 minutes ago"); dateTitle is
// the same time as a full local date, for a tooltip.
export function ago(iso) {
  const t = Date.parse(iso || '');
  if (!Number.isFinite(t)) return '';
  const s = Math.round((t - Date.now()) / 1000), abs = Math.abs(s);
  if (abs < 60) return 'just now';
  if (abs < 3600) return relativeTime.format(Math.round(s / 60), 'minute');
  if (abs < 86400) return relativeTime.format(Math.round(s / 3600), 'hour');
  return relativeTime.format(Math.round(s / 86400), 'day');
}
export function dateTitle(iso) { const t = Date.parse(iso || ''); return Number.isFinite(t) ? fullDate.format(t) : ''; }
export function minutesText(ms) {
  const min = Math.floor(ms / 60000);
  if (min < 1) return 'under a minute';
  if (min < 60) return min + ' min';
  return Math.floor(min / 60) + ' h' + (min % 60 ? ' ' + (min % 60) + ' min' : '');
}

// The seven steps of a Molecule, shown in the Inbox rows and the Molecule view.
export const STEPS = ['Plan', 'Your approval', 'Coding', 'Checks', 'Review', 'Draft PR', 'Merged'];
// RERUN_NOTES are the fixed rerun notes the server sends; nothing else shows.
const RERUN_NOTES = ['rerun', 'rerun after a plan revision'];
// withAttempt appends the current attempt's elapsed minutes, when its start is
// recorded, and its rerun note: "Checks · 10 min · rerun after a plan revision".
export function withAttempt(label, since, note) {
  const parts = [label];
  const t = Date.parse(since || '');
  if (Number.isFinite(t)) parts.push(minutesText(Math.max(0, Date.now() - t)));
  if (RERUN_NOTES.includes(note)) parts.push(note);
  return parts.join(' · ');
}

// ---- Dialog ---------------------------------------------------------------------
let dialogReturn = null;
export function show(title, body, note) {
  dialogReturn = document.activeElement;
  $('#dialog-title').textContent = title;
  const noteBox = $('#dialog-note');
  noteBox.textContent = note || '';
  noteBox.hidden = !note;
  $('#dialog-body').textContent = body;
  $('#detail').showModal();
}
export function mountDialog() {
  $('#close-dialog').addEventListener('click', () => $('#detail').close());
  $('#detail').addEventListener('close', () => { if (dialogReturn && dialogReturn.focus) dialogReturn.focus({ preventScroll: true }); });
}
