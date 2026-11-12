// ---- Molecule view: the Findings tab ---------------------------------------------
// Outside reviewers' findings on a published Molecule and its fix rounds, from
// GET /api/molecules/{id}/findings. The tab shows only when that view says the
// Formula declares reviewers. Every value it shows is untrusted text and goes
// in as a text node; links are https only. The view is read when the Molecule
// opens (to know whether the tab shows), when the tab opens, after each write
// and on the view's 30 s tick while the tab is visible: reading a verified fix
// round runs a git preview, so it is never read in a tight loop or while the
// page is hidden. A collection or a push started here is followed by the
// bounded poll below, since /job does not report these jobs.
import { $, el, uid, fill, state, ctx, PULL_URL, externalLink, api, failed, keepFocus, setText, plainText, count, counted, whenText, dateTitle, SessionError, syncOf } from './core.js';
import { postJSON, refusalText, reasonText } from './core-gates.js';
import { submitButton, field, fillKeyed } from './molecule-actions.js';

// After a 202 the panel reads the view every 2 s, only while the tab is
// visible, and for at most 5 minutes; then it says so and stops.
const FINDINGS_POLL_MS = 2000;
const FINDINGS_FOLLOW_MS = 5 * 60 * 1000;
// The view's own tick reads it at most once per 30 s.
const FINDINGS_TICK_MS = 30000;
const FINDINGS_FINGERPRINT = /^[0-9a-f]{64}$/;
const FINDINGS_COMMIT = /^[0-9a-f]{7,64}$/;
// Bounds on what one render shows; the server bounds them too.
const FINDINGS_MAX_ROWS = 500;
const FINDINGS_MAX_LIST = 50;
const FINDINGS_MAX_FILES = 200;
const FINDINGS_MAX_INCLUDES = 50;
const FINDINGS_MAX_REASON = 500;
const FINDINGS_BODY_SHORT = 280; // a longer or multi-line body starts collapsed
const FINDINGS_SEVERITIES = [['high', 'High'], ['medium', 'Medium'], ['low', 'Low'], ['info', 'Info']];
const FINDINGS_ROUND_STATUS = { complete: 'complete', pending: 'pending', error: 'failed' };
const FINDINGS_PENDING_TEXT = {
  'waiting-approval': 'Proposed. Its plan waits for your approval on the Plan tab.',
  running: 'Coding and its checks are running.',
  blocked: 'Blocked. The Plan and Timeline tabs say what it needs.',
  verified: 'Verified. Its push preview could not be read yet.',
  ready: 'Verified and ready to push.',
};

// fx is the panel's state, for the one open Molecule.
const fx = { id: null, token: 0, loading: false, view: null, readFailed: false, fetchedAt: 0, want: false, follow: null, pending: false, message: '', announce: false, note: '', proposal: null, checked: new Set(), dismissing: '', reasonError: '', confirming: '', ids: new Map(), chooseTab: null, onView: null, c: null };
const FINDINGS_READ_FAILED = 'could not read the findings; showing the last read';

// mountFindings takes the Molecule view's tab switch, so the panel can select
// the Findings or the Plan tab without importing the view, and a call run
// whenever the tab shows or hides (the roles row names the reviewers).
export function mountFindings(chooseTab, onView) { fx.chooseTab = chooseTab; fx.onView = onView || null; }
// findingsReviewers is Molecule id's declared reviewer plugins, or [].
export function findingsReviewers(id) { return (id && fx.id === id && fx.view && Array.isArray(fx.view.plugins) ? fx.view.plugins : []).filter((n) => typeof n === 'string' && n).slice(0, FINDINGS_MAX_LIST); }

// findingsSelect starts the panel for Molecule id, or closes it for null.
// want selects the Findings tab once the view says it shows, as for an Inbox
// fix-round or findings entry.
export function findingsSelect(id, want) {
  findingsStop();
  fx.token++;
  Object.assign(fx, { id: id, loading: false, view: null, readFailed: false, fetchedAt: 0, want: Boolean(id && want), pending: false, message: '', announce: false, note: '', proposal: null, dismissing: '', reasonError: '', confirming: '' });
  fx.checked = new Set();
  if (fx.c) { fx.c.reason.value = ''; }
  findingsShowTab(false);
  findingsRender();
  if (id) findingsLoad();
}

// findingsOpened runs when the Findings tab is selected: it reads the view
// unless a read is in flight or has just finished.
export function findingsOpened() {
  if (!fx.id) return;
  if (fx.loading || Date.now() - fx.fetchedAt < FINDINGS_POLL_MS) { findingsRender(); return; }
  findingsLoad();
}

