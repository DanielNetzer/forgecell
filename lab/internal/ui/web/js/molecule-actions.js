// Molecule view: Why it's blocked · What you can do (#243). The panel lists
// the actions the engine accepts now, from the server's /actions read. Each is
// a primary button posting the exact plan digest it was shown; none approves,
// and its outcome shows as committed only from the server's ledger read-back.
// Retire stays in the Danger zone. It also holds the small form helpers the
// decision card shares.
import { $, el, uid, fill, state, ctx, ISSUE_URL, notRecorded, api, failed, plainText, count, counted, SessionError } from './core.js';
import { refusalText, postJSON } from './core-gates.js';

export function hasAction(name) { return Boolean(state.lab && Array.isArray(state.lab.actions) && state.lab.actions.includes(name)); }

export function latestDigest(d) {
  const plans = (d.readiness && d.readiness.plans) || [];
  const last = plans[plans.length - 1];
  return last && typeof last.digest === 'string' && /^[0-9a-f]{64}$/.test(last.digest) ? last.digest : '';
}

// submitButton is a form's submit button, so Enter submits the form.
export function submitButton(props, ...children) {
  const button = el('button', props, ...children);
  button.type = 'submit';
  return button;
}
export function field(id, label, input) { return el('div', { class: 'drive-field' }, el('label', { class: 'field-label', for: id }, label), input); }
// fillKeyed rebuilds node only when key changes, so an unchanged status is
// neither rebuilt nor announced again.
export function fillKeyed(node, key, build) {
  if (node.dataset.key === key) return;
  node.dataset.key = key;
  fill(node, key ? build() : null);
}

// acts is the panel's state. seen is the actionsKey the held result and
// message belong to: when the Molecule moves on, both are dropped.
const acts = { id: null, key: null, seen: null, token: 0, data: null, pending: false, message: '', result: null, reason: {}, confirm: {}, ids: new Map() };

// actionId is one uid() per action field, kept across rebuilds so a label
// stays bound and focus finds the same id after a poll. Kinds come from the
// server, so the map stays bounded whatever it sends.
function actionId(name) {
  if (!acts.ids.has(name)) { if (acts.ids.size >= 32) acts.ids.clear(); acts.ids.set(name, uid('mv-action')); }
  return acts.ids.get(name);
}

function actionsKey(d) { return [d.id, d.status, d.phase, d.lane, latestDigest(d)].join(':'); }
// actionsState reports the states the panel is for, from the ledger's status
// and phase alone.
function actionsState(d) { return d.status === 'blocked' || d.status === 'failed' || d.phase === 'interrupted' || d.phase === 'scope-change'; }

// forget drops the held outcome when d is not the Molecule state it was for.
function forget(d) {
  const seen = actionsKey(d);
  if (acts.seen !== seen) { acts.seen = seen; acts.result = null; acts.message = ''; }
}

export async function loadActions(d) {
  if (acts.id !== d.id) { acts.id = d.id; acts.result = null; acts.message = ''; acts.reason = {}; acts.confirm = {}; }
  forget(d);
  if (d.sample || (state.lab && state.lab.readOnly) || !hasAction('unblock Molecule')) { acts.key = null; acts.data = null; renderActions(d); return; }
  const key = actionsKey(d);
  if (key === acts.key) { renderActions(d); return; }
  acts.key = key;
  const token = ++acts.token;
  try {
    const res = await api('/api/molecules/' + encodeURIComponent(d.id) + '/actions');
    if (token !== acts.token) return;
    const data = res.ok ? await res.json() : null;
    if (token !== acts.token) return;
    acts.data = data ? { id: d.id, view: data } : null;
  } catch (error) { failed(error); }
  if (state.detail && state.detail.id === d.id) renderActions(state.detail);
}

