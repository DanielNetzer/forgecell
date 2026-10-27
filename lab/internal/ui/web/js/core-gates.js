// ---- Human gates ---------------------------------------------------------------
// Each gate posts the exact digest this page showed. One request is in flight
// at a time and the controls are disabled meanwhile. After an ambiguous
// response (no response, or a 5xx) the page reads the canonical state back
// and never resubmits. A decision shows as committed only when the server's
// read-back of the ledger or the receipt says so, never because a job ended.
import { el, state, ctx, notRecorded, SessionError, tabSession, api, failed, keepFocus } from './core.js';

export async function gatePost(id, action, body) {
  return postJSON('/api/molecules/' + encodeURIComponent(id) + action, body);
}

// postJSON is one write: the tab session header, same-origin credentials and a
// JSON body. A 403 means this tab lost its session.
export async function postJSON(path, body) {
  const session = tabSession();
  if (!session || state.sessionLost) throw new SessionError('no session');
  const res = await fetch(path, { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'X-Forgecell-Session': session, 'Content-Type': 'application/json' }, body: body });
  if (res.status === 403) throw new SessionError('forbidden');
  return res;
}

// reasonText is the server's bounded, redacted next step for a refusal, or ''.
export function reasonText(body) {
  return body && typeof body.reason === 'string' && body.reason ? ' ' + body.reason + '.' : '';
}

export function refusalText(body) {
  const category = body && typeof body.category === 'string' ? body.category : 'refused';
  return 'Refused (' + category + '): ' + notRecorded(body && body.error) + '.' + reasonText(body) + ' Nothing was resubmitted.';
}

export function rerenderGate(gate) {
  if (state.gate === gate && state.detail && state.detail.id === gate.id) keepFocus(() => ctx.renderDecision(state.detail));
}

// GATE_FORMS names the form whose inline error shows each action's outcome;
// every other action reports in the decision card.
const GATE_FORMS = { '/amend': 'amend', '/delivery/publish': 'publish', '/stop': 'stop', '/retire': 'retire' };

// submitGate posts once. It returns the accepted body, or null after a refusal
// or an ambiguous response, which is read back instead of retried. Focus moves
// to the inline error of the action's form.
export async function submitGate(action, payload) {
  if (state.gate.pending) return null;
  const gate = state.gate;
  gate.pending = true;
  gate.where = GATE_FORMS[action] || 'decision';
  gate.message = '';
  rerenderGate(gate);
  let res = null, body = {};
  try {
    res = await gatePost(gate.id, action, payload);
    body = await res.json().catch(() => ({}));
  } catch (error) {
    if (error instanceof SessionError) { gate.pending = false; failed(error); rerenderGate(gate); return null; }
    res = null;
  }
  gate.pending = false;
  if (state.gate !== gate) return null;
  if (!res || res.status >= 500) {
    gate.message = 'The response was lost or ambiguous. The canonical state is read back; nothing is resubmitted.';
    gate.announce = true;
    readBack(gate);
    return null;
  }
  if (!res.ok && action === '/amend' && body && body.category === 'stale') {
    // Someone recorded a newer plan after this one was shown: load it to review.
    gate.message = 'A newer plan exists, so your changes were not recorded. The newer plan is loaded; review it before requesting changes again.';
    gate.announce = true;
    readBack(gate);
    return null;
  }
  if (!res.ok) { gate.message = refusalText(body); gate.announce = true; rerenderGate(gate); return null; }
  return body;
}

// gateMessage shows a check this page makes before anything is sent.
export function gateMessage(where, text) {
  const gate = state.gate;
  gate.where = where;
  gate.message = text;
  gate.announce = true;
  rerenderGate(gate);
}

// fetchJob reads the job and its canonical read-back; while the job runs it
// reads again, one timer at a time.
export async function fetchJob(gate) {
  clearTimeout(gate.timer);
  try {
    const res = await api('/api/molecules/' + encodeURIComponent(gate.id) + '/job');
    const job = res.ok ? await res.json() : null;
    if (state.gate !== gate) return;
    // A Molecule with no job from this Lab UI answers 200 with state none.
    gate.job = job && job.state !== 'none' ? job : null;
    if (job && job.state === 'running') gate.timer = setTimeout(() => readBack(gate), 2000);
  } catch (error) { failed(error); }
  rerenderGate(gate);
}

