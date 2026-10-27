// ---- Formula: the active Formula and its reviewed proposals ---------------------
// The Formula new Molecules use, and the one reviewed place to change it: create
// a proposal through the engine's own init functions, review its readable diff,
// and approve or dismiss the exact proposal shown. Existing Molecules keep their
// saved snapshot, shown in each Molecule's Timeline. Every value from the API is
// untrusted and reaches the page as text only. Raw YAML, ids and full digests
// appear only under Details.
import { $, el, fill, add, state, tabSession, SessionError, failed, notRecorded, setText, plural, counted, whenText, ago, dateTitle } from './core.js';

const STATES = {
  approved: 'Approved for execution',
  none: 'No active Formula',
  'activation-locked': 'Activation in progress or interrupted; execution refuses it until resumed',
  unapproved: 'Not approved for execution: missing, stale or unfinished decision',
  unreadable: 'Formula unreadable',
  sample: 'Sample data · not a real Formula',
};

// The proposal kinds this view creates, with their Title Case buttons.
const KINDS = {
  rebind: { button: 'Rebind to This Build', help: 'Points the coding agent at the Forgecell build this Lab UI runs.' },
  baseline: { button: 'Refresh Baseline from CI', help: 'Re-derives the required checks from your pull-request workflows.' },
  'enable-review': { button: 'Enable In-Loop Review', help: 'Adds a review Atom after Checks; runs still stop for your review.' },
  model: { button: 'Propose Model Pin', help: 'Pins the model the agents run.' },
};
const ROLES = [['', 'Default agent and every declared role'], ['analysis', 'Planning agent'], ['coding', 'Coding agent'], ['meta', 'Learning agent'], ['review', 'Review agent']];
const CHANGE_WORDS = { added: 'Added', removed: 'Removed', changed: 'Changed' };

// view is the last GET /api/formula; pending is true while a write is in flight.
const fv = { view: null, pending: false, token: 0, fetchedAt: 0, sigs: {}, live: '' };

function text(value, fallback) { return typeof value === 'string' && value ? value : fallback; }
function list(value) { return Array.isArray(value) ? value : []; }
function shortId(s) { return typeof s === 'string' && s.length > 12 ? s.slice(0, 12) + '…' : notRecorded(s); }

function announce(message) {
  if (fv.live === message) return;
  fv.live = message;
  setText($('#formula-live'), message);
}

// showError puts a refusal inline, linked from the control, and moves focus to it.
function showError(message) {
  const box = $('#formula-error');
  setText(box, message);
  box.hidden = !message;
  if (message) box.focus({ preventScroll: false });
}

// copyButton copies a full value shown only under Details.
function copyButton(value, label) {
  return el('button', { type: 'button', class: 'btn btn-ghost formula-copy', onclick: (e) => {
    const button = e.currentTarget;
    try {
      navigator.clipboard.writeText(String(value)).then(() => setText(button, 'Copied'), () => setText(button, 'Copy failed'));
    } catch (error) { setText(button, 'Copy failed'); }
  } }, label);
}