function renderActions(d) {
  const node = $('#mv-actions');
  forget(d);
  const data = acts.data && acts.data.id === d.id ? acts.data.view : null;
  const list = data && Array.isArray(data.actions) ? data.actions.filter((a) => a && typeof a.kind === 'string') : [];
  const blocking = data ? plainText(data.blocking, '') : '';
  // An action this page cannot offer exactly is named with its plain reason.
  const notOffered = data && Array.isArray(data.notOffered) ? data.notOffered.filter((a) => a && typeof a.kind === 'string') : [];
  // The panel is for a way out (#243): a blocked, failed, interrupted or
  // scope-change Molecule, or one the engine offers more than Retire. A
  // Retire-only list on a gate waiting for approval or publishing stays in the
  // Danger zone, and a held outcome never shows the panel on its own.
  const stuck = actionsState(d) || list.some((a) => a.kind !== 'retire') || notOffered.length > 0;
  const shown = Boolean(stuck && (list.length || notOffered.length || blocking || acts.result || acts.message));
  node.hidden = !shown;
  fillKeyed(node, shown ? JSON.stringify([d.id, data, acts.result, acts.message, acts.pending]) : '', () => [
    el('h2', { class: 'decision-title', id: 'mv-actions-title' }, 'Why it’s blocked · What you can do'),
    blocking ? el('p', { class: 'decision-copy' }, blocking) : null,
    list.length ? null : el('p', { class: 'drive-help' }, 'The engine accepts no action for this Molecule now.'),
    ...list.map((a) => actionItem(d, a)),
    ...notOffered.map((a) => el('p', { class: 'drive-help', dataset: { action: 'not-offered' } }, plainText(a.label, 'Action') + ' is not offered: ' + plainText(a.reason, ''))),
    acts.message ? el('p', { class: 'drive-error', role: 'alert' }, acts.message) : null,
    acts.result ? actionResultText(acts.result) : null,
  ]);
}

// actionItem is one action: its plain words, what it shows, and a primary
// button directly under them. Retire only points at the Danger zone.
function actionItem(d, a) {
  const label = plainText(a.label, 'Action'), copy = plainText(a.copy, '');
  if (a.kind === 'retire') return el('p', { class: 'drive-help' }, label + ': ' + copy);
  const parts = [el('h3', { class: 'amend-title' }, label), copy ? el('p', { class: 'drive-help' }, copy) : null];
  if (a.kind === 'accept-failures') {
    const deltas = Array.isArray(a.deltas) ? a.deltas : [];
    parts.push(el('p', { class: 'scope-note' }, counted(deltas.length, 'test is', 'tests are') + ' declared as expected failures:'));
    parts.push(el('ul', { class: 'diagnostics' }, deltas.slice(0, 64).map((x) => el('li', null, el('code', null, notRecorded(x.package) + ' ' + notRecorded(x.test)), ' · check ' + notRecorded(x.check) + ' · declared in ', el('code', null, notRecorded(x.file))))));
  }
  if (a.kind === 'scope-change') {
    const paths = Array.isArray(a.paths) ? a.paths : [];
    parts.push(el('ul', { class: 'diagnostics' }, paths.map((p) => el('li', null, el('code', null, notRecorded(p))))));
    const id = actionId('reason-' + a.kind);
    const input = el('input', { type: 'text', id: id, maxlength: '1000', autocomplete: 'off', dataset: { action: 'reason' }, oninput: (e) => { acts.reason[a.kind] = e.target.value; } });
    input.value = acts.reason[a.kind] !== undefined ? acts.reason[a.kind] : plainText(a.reason, '');
    parts.push(field(id, 'Why these files need to change (the coding agent’s reason; edit it if needed)', input));
  }
  if (a.kind === 'fresh-intake') {
    const questions = Array.isArray(a.questions) ? a.questions : [];
    if (questions.length) parts.push(el('ul', { class: 'diagnostics' }, questions.map((q) => el('li', null, plainText(q, '')))));
    if (typeof a.issueUrl === 'string' && ISSUE_URL.test(a.issueUrl)) parts.push(el('a', { class: 'mv-issue-link', href: a.issueUrl, target: '_blank', rel: 'noopener noreferrer' }, 'Edit the Issue on GitHub ↗'));
  }
  if (a.confirmStopped) {
    const id = actionId('confirm-' + a.kind);
    const box = el('input', { type: 'checkbox', id: id, dataset: { action: 'confirm' }, onchange: (e) => { acts.confirm[a.kind] = e.target.checked; } });
    box.checked = acts.confirm[a.kind] === true;
    parts.push(el('label', { class: 'accept-label', for: id }, box, el('span', null, 'I confirm it is stopped: no Lab process works on this Molecule')));
  }
  const button = submitButton({ class: 'btn btn-primary btn-decision', dataset: { action: a.kind } }, acts.pending ? 'Working…' : label);
  button.disabled = acts.pending;
  parts.push(button);
  return el('form', { class: 'decision-form mv-action', novalidate: true, onsubmit: (e) => { e.preventDefault(); submitAction(d, a); } }, ...parts);
}