// findingsTick is the view's tick: at most one read per 30 s, only while the
// tab is visible and no job is being followed.
export function findingsTick() {
  if (!fx.id || fx.follow || fx.pending || fx.loading || !findingsVisible()) return;
  if (Date.now() - fx.fetchedAt < FINDINGS_TICK_MS) return;
  findingsLoad();
}

function findingsVisible() {
  return !document.hidden && !$('#selected').hidden && !$('#mv-findings').hidden;
}

// findingsLoad reads the view once; a response for another Molecule or an
// older request is dropped. It reports whether this read was applied. Only an
// explicit reviewers false, a 404 or a 422 hides the tab; any other failed
// read keeps the last view, says so, and leaves a typed reason or an open
// confirmation as they are.
async function findingsLoad() {
  const id = fx.id;
  if (!id) return false;
  const token = ++fx.token;
  fx.loading = true;
  try {
    const res = await api('/api/molecules/' + encodeURIComponent(id) + '/findings');
    if (token !== fx.token) return false;
    const data = res.ok ? await res.json().catch(() => null) : null;
    if (token !== fx.token) return false;
    fx.fetchedAt = Date.now();
    const view = data && typeof data === 'object' && !Array.isArray(data) ? data : null;
    if (res.status === 404 || res.status === 422 || (view && view.reviewers === false)) {
      fx.view = null;
      fx.readFailed = false;
      findingsShowTab(false);
      findingsRender();
      return true;
    }
    if (!view || view.reviewers !== true) { findingsReadFailed(); return false; }
    fx.view = view;
    fx.readFailed = false;
    findingsShowTab(true);
    findingsRender();
    return true;
  } catch (error) {
    if (token !== fx.token) return false;
    if (error instanceof SessionError) failed(error);
    else { fx.fetchedAt = Date.now(); findingsReadFailed(); }
    return false;
  } finally {
    if (token === fx.token) fx.loading = false;
  }
}

function findingsReadFailed() {
  fx.readFailed = true;
  findingsRender();
}

// findingsShowTab shows or hides the third tab. Hiding it while selected
// returns to Plan; an Inbox entry's wish selects it once it shows.
function findingsShowTab(on) {
  const tab = $('#mv-tab-findings');
  if (tab.hidden !== !on) tab.hidden = !on;
  if (!on && tab.getAttribute('aria-selected') === 'true' && fx.chooseTab) fx.chooseTab('plan', false);
  if (on && fx.want && fx.chooseTab) { fx.want = false; fx.chooseTab('findings', false); }
  if (fx.onView) fx.onView();
}

// ---- Following a job ---------------------------------------------------------------
function findingsFollow(kind) {
  findingsStop();
  const f = { kind: kind, id: fx.id, started: Date.now(), timer: 0 };
  fx.follow = f;
  fx.note = kind === 'push' ? 'Pushing the fix round to the change request…' : 'Collecting findings…';
  findingsRender();
  f.timer = setTimeout(() => findingsFollowTick(f), FINDINGS_POLL_MS);
}

function findingsStop() {
  if (fx.follow) clearTimeout(fx.follow.timer);
  fx.follow = null;
}

// findingsFollowTick reads the view while the tab is visible and stops at the
// first terminal state of the followed job, or at the 5-minute bound.
async function findingsFollowTick(f) {
  if (fx.follow !== f || f.id !== fx.id) return;
  if (Date.now() - f.started >= FINDINGS_FOLLOW_MS) {
    fx.follow = null;
    fx.note = 'The ' + (f.kind === 'push' ? 'push' : 'collection') + ' is still running; refresh to check.';
    findingsRender();
    return;
  }
  const read = !document.hidden && !$('#mv-findings').hidden ? await findingsLoad() : false;
  if (fx.follow !== f) return;
  const v = fx.view || {}, job = f.kind === 'push' ? v.push : v.collect;
  // Only the followed job's own state decides; collecting covers other work.
  const running = Boolean(job && job.state === 'running');
  if (read && fx.view && !running) {
    fx.follow = null;
    fx.note = findingsJobText(f.kind, job);
    fx.announce = true;
    findingsRender();
    return;
  }
  f.timer = setTimeout(() => findingsFollowTick(f), FINDINGS_POLL_MS);
}