export function readBack(gate) {
  if (state.gate !== gate) return;
  fetchJob(gate);
  ctx.loadDetail(gate.id, false);
}

export function jobText(job) {
  if (!job) return null;
  const c = job.canonical || {};
  const kind = job.kind === 'scope' ? 'Scope approval' : 'Draft publication';
  let status;
  if (c.unreadable) status = 'The ' + (c.source || 'record') + ' could not be read back; nothing is shown as committed.';
  else if (job.committed && job.kind === 'scope') status = 'Committed: the ledger records your approval of this plan. Coding starts on its own.';
  else if (job.committed) status = 'Committed: receipt ' + notRecorded(c.receipt) + ', lifecycle ' + notRecorded(c.lifecycle) + (c.published ? '. Draft published; merge and production are not verified.' : '. Not published as a draft yet.');
  else if (job.state === 'running') status = 'Running. Not committed until the ' + (job.kind === 'scope' ? 'ledger records the approval.' : 'receipt records it.');
  else status = 'Not committed: the ' + (job.kind === 'scope' ? 'ledger does not record this approval' : 'receipt does not record a committed delivery') + (c.receipt ? ' (receipt ' + c.receipt + (c.lifecycle ? ', lifecycle ' + c.lifecycle : '') + ')' : '') + '.';
  // Not a live region itself: the view's one live region, #mv-live, carries it.
  return el('div', { class: 'gate-job' },
    el('strong', null, kind + ' · ' + notRecorded(job.state)),
    el('p', { class: 'scope-note' }, status),
    job.error ? el('p', { class: 'scope-note' }, (job.category ? job.category + ': ' : '') + job.error + '.' + reasonText(job)) : null);
}

// ---- The decision card's gate posts (used by the Molecule view) ------------------
// approveScope posts the exact digest the plan shows; the approval shows as
// committed only once the ledger read back records it.
export async function approveScope(digest) {
  const job = await submitGate('/scope', JSON.stringify({ digest: digest }));
  if (job) { state.gate.job = job; readBack(state.gate); }
}

export async function previewDelivery() {
  const gate = state.gate;
  const preview = await submitGate('/delivery/preview', '{}');
  if (preview && state.gate === gate) { gate.preview = preview; rerenderGate(gate); }
}

export async function publishDelivery() {
  const gate = state.gate;
  const preview = gate.preview, title = gate.title.trim(), accept = gate.accept;
  if (!preview || !title) { gateMessage('publish', 'Enter a one-line title for the draft PR, such as the change in a few words.'); return; }
  if (preview.acceptanceUnverified && !accept) { gateMessage('publish', 'Tick the box once you have checked the acceptance criteria yourself.'); return; }
  const job = await submitGate('/delivery/publish', JSON.stringify({ digest: preview.digest, title: title, acceptUnverified: accept }));
  if (job && state.gate === gate) { gate.job = job; readBack(gate); }
}

// stopMolecule posts once; the stop shows as done only when the ledger read back
// no longer reports the Molecule running.
export async function stopMolecule() {
  const gate = state.gate;
  const stop = await submitGate('/stop', '{}');
  if (stop && state.gate === gate) { gate.stop = stop; rerenderGate(gate); ctx.loadDetail(gate.id, false); }
}

// retireMolecule posts once with the typed reason; the retirement shows as
// committed only when the response's ledger read-back records it.
export async function retireMolecule() {
  const gate = state.gate;
  const reason = gate.reason.trim();
  if (!reason || /[\r\n]/.test(reason) || [...reason].length > 200) { gateMessage('retire', 'Enter a one-line reason of at most 200 characters, such as why the Molecule is no longer needed.'); return; }
  const retired = await submitGate('/retire', JSON.stringify({ reason: reason }));
  if (retired && state.gate === gate) { gate.retire = retired; rerenderGate(gate); ctx.loadDetail(gate.id, false); }
}