// ---- API ------------------------------------------------------------------------
async function formulaPost(path, body) {
  const session = tabSession();
  if (!session || state.sessionLost) throw new SessionError('no session');
  const res = await fetch(path, { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'X-Forgecell-Session': session, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  if (res.status === 403) throw new SessionError('forbidden');
  return res;
}

async function load() {
  const token = ++fv.token;
  fv.fetchedAt = Date.now();
  try {
    const session = tabSession();
    if (!session || state.sessionLost) return;
    const res = await fetch('/api/formula', { credentials: 'same-origin', cache: 'no-store', headers: { 'X-Forgecell-Session': session } });
    if (res.status === 403) throw new SessionError('forbidden');
    if (!res.ok) return;
    const view = await res.json();
    if (token !== fv.token || fv.pending) return;
    fv.view = view;
    render();
  } catch (error) { failed(error); }
}

// submit sends one write; the button stays enabled until the request starts,
// then shows progress. A refusal is shown inline with its reason; nothing is
// ever resubmitted.
async function submit(button, path, body, progress) {
  if (fv.pending) return;
  fv.pending = true;
  const label = button.textContent;
  button.disabled = true;
  button.setAttribute('aria-busy', 'true');
  setText(button, progress);
  showError('');
  try {
    const res = await formulaPost(path, body);
    let data = null;
    try { data = await res.json(); } catch (error) { data = null; }
    if (res.ok && data) {
      fv.view = data;
      announce({ proposed: 'Proposal saved; review it below', approved: 'Proposal approved; new Molecules use the new Formula', dismissed: 'Proposal dismissed' }[data.outcome] || 'Formula updated');
    } else {
      const reason = data && typeof data.reason === 'string' && data.reason ? ' ' + data.reason + '.' : '';
      showError('Refused: ' + notRecorded(data && data.error) + '.' + reason);
    }
  } catch (error) {
    if (error instanceof SessionError) failed(error);
    else showError('The request did not complete. The page reads the Formula back and never resubmits.');
  } finally {
    fv.pending = false;
    button.disabled = false;
    button.removeAttribute('aria-busy');
    setText(button, label);
    fv.sigs = {};
    render();
    load();
  }
}

// ---- Active Formula card -----------------------------------------------------------
function renderFormula(lab) {
  const a = (fv.view && fv.view.active) || {};
  const f = (lab && lab.formula) || {};
  const box = $('#active-formula');
  const sig = JSON.stringify([a, f.state, f.formulaId, f.sha256]);
  if (fv.sigs.active === sig) return;
  if (box.contains(document.activeElement)) return;
  fv.sigs.active = sig;
  const stateText = STATES[a.state || f.state] || notRecorded(a.state || f.state);
  const name = text(a.name, text(f.formulaId, 'None'));
  const shortSha = text(a.shortSha, typeof f.sha256 === 'string' ? f.sha256.slice(0, 12) : '');
  const rows = [];
  if (list(a.atoms).length) rows.push(el('li', null, el('span', { class: 'formula-key' }, 'Atoms'), ' ', list(a.atoms).map(String).join(' → ')));
  if (a.coding && a.coding.agent) rows.push(el('li', null, el('span', { class: 'formula-key' }, 'Coding agent'), ' ', a.coding.agent + (a.coding.model ? ' · model ' + a.coding.model : '')));
  if (a.reviewer && a.reviewer.agent) {
    rows.push(el('li', null, el('span', { class: 'formula-key' }, 'Reviewer'), ' ', a.reviewer.agent + (a.reviewer.model ? ' · model ' + a.reviewer.model : '') + (a.reviewer.source ? ' (from ' + a.reviewer.source + ')' : ''),
      a.reviewer.warning ? el('span', { class: 'formula-warning' }, ' ' + a.reviewer.warning) : null));
  } else if (a.state === 'approved') {
    rows.push(el('li', null, el('span', { class: 'formula-key' }, 'Reviewer'), ' No in-loop review'));
  }
  const checks = list(a.checks);
  if (a.state === 'approved') {
    rows.push(el('li', null, el('span', { class: 'formula-key' }, 'Required checks'), ' ',
      checks.length ? counted(checks.length, 'check', 'checks') + ': ' + checks.map((c) => text(c && c.name, 'Not recorded')).join(' · ') : 'None'));
    const approved = a.approvedAt ? 'Approved ' + whenText(a.approvedAt) + ' (' + ago(a.approvedAt) + ')' : 'Approval time not recorded';
    rows.push(el('li', null, el('span', { class: 'formula-key' }, 'Approval'), ' ', el('span', { title: dateTitle(a.approvedAt) }, approved), ' · by ' + text(a.approvedBy, 'Not recorded')));
  }
  const details = a.details && a.details.sha256 ? el('details', { class: 'formula-details' }, el('summary', null, 'Details'),
    el('p', { class: 'mono-text' }, 'Formula ' + text(a.details.formulaId, 'Not recorded')),
    el('p', { class: 'mono-text' }, 'sha256 ' + a.details.sha256, ' ', copyButton(a.details.sha256, 'Copy Digest'))) : null;
  fill(box,
    el('div', null,
      el('span', { class: 'label', text: 'NEW MOLECULES · ACTIVE FORMULA' }),
      el('strong', null, name, shortSha ? el('span', { class: 'version-tag' }, 'sha256 ' + shortSha + '…') : null),
      el('small', null, stateText + '. Applies to future Molecules; existing Molecules keep their saved snapshot.'),
      rows.length ? el('ul', { class: 'formula-facts' }, rows) : null,
      details),
    el('div', { class: 'formula-actions' }, el('span', { class: 'unavailable-tag', title: 'Version history and rollback are not connected in this slice' }, 'Versions & history · Unavailable')));
}

// ---- Pending proposal: readable diff, notes and the decision --------------------
function renderPending() {
  const v = fv.view || {};
  const p = v.pending;
  const box = $('#formula-pending');
  const sig = JSON.stringify([p, v.inFlight]);
  if (fv.sigs.pending === sig) return;
  // Never rebuild under the user: a focused control keeps its block until it
  // loses focus, unless the proposal itself changed.
  const sameProposal = p && fv.sigs.pendingId === (p.details && p.details.id);
  if (sameProposal && box.contains(document.activeElement)) return;
  fv.sigs.pending = sig;
  fv.sigs.pendingId = p && p.details ? p.details.id : null;
  if (!p) {
    fill(box, el('p', { class: 'formula-empty' }, v.sample ? 'Sample data has no proposals.' : 'No proposal is waiting for your review.'));
    return;
  }
  const changes = list(p.changes);
  const rows = changes.map((c) => el('li', { class: 'formula-change' },
    el('span', { class: 'formula-change-label' }, text(c.label, 'A Formula key')),
    el('span', { class: 'formula-change-kind' }, CHANGE_WORDS[c.change] || 'Changed'),
    el('span', { class: 'formula-change-values' }, el('span', { class: 'formula-old' }, text(c.old, 'none')), ' → ', el('span', { class: 'formula-new' }, text(c.new, 'none')))));
  const inFlight = (v.inFlight && v.inFlight.running) || 0;
  const notes = list(p.notes).map((n) => el('p', null, String(n)));
  if (p.warning) notes.push(el('p', { class: 'formula-warning', role: 'note' }, String(p.warning)));
  if (inFlight > 0) notes.push(el('p', { class: 'formula-inflight' }, inFlightText(inFlight)));
  const waiting = (v.inFlight && v.inFlight.waiting) || 0;
  if (p.rebase && waiting > 0) notes.push(el('p', { class: 'formula-inflight' }, 'This is a launcher-only rebind. After you approve, ' + counted(waiting, 'Molecule waiting', 'Molecules waiting') + ' for plan approval can carry ' + (plural.select(waiting) === 'one' ? 'its' : 'their') + ' plan to it with forgecell run --rebase-formula, where the engine allows it.'));
  const details = p.details || {};
  const lines = list(details.lines).map((line) => {
    const op = line && typeof line.op === 'string' ? line.op : ' ';
    const cls = op === '+' ? 'diff-add' : op === '-' ? 'diff-del' : op === '…' ? 'diff-gap' : 'diff-same';
    return el('span', { class: cls }, (op === '…' ? '…' : op + ' ' + text(line.text, '')) + '\n');
  });
  const digest = text(details.digest, '');
  const id = text(details.id, '');
  const approve = el('button', { type: 'button', class: 'btn btn-primary', 'aria-describedby': 'formula-error', dataset: { drive: 'formula-approve' }, onclick: (e) => submit(e.currentTarget, '/api/formula/proposals/' + encodeURIComponent(id) + '/approve', { digest: digest }, 'Approving…') }, p.interrupted ? 'Resume Approval' : 'Approve Proposal');
  const dismiss = p.interrupted ? null : el('button', { type: 'button', class: 'btn btn-secondary', 'aria-describedby': 'formula-error', dataset: { drive: 'formula-dismiss' }, onclick: (e) => submit(e.currentTarget, '/api/formula/proposals/' + encodeURIComponent(id) + '/dismiss', { digest: digest }, 'Dismissing…') }, 'Dismiss');
  fill(box, el('article', { class: 'formula-proposal card' },
    el('h3', null, text(p.title, 'Formula proposal') + ' · ready for your approval'),
    p.createdAt ? el('small', { title: dateTitle(p.createdAt) }, 'Proposed ' + whenText(p.createdAt) + ' (' + ago(p.createdAt) + ')') : null,
    el('ul', { class: 'formula-changes', 'aria-label': 'Changed keys' }, rows.length ? rows : el('li', null, 'No key changes; see Details.')),
    p.truncated ? el('p', { class: 'formula-warning' }, 'This change is larger than the view shows; review the exact YAML under Details before approving.') : null,
    el('div', { class: 'formula-notes' }, notes),
    el('details', { class: 'formula-details' }, el('summary', null, 'Details'),
      el('p', { class: 'mono-text' }, 'Proposal ' + shortId(id), ' ', copyButton(id, 'Copy Id')),
      el('p', { class: 'mono-text' }, 'Digest ' + shortId(digest), ' ', copyButton(digest, 'Copy Digest')),
      list(details.keys).length ? el('ul', { class: 'formula-keys mono-text' }, list(details.keys).map((k) => el('li', null, text(k.key, '?') + ': ' + text(k.old, 'absent') + ' → ' + text(k.new, 'absent')))) : null,
      el('pre', { class: 'formula-diff', 'aria-label': 'Exact YAML diff' }, lines),
      details.truncated ? el('p', null, 'The YAML diff was cut at its bound; inspect the proposal with forgecell init --review.') : null),
    el('div', { class: 'formula-decide' }, approve, dismiss)));
}

function inFlightText(n) {
  return counted(n, 'Molecule is', 'Molecules are') + ' in flight. Approving changes future Molecules only; in-flight Molecules keep their snapshot.';
}

// ---- Create: one row per proposal kind, patched in place ----------------------------
function optionRow(kind) {
  const row = el('li', { class: 'formula-option', dataset: { kind: kind } });
  if (kind === 'model') {
    const input = el('input', { type: 'text', id: 'formula-model', name: 'model', maxlength: '200', autocomplete: 'off', spellcheck: 'false', placeholder: 'claude-opus-4-8…', 'aria-describedby': 'formula-model-help formula-error', required: true });
    const role = el('select', { id: 'formula-role', name: 'role', 'aria-describedby': 'formula-model-help' }, ROLES.map(([value, label]) => el('option', { value: value }, label)));
    // A button in a form submits by default, so Enter in the input proposes.
    const button = el('button', { class: 'btn btn-secondary', dataset: { drive: 'formula-model' } }, KINDS.model.button);
    const form = el('form', { class: 'formula-model-form', novalidate: true, onsubmit: (e) => {
      e.preventDefault();
      const model = input.value.trim();
      if (!model) { showError('Give a model name, for example claude-opus-4-8.'); input.focus(); return; }
      submit(button, '/api/formula/proposals', { kind: 'model', digest: digestShown(), model: model, role: role.value }, 'Proposing…');
    } },
      el('label', { for: 'formula-model' }, 'Pin a Model'), input,
      el('label', { for: 'formula-role' }, 'For'), role, button);
    add(row, form, el('small', { id: 'formula-model-help', class: 'formula-help' }, KINDS.model.help));
    return row;
  }
  const button = el('button', { type: 'button', class: 'btn btn-secondary', 'aria-describedby': 'formula-error', dataset: { drive: 'formula-' + kind }, onclick: (e) => {
    const o = optionOf(kind);
    submit(e.currentTarget, '/api/formula/proposals', { kind: kind, digest: digestShown(), anyway: Boolean(o && o.needsAnyway) }, 'Proposing…');
  } }, KINDS[kind].button);
  add(row, button, el('small', { class: 'formula-help' }), el('small', { class: 'formula-inflight', hidden: true }));
  return row;
}

function optionOf(kind) { return list(fv.view && fv.view.create).find((o) => o && o.kind === kind) || null; }
function digestShown() { const a = fv.view && fv.view.active; return (a && a.details && a.details.sha256) || ''; }

function renderCreate() {
  const v = fv.view || {};
  const box = $('#formula-create');
  let ul = box.querySelector('ul.formula-options');
  if (!ul) {
    ul = el('ul', { class: 'formula-options' });
    fill(box, el('h3', null, 'Propose a Change'), el('p', { id: 'formula-create-reason', class: 'formula-help' }), ul);
  }
  setText($('#formula-create-reason'), v.reason || (list(v.create).length ? 'Each change is saved as a proposal; nothing applies until you approve it.' : ''));
  const kinds = list(v.create).map((o) => o && o.kind).filter((k) => KINDS[k]);
  // Rows are keyed by kind: an existing row, and any input in it, is kept.
  for (const row of [...ul.children]) if (!kinds.includes(row.dataset.kind)) row.remove();
  kinds.forEach((kind, i) => {
    let row = ul.querySelector('[data-kind="' + kind + '"]');
    if (!row) row = optionRow(kind);
    if (ul.children[i] !== row) ul.insertBefore(row, ul.children[i] || null);
    const o = optionOf(kind);
    const button = row.querySelector('button');
    if (!fv.pending) button.disabled = !o.available;
    if (kind !== 'model') {
      let label = KINDS[kind].button;
      if (kind === 'rebind' && o.needsAnyway) label = 'Rebind Anyway';
      if (!button.hasAttribute('aria-busy')) setText(button, label);
      const help = row.querySelector('.formula-help');
      const why = o.available && o.reason ? o.reason : KINDS[kind].help + (kind === 'rebind' && o.build ? ' This build: ' + o.build + '.' : '');
      setText(help, o.available ? why : KINDS[kind].help + ' ' + text(o.reason, ''));
      const note = row.querySelector('.formula-inflight');
      const running = (v.inFlight && v.inFlight.running) || 0;
      const show = (kind === 'rebind' || kind === 'baseline') && running > 0;
      note.hidden = !show;
      if (show) setText(note, inFlightText(running));
    } else {
      setText(row.querySelector('.formula-help'), o.available ? KINDS.model.help : KINDS.model.help + ' ' + text(o.reason, ''));
    }
  });
}

function render() {
  renderFormula(state.lab);
  if (!fv.view) return;
  renderPending();
  renderCreate();
}

// The Formula view mounts on its data-view-panel; its markup stays in index.html.
// It reads GET /api/formula while the view is shown.
export function mount(root, ctx) {
  setInterval(() => { if (!document.hidden && state.view === 'formula' && !fv.pending) load(); }, 10000);
}

// update renders the active-Formula card from the Lab and reads the proposals.
export function update(state) {
  render();
  if (!fv.pending && Date.now() - fv.fetchedAt > 3000) load();
}