function findingsJobText(kind, job) {
  const done = job && job.state === 'finished';
  if (kind === 'push') {
    if (done) return 'Pushed to the change request. Nothing was merged or marked ready.';
    return 'The push did not finish' + findingsJobError(job);
  }
  if (done || !job) return 'Collection finished.';
  return 'The collection did not finish' + findingsJobError(job);
}
function findingsJobError(job) {
  const error = job && plainText(job.error, '');
  return (error ? ': ' + error : '') + '.' + reasonText(job);
}

// ---- Writes ------------------------------------------------------------------------
// findingsPost posts once. A refusal shows its fixed text; a lost or 5xx
// response and a stale fix round read the view back. Nothing is resubmitted.
async function findingsPost(action, body) {
  if (fx.pending || !fx.id) return null;
  const id = fx.id;
  fx.pending = true;
  fx.message = '';
  findingsRender();
  let res = null, data = {};
  try {
    res = await postJSON('/api/molecules/' + encodeURIComponent(id) + '/' + action, JSON.stringify(body));
    data = await res.json().catch(() => ({}));
  } catch (error) {
    if (error instanceof SessionError) { fx.pending = false; failed(error); findingsRender(); return null; }
    res = null;
  }
  fx.pending = false;
  if (fx.id !== id) return null;
  if (!res || res.status >= 500) {
    findingsSay('The response was lost or ambiguous. The findings are read back; nothing is resubmitted.');
    await findingsLoad();
    return null;
  }
  if (res.status === 409 && data && data.category === 'stale') {
    fx.confirming = '';
    findingsSay('The fix round changed, so nothing was pushed. Its new state is shown; review it again.');
    await findingsLoad();
    return null;
  }
  if (!res.ok) { findingsSay(refusalText(data)); return null; }
  return data && typeof data === 'object' ? data : {};
}

function findingsSay(text) {
  fx.message = text;
  fx.announce = true;
  findingsRender();
}

async function findingsCollect() {
  if (await findingsPost('findings/collect', {})) findingsFollow('collect');
}

async function findingsPropose() {
  const open = findingsOpenRows(fx.view);
  const include = [...fx.checked].filter((fp) => open.some((f) => f.fingerprint === fp && !f.autoFix));
  if (include.length > FINDINGS_MAX_INCLUDES) { findingsSay('Include at most ' + FINDINGS_MAX_INCLUDES + ' findings below the threshold in one fix round.'); return; }
  const id = fx.id, kept = new Set(fx.checked);
  const proposal = await findingsPost('findings/propose', { include: include });
  if (fx.id !== id) return;
  // A refused or lost proposal keeps every tick; only a proposal clears them.
  if (!proposal) { fx.checked = kept; findingsRender(); return; }
  fx.proposal = { body: proposal, open: open };
  fx.checked = new Set();
  await findingsLoad();
  ctx.loadDetail(id, false);
}

async function findingsDismiss(e) {
  e.preventDefault();
  const fp = fx.dismissing, reason = fx.c.reason.value.trim();
  if (!FINDINGS_FINGERPRINT.test(fp)) return;
  if (!reason || /[\r\n]/.test(reason) || [...reason].length > FINDINGS_MAX_REASON) {
    fx.reasonError = 'Enter a one-line reason of 1 to ' + FINDINGS_MAX_REASON + ' characters, such as why the finding does not apply.';
    findingsRender();
    fx.c.reason.focus({ preventScroll: true });
    fx.c.reasonError.scrollIntoView({ block: 'nearest' });
    return;
  }
  fx.reasonError = '';
  if (await findingsPost('findings/dismiss', { fingerprint: fp, reason: reason })) {
    fx.dismissing = '';
    fx.c.reason.value = '';
    fx.checked.delete(fp);
    await findingsLoad();
  }
}

async function findingsUndismiss(fp) {
  if (!FINDINGS_FINGERPRINT.test(fp)) return;
  if (await findingsPost('findings/undismiss', { fingerprint: fp })) await findingsLoad();
}

// findingsPush posts the digest of the preview the confirmation was opened
// for, with the one explicit confirmation given.
async function findingsPush(e) {
  e.preventDefault();
  const digest = fx.confirming;
  if (!FINDINGS_FINGERPRINT.test(digest)) return;
  if (await findingsPost('fixround/push', { approve: digest, confirm: true })) {
    fx.confirming = '';
    findingsFollow('push');
  }
}

function findingsOpenConfirm() {
  const p = findingsPreview(fx.view);
  if (!p) return;
  fx.confirming = p.digest;
  findingsRender();
  fx.c.pushSubmit.focus();
}
function findingsCloseConfirm() {
  fx.confirming = '';
  findingsRender();
  fx.c.pushOpen.focus({ preventScroll: true });
}