// submitAction posts once with the shown digest; after an ambiguous response
// it reads the Molecule back and never resubmits.
async function submitAction(d, a) {
  if (acts.pending) return;
  let path = '/api/molecules/' + encodeURIComponent(d.id) + '/actions/' + encodeURIComponent(a.kind);
  let body = { digest: a.digest };
  if (a.kind === 'fresh-intake') { path = '/api/runs'; body = { issue: a.issue }; }
  // Accept-failures binds the exact tests listed: the server derives them again.
  if (a.kind === 'accept-failures') body.deltas = a.deltasDigest;
  if (a.kind === 'scope-change') {
    const reason = String(acts.reason[a.kind] !== undefined ? acts.reason[a.kind] : plainText(a.reason, '')).trim();
    if (!reason || /[\r\n]/.test(reason)) { acts.message = 'Say in one line why these files need to change.'; renderActions(d); return; }
    body.paths = a.paths;
    body.reason = reason;
  }
  if (a.confirmStopped) {
    if (acts.confirm[a.kind] !== true) { acts.message = 'Tick I confirm it is stopped once no Lab process works on this Molecule.'; renderActions(d); return; }
    body.confirmStopped = true;
  }
  acts.pending = true;
  acts.message = '';
  renderActions(d);
  let res = null, out = {};
  try {
    res = await postJSON(path, JSON.stringify(body));
    out = await res.json().catch(() => ({}));
  } catch (error) {
    if (error instanceof SessionError) { acts.pending = false; failed(error); renderActions(d); return; }
    res = null;
  }
  acts.pending = false;
  if (!res || res.status >= 500) acts.message = 'The response was lost or ambiguous. The Molecule is read back; nothing is resubmitted.';
  else if (!res.ok) acts.message = refusalText(out);
  else acts.result = { kind: a.kind, body: out };
  acts.key = null;
  if (state.detail && state.detail.id === d.id) renderActions(state.detail);
  ctx.loadDetail(d.id, false);
}

function actionResultText(r) {
  const b = r.body || {};
  if (r.kind === 'fresh-intake') return el('p', { class: 'scope-note' }, b.committed ? 'Started: the ledger records a fresh Molecule for this Issue.' : 'Intake was submitted. Not shown as started until a ledger for this Issue appears.');
  if (r.kind === 'recover') return el('p', { class: 'scope-note' }, b.committed ? 'Recovered: the ledger records the interruption and keeps the work. Choose the next action above.' : 'Not committed: the ledger read back does not record the recovery.');
  if (!b.committed) return el('p', { class: 'scope-note' }, (b.recovered ? 'Recovered, but ' : '') + 'Not committed: the ledger read back shows no new version of this plan. Nothing was approved.');
  const digest = typeof b.digest === 'string' && /^[0-9a-f]{64}$/.test(b.digest) ? b.digest.slice(0, 12) : 'not recorded';
  const deltas = Array.isArray(b.deltas) ? b.deltas : [];
  return el('div', { class: 'gate-job' },
    el('p', { class: 'scope-note' }, 'Version ' + count(b.revision) + ' of the plan is recorded (digest ', el('code', null, digest), '…' + (b.changes ? '; ' + plainText(b.changes, '') : '') + '). It waits for your approval: click Approve plan & start coding. Nothing was approved.'),
    deltas.length ? el('ul', { class: 'diagnostics' }, deltas.slice(0, 64).map((x) => el('li', null, el('code', null, notRecorded(x.package) + ' ' + notRecorded(x.test)), ' · declared in ', el('code', null, notRecorded(x.file))))) : null);
}