function findingsToggleDismiss(fp) {
  fx.dismissing = fx.dismissing === fp ? '' : fp;
  fx.reasonError = '';
  fx.c.reason.value = '';
  findingsRender();
  if (fx.dismissing) fx.c.reason.focus();
}

// ---- Rendering ---------------------------------------------------------------------
function findingsOpenRows(v) {
  const open = v && Array.isArray(v.open) ? v.open : [];
  return open.filter((f) => f && typeof f === 'object' && FINDINGS_FINGERPRINT.test(f.fingerprint)).slice(0, FINDINGS_MAX_ROWS);
}
function findingsPreview(v) {
  const p = v && v.pending && v.pending.preview;
  return p && typeof p === 'object' && FINDINGS_FINGERPRINT.test(p.digest) ? p : null;
}
function findingsBusy(v) { return fx.pending || Boolean(fx.follow) || Boolean(v && v.collecting === true); }

// findingsId is one uid() per finding control, kept across rebuilds so a
// label stays bound; the map stays bounded whatever the server sends.
function findingsId(key) {
  if (!fx.ids.has(key)) { if (fx.ids.size >= 2 * FINDINGS_MAX_ROWS) fx.ids.clear(); fx.ids.set(key, uid('mv-finding')); }
  return fx.ids.get(key);
}

function findingsSection(title, ...children) {
  const id = uid('mv-findings-title');
  return el('section', { class: 'glass mv-card mv-list-card', 'aria-labelledby': id }, el('h2', { class: 'mv-card-title', id: id }, title), ...children);
}

// findingsEnsure builds the panel's fixed controls once, so a read never
// rebuilds a reason being typed or a confirmation being read.
function findingsEnsure(panel) {
  if (fx.c && panel.contains(fx.c.root)) return fx.c;
  const c = {};
  c.message = el('p', { class: 'drive-error', role: 'alert', tabindex: '-1', hidden: true });
  c.note = el('p', { class: 'scope-note', role: 'status', tabindex: '-1' });
  c.stale = el('p', { class: 'drive-help', hidden: true }, FINDINGS_READ_FAILED + '.');
  c.unreadable = el('p', { class: 'drive-help', hidden: true });
  c.rounds = el('ul', { class: 'file-list' });
  c.collectHelp = el('p', { class: 'drive-help', id: uid('mv-collect-help') });
  c.collect = el('button', { type: 'button', class: 'btn btn-secondary', 'aria-describedby': c.collectHelp.id, dataset: { gate: 'findings-collect' }, onclick: findingsCollect }, 'Collect now');
  c.openCount = el('p', { class: 'drive-help' });
  c.open = el('div');
  c.proposeHelp = el('p', { class: 'drive-help', id: uid('mv-propose-help'), hidden: true });
  c.propose = el('button', { type: 'button', class: 'btn btn-secondary', 'aria-describedby': c.proposeHelp.id, dataset: { gate: 'findings-propose' }, onclick: findingsPropose }, 'Propose a fix round');
  c.proposal = el('div', { class: 'gate-status' });
  const reasonId = uid('mv-dismiss-reason');
  // The reason's validation error sits next to it and stays until fixed.
  c.reasonError = el('p', { class: 'drive-error', id: uid('mv-dismiss-error'), role: 'alert', hidden: true });
  c.reason = el('input', { type: 'text', id: reasonId, name: 'reason', maxlength: String(FINDINGS_MAX_REASON), required: true, autocomplete: 'off', placeholder: 'Not applicable to this change…', 'aria-describedby': c.reasonError.id, dataset: { gate: 'findings-reason' } });
  c.dismissForm = el('form', { class: 'confirm-form', id: uid('mv-dismiss-form'), novalidate: true, onsubmit: findingsDismiss },
    field(reasonId, 'Why dismiss it?', c.reason), c.reasonError,
    el('p', { class: 'drive-help' }, 'A dismissed finding is left out of the open findings and of fix-round proposals until you undismiss it.'),
    el('div', { class: 'confirm-actions' }, submitButton({ class: 'btn btn-secondary', dataset: { gate: 'findings-dismiss' } }, 'Dismiss finding'),
      el('button', { type: 'button', class: 'btn btn-secondary', dataset: { gate: 'findings-dismiss-cancel' }, onclick: () => findingsToggleDismiss(fx.dismissing) }, 'Cancel')));
  c.pendingBody = el('div');
  c.pushOpen = el('button', { type: 'button', class: 'btn btn-secondary', 'aria-expanded': 'false', dataset: { gate: 'findings-push-open' }, onclick: findingsOpenConfirm }, 'Push to the change request…');
  c.pushQuestion = el('p', { class: 'confirm-question', id: uid('mv-push-question') });
  c.pushGate = el('p', { class: 'drive-help' });
  c.pushSubmit = submitButton({ class: 'btn btn-primary', dataset: { gate: 'findings-push' } }, 'Push fix round');
  c.pushForm = el('form', { class: 'confirm-form', id: uid('mv-push-form'), 'aria-labelledby': c.pushQuestion.id, novalidate: true, hidden: true, onsubmit: findingsPush },
    c.pushQuestion,
    el('p', { class: 'drive-help' }, 'Forgecell pushes one commit to the same change request. Nothing is merged or marked ready.'),
    c.pushGate,
    el('div', { class: 'confirm-actions' }, c.pushSubmit, el('button', { type: 'button', class: 'btn btn-secondary', dataset: { gate: 'findings-push-cancel' }, onclick: findingsCloseConfirm }, 'Cancel')));
  c.pushOpen.setAttribute('aria-controls', c.pushForm.id);
  c.pendingSection = findingsSection('Pending fix round', c.pendingBody, c.pushOpen, c.pushForm);
  c.pushed = el('ul', { class: 'file-list' });
  c.pushedSection = findingsSection('Fix rounds pushed', c.pushed);
  c.dismissedSummary = el('summary');
  c.dismissed = el('ul', { class: 'file-list' });
  c.dismissedBlock = el('details', { class: 'mv-details' }, c.dismissedSummary, el('div', { class: 'mv-details-body' }, c.dismissed));
  c.root = el('div', { class: 'mv-sections' },
    c.message, c.note, c.stale, c.unreadable,
    findingsSection('Latest collection', c.rounds, c.collect, c.collectHelp),
    findingsSection('Open findings', c.openCount, c.open, c.propose, c.proposeHelp, c.proposal),
    c.pendingSection, c.pushedSection, c.dismissedBlock);
  fill(panel, c.root);
  fx.c = c;
  return c;
}

// findingsRender patches the panel from the view held; a Molecule without
// reviewers has an empty, hidden panel.
function findingsRender() {
  const panel = $('#mv-findings'), v = fx.view;
  if (!fx.id || !v || v.reviewers !== true) { if (panel.firstChild) fill(panel); fx.c = null; return; }
  keepFocus(() => {
    const c = findingsEnsure(panel);
    setText(c.note, fx.note);
    c.note.hidden = !fx.note;
    c.stale.hidden = !fx.readFailed;
    setText(c.reasonError, fx.reasonError);
    c.reasonError.hidden = !fx.reasonError;
    if (fx.reasonError) c.reason.setAttribute('aria-invalid', 'true'); else c.reason.removeAttribute('aria-invalid');
    const unreadable = v.unreadable === true ? plainText(v.error, 'The findings record could not be read safely.') : '';
    setText(c.unreadable, unreadable);
    c.unreadable.hidden = !unreadable;
    findingsRenderCollect(c, v);
    findingsRenderOpen(c, v);
    findingsRenderProposal(c);
    findingsRenderPending(c, v);
    findingsRenderPushed(c, v);
    findingsRenderDismissed(c, v);
    setText(c.message, fx.message);
    c.message.hidden = !fx.message;
  });
  if (fx.announce && fx.c && !$('#mv-findings').hidden) { fx.announce = false; findingsAnnounce(fx.message ? fx.c.message : fx.c.note); }
}

// findingsAnnounce moves focus to an announced element and scrolls it into
// view only as far as needed.
function findingsAnnounce(node) {
  node.focus({ preventScroll: true });
  node.scrollIntoView({ block: 'nearest' });
}

function findingsRenderCollect(c, v) {
  const rounds = Array.isArray(v.rounds) ? v.rounds.filter((r) => r && typeof r === 'object').slice(0, FINDINGS_MAX_LIST) : [];
  fillKeyed(c.rounds, JSON.stringify(rounds), () => rounds.length ? rounds.map(findingsRoundItem) : el('li', { class: 'drive-help' }, 'No findings collected yet.'));
  const busy = findingsBusy(v);
  // collecting covers any findings work; only the collect job, or the one
  // being followed, is a collection running.
  const collecting = Boolean(v.collect && v.collect.state === 'running') || Boolean(fx.follow && fx.follow.kind === 'collect');
  const why = !v.published ? 'Findings follow a published draft; publish the delivery first.'
    : collecting ? 'A collection is running for this Molecule.'
      : busy ? 'Another findings action is running for this Molecule.'
        : 'Reads every reviewer once for the change request’s current head.';
  setText(c.collectHelp, why);
  c.collect.disabled = busy || !v.published;
  setText(c.collect, collecting ? 'Collecting…' : 'Collect now');
}

// findingsName is a reviewer's plugin name, never its key ("0:forgecomments").
function findingsName(r) { return plainText(r.plugin, '') || plainText(r.reviewer, 'Reviewer').replace(/^\d+:/, ''); }
// findingsRoundItem is a reviewer's latest round; a failure shows only itself and its reason.
function findingsRoundItem(r) {
  const status = FINDINGS_ROUND_STATUS[r.status] || 'pending', when = whenText(r.collectedAt), error = plainText(r.error, '');
  const line = status === 'failed' ? 'Failed: ' + (error || 'no reason was recorded') + '.' : [when ? 'collected ' + when : 'not collected', r.complete === true ? 'round complete' : 'round incomplete'].join(' · ');
  return el('li', { class: 'file' },
    el('span', { class: 'file-badge' + (status === 'complete' ? ' is-new' : '') }, status),
    el('div', { class: 'file-text' },
      el('code', null, findingsName(r)),
      el('p', { class: 'file-reason', title: dateTitle(r.collectedAt) }, line),
      status !== 'failed' && error ? el('p', { class: 'file-reason' }, error) : null));
}

function findingsRenderOpen(c, v) {
  const open = findingsOpenRows(v), busy = findingsBusy(v);
  const total = count(v.openCount);
  setText(c.openCount, total === 0 ? 'No open findings on the latest complete round.' : v.truncated === true ? 'Showing ' + open.length + ' of ' + counted(total, 'open finding', 'open findings') + '.' : counted(total, 'open finding', 'open findings') + '. Findings marked auto-fix go into the next fix round; tick a finding below the threshold to include it.');
  // A rebuild moves the reason field into a new row, which drops its focus.
  const typing = document.activeElement === c.reason;
  fillKeyed(c.open, JSON.stringify([open, fx.dismissing, busy, v.published === true]), () => {
    const groups = [];
    const known = FINDINGS_SEVERITIES.map(([s]) => s);
    for (const [severity, label] of FINDINGS_SEVERITIES.concat([['', 'Other']])) {
      const rows = open.filter((f) => severity ? f.severity === severity : !known.includes(f.severity));
      if (!rows.length) continue;
      groups.push(el('h3', { class: 'check-name' }, label + ' · ' + rows.length), el('ul', { class: 'file-list' }, rows.map((f) => findingsOpenItem(f, busy || v.published !== true))));
    }
    return groups;
  });
  if (typing && c.reason.isConnected && document.activeElement !== c.reason) c.reason.focus({ preventScroll: true });
  for (const box of c.open.querySelectorAll('input[type=checkbox]')) box.checked = fx.checked.has(box.dataset.fingerprint);
  // Propose shows only with open findings; a merged or closed change request
  // (on its receipt or the pull request GitHub sync read) disables it and says why.
  const s = syncOf(fx.id), pr = s && s.pr, ended = v.lifecycle === 'merged' || (pr && pr.state === 'MERGED') ? 'merged' : v.lifecycle === 'closed' || (pr && pr.state !== 'OPEN') ? 'closed' : '';
  c.propose.hidden = open.length === 0;
  c.propose.disabled = busy || v.published !== true || open.length === 0 || Boolean(ended);
  setText(c.proposeHelp, ended ? 'The change request is ' + ended + ', so there is nothing left to propose a fix round for.' : '');
  c.proposeHelp.hidden = !ended || open.length === 0;
}

// findingsBodyBlock is a finding's body as text, collapsed when long or multi-line.
function findingsBodyBlock(f) {
  const body = typeof f.body === 'string' ? f.body.trim() : '', text = body && el('p', { class: 'mv-finding-body' }, body + (f.bodyTruncated === true ? ' …' : ''));
  if (!body || (body.length <= FINDINGS_BODY_SHORT && !body.includes('\n'))) return text || null;
  return el('details', { class: 'mv-details' }, el('summary', null, 'The reviewer’s explanation'), el('div', { class: 'mv-details-body' }, text));
}

function findingsOpenItem(f, locked) {
  const fp = f.fingerprint, where = plainText(f.path, '') ? f.path + (Number.isInteger(f.line) && f.line > 0 ? ':' + f.line : '') : 'No file';
  const marks = [];
  if (f.repeated === true) marks.push(el('span', { class: 'chip' }, 'came back after a fix'));
  if (f.autoFix === true) marks.push(el('span', { class: 'chip' }, 'auto-fix'));
  let include = null;
  if (f.autoFix !== true) {
    const id = findingsId('include:' + fp);
    const box = el('input', { type: 'checkbox', id: id, disabled: locked, dataset: { gate: 'findings-include-' + fp, fingerprint: fp }, onchange: (e) => { if (e.target.checked) fx.checked.add(fp); else fx.checked.delete(fp); } });
    include = el('label', { class: 'accept-label', for: id }, box, el('span', null, 'Include in the fix round'));
  }
  const open = fx.dismissing === fp;
  const dismiss = el('button', { type: 'button', class: 'text-button', 'aria-expanded': String(open), 'aria-controls': fx.c.dismissForm.id, disabled: locked, dataset: { gate: 'findings-dismiss-open-' + fp }, onclick: () => findingsToggleDismiss(fp) }, 'Dismiss…');
  return el('li', { class: 'file' },
    el('span', { class: 'file-badge' }, plainText(f.severity, 'info')),
    el('div', { class: 'file-text' },
      el('code', null, where),
      el('p', { class: 'file-reason' }, plainText(f.title, '(untitled)')),
      findingsBodyBlock(f),
      el('p', { class: 'file-reason' }, findingsName(f)),
      marks.length ? el('p', null, marks) : null,
      include, dismiss, open ? fx.c.dismissForm : null));
}

// findingsRenderProposal shows the last proposal in plain words, with the
// way to its scope approval on the Plan tab.
function findingsRenderProposal(c) {
  const p = fx.proposal;
  fillKeyed(c.proposal, p ? JSON.stringify(p.body) : '', () => {
    const b = p.body, picked = Array.isArray(b.picked) ? b.picked.slice(0, FINDINGS_MAX_ROWS) : [], skipped = Array.isArray(b.skipped) ? b.skipped.slice(0, FINDINGS_MAX_ROWS) : [];
    if (b.none === true) return el('p', { class: 'scope-note' }, 'No finding was picked, so no fix round was proposed and the plan is unchanged.' + (skipped.length ? ' ' + counted(skipped.length, 'finding was', 'findings were') + ' skipped.' : ''));
    const title = (fp) => { const f = p.open.find((x) => x.fingerprint === fp); return f ? plainText(f.title, '(untitled)') : 'A finding'; };
    return el('div', { class: 'gate-job' },
      el('strong', null, 'Fix round ' + count(b.round) + ' proposed with ' + counted(picked.length, 'finding', 'findings')),
      el('ul', { class: 'acceptance-list' }, picked.map((f) => el('li', null, plainText(f.severity, 'info') + ' · ' + plainText(f.title, '(untitled)')))),
      skipped.length ? el('p', { class: 'scope-note' }, 'Skipped: ' + skipped.map((s) => title(s.fingerprint) + ' (' + plainText(s.reason, 'skipped') + ')').join('; ') + '.') : null,
      el('p', { class: 'scope-note' }, 'Nothing is approved yet: its plan waits for your approval.'),
      el('button', { type: 'button', class: 'text-button', dataset: { gate: 'findings-to-plan' }, onclick: () => { if (fx.chooseTab) fx.chooseTab('plan', true); } }, 'Review the fix round on the Plan tab'));
  });
}

// findingsGateText is the push gate in plain words; no digest is shown.
function findingsGateText(p) {
  if (p.needsConfirm !== true) return 'Forgecell’s review passed on this exact change.';
  const why = [];
  if (p.acceptanceUnverified === true) why.push('no independent check verified its acceptance');
  if (p.reviewGreen !== true) why.push('Forgecell’s review is not green on this exact change');
  return 'This push needs your confirmation because ' + (why.length ? why.join(' and ') : 'its push gate asks for it') + '.';
}

function findingsRenderPending(c, v) {
  const pending = v.pending && typeof v.pending === 'object' ? v.pending : null, p = findingsPreview(v);
  c.pendingSection.hidden = !pending;
  if (fx.confirming && (!p || p.digest !== fx.confirming)) fx.confirming = '';
  const busy = findingsBusy(v);
  fillKeyed(c.pendingBody, pending ? JSON.stringify([pending.round, pending.state, pending.findings, pending.previewError, p && [p.digest, p.round, p.files, p.findings, p.needsConfirm, p.acceptanceUnverified, p.reviewGreen, p.title]]) : '', () => {
    const files = p && Array.isArray(p.files) ? p.files.filter((f) => typeof f === 'string') : [];
    return [
      el('p', { class: 'mv-summary' }, 'Round ' + count(pending.round) + ' · ' + counted(count(pending.findings), 'finding', 'findings') + ' · ' + (FINDINGS_PENDING_TEXT[pending.state] || 'State not recorded.')),
      pending.previewError ? el('p', { class: 'drive-help' }, plainText(pending.previewError, '')) : null,
      p ? el('p', { class: 'file-reason' }, plainText(p.title, '')) : null,
      p ? el('ul', { class: 'file-list' }, files.slice(0, FINDINGS_MAX_FILES).map((f) => el('li', null, el('code', null, f)))) : null,
      files.length > FINDINGS_MAX_FILES ? el('p', { class: 'mv-more' }, counted(files.length - FINDINGS_MAX_FILES, 'more file', 'more files') + '.') : null,
      p ? el('p', { class: 'scope-note' }, findingsGateText(p)) : null,
    ];
  });
  c.pushOpen.hidden = !p || Boolean(fx.confirming);
  c.pushOpen.disabled = busy;
  c.pushOpen.setAttribute('aria-expanded', String(Boolean(fx.confirming)));
  c.pushForm.hidden = !fx.confirming;
  c.pushSubmit.disabled = busy;
  setText(c.pushSubmit, fx.pending && fx.confirming ? 'Pushing…' : 'Push fix round');
  setText(c.pushQuestion, p ? 'Push fix round ' + count(p.round) + ' to the change request?' : '');
  setText(c.pushGate, p ? findingsGateText(p) : '');
}

// findingsPullURL is the Molecule detail's recorded pull request, or '' when
// none is recorded or it is not an https GitHub pull request URL.
function findingsPullURL() {
  const d = state.detail, dec = d && d.id === fx.id && d.view && d.view.decision;
  return dec && typeof dec.pullUrl === 'string' && PULL_URL.test(dec.pullUrl) ? dec.pullUrl : '';
}

function findingsRenderPushed(c, v) {
  const rounds = Array.isArray(v.fixRounds) ? v.fixRounds.filter((r) => r && typeof r === 'object').slice(0, FINDINGS_MAX_LIST) : [];
  const pull = findingsPullURL();
  c.pushedSection.hidden = !rounds.length;
  fillKeyed(c.pushed, rounds.length ? JSON.stringify([rounds, pull]) : '', () => rounds.map((r) => {
    const when = whenText(r.pushedAt);
    const commit = typeof r.commit === 'string' && FINDINGS_COMMIT.test(r.commit) ? r.commit.slice(0, 7) : '';
    return el('li', { class: 'file' },
      el('span', { class: 'file-badge is-new' }, 'pushed'),
      el('div', { class: 'file-text' },
        el('p', { title: dateTitle(r.pushedAt) }, ['Round ' + count(r.round), commit ? 'commit ' + commit : '', when].filter(Boolean).join(' · ')),
        pull ? externalLink(pull, PULL_URL, 'Open the change request ↗') : null));
  }));
}

function findingsRenderDismissed(c, v) {
  const list = Array.isArray(v.dismissals) ? v.dismissals.filter((d) => d && typeof d === 'object' && FINDINGS_FINGERPRINT.test(d.fingerprint)).slice(0, FINDINGS_MAX_ROWS) : [];
  const busy = findingsBusy(v);
  c.dismissedBlock.hidden = !list.length;
  if (!list.length) c.dismissedBlock.open = false;
  setText(c.dismissedSummary, 'Dismissed · ' + list.length);
  fillKeyed(c.dismissed, list.length ? JSON.stringify([list, busy]) : '', () => list.map((d) => {
    const when = whenText(d.at);
    return el('li', { class: 'file' },
      el('span', { class: 'file-badge is-unknown' }, 'dismissed'),
      el('div', { class: 'file-text' },
        el('p', { title: dateTitle(d.at) }, [findingsName(d), when ? 'dismissed ' + when : ''].filter(Boolean).join(' · ')),
        el('p', { class: 'file-reason' }, 'Reason: ' + plainText(d.reason, 'Not recorded')),
        el('button', { type: 'button', class: 'btn btn-secondary btn-small', disabled: busy, dataset: { gate: 'findings-undismiss-' + d.fingerprint }, onclick: () => findingsUndismiss(d.fingerprint) }, 'Undismiss')));
  }));
}
