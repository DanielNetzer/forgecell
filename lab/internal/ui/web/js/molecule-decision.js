// ---- Molecule view: the decision card, the danger zone and the roles ------------
// The sticky decision card has one primary action, bound to the exact digest
// the plan shows. Retire and Stop sit apart, in the collapsed Not needed
// anymore? block, each behind a confirmation that names the Molecule. Controls
// are built once and patched in place, so a poll never rebuilds a form being
// typed in, and the one live region changes only when its text does.
import { $, el, fill, state, ctx, newGate, RETIRED, PULL_URL, ISSUE_URL, notRecorded, api, failed, setText, setAttr, plainText, count, counted, wholeNumber, money, whenText, dateTitle, minutesText, STEPS, show, SessionError } from './core.js';
import { submitGate, gateMessage, fetchJob, readBack, rerenderGate, jobText, refusalText, postJSON } from './core-gates.js';

// mv is the Molecule view's state, shared by its sub-modules.
export const mv = { controls: null, steps: null, from: 'work', planKey: null, statsKey: null, statsToken: 0, stats: null, lastDigest: null, replaced: false, timelineId: null, timelineDirty: false };

// approveScope posts the exact digest the plan shows; the approval shows as
// committed only once the ledger read back records it.
async function approveScope(digest) {
  const job = await submitGate('/scope', JSON.stringify({ digest: digest }));
  if (job) { state.gate.job = job; readBack(state.gate); }
}

async function previewDelivery() {
  const gate = state.gate;
  const preview = await submitGate('/delivery/preview', '{}');
  if (preview && state.gate === gate) { gate.preview = preview; rerenderGate(gate); }
}

async function publishDelivery() {
  const gate = state.gate;
  const preview = gate.preview, title = gate.title.trim(), accept = gate.accept;
  if (!preview || !title) { gateMessage('publish', 'Enter a one-line title for the draft PR, such as the change in a few words.'); return; }
  if (preview.acceptanceUnverified && !accept) { gateMessage('publish', 'Tick the box once you have checked the acceptance criteria yourself.'); return; }
  const job = await submitGate('/delivery/publish', JSON.stringify({ digest: preview.digest, title: title, acceptUnverified: accept }));
  if (job && state.gate === gate) { gate.job = job; readBack(gate); }
}

// requestChanges posts the additions with the exact digest the plan shows. The
// engine records a new version of the plan and approves nothing: the new
// version waits for its own approval through Approve plan & start coding.
async function requestChanges(digest, acceptance, path, reason) {
  const gate = state.gate;
  const body = { digest: digest, addAcceptance: acceptance ? [acceptance] : [], addScope: path ? [{ path: path, reason: reason }] : [] };
  const amended = await submitGate('/amend', JSON.stringify(body));
  if (amended && state.gate === gate) {
    gate.amend = amended;
    if (amended.committed) clearChanges();
    rerenderGate(gate);
    ctx.loadDetail(gate.id, false);
  }
}

// stopMolecule posts once; the stop shows as done only when the ledger read back
// no longer reports the Molecule running.
async function stopMolecule() {
  const gate = state.gate;
  const stop = await submitGate('/stop', '{}');
  if (stop && state.gate === gate) { gate.stop = stop; rerenderGate(gate); ctx.loadDetail(gate.id, false); }
}

// retireMolecule posts once with the typed reason; the retirement shows as
// committed only when the response's ledger read-back records it.
async function retireMolecule() {
  const gate = state.gate;
  const reason = gate.reason.trim();
  if (!reason || /[\r\n]/.test(reason) || [...reason].length > 200) { gateMessage('retire', 'Enter a one-line reason of at most 200 characters, such as why the Molecule is no longer needed.'); return; }
  const retired = await submitGate('/retire', JSON.stringify({ reason: reason }));
  if (retired && state.gate === gate) { gate.retire = retired; rerenderGate(gate); ctx.loadDetail(gate.id, false); }
}

function retireText(d, gate) {
  const r = d.retired;
  if (d.lane === RETIRED && r) {
    const when = whenText(r.at);
    return el('p', { class: 'scope-note', title: dateTitle(r.at) }, 'Retired' + (when ? ' ' + when : '') + '. Reason: ' + notRecorded(r.reason) + '. Every record is kept.');
  }
  if (gate.retire) return el('p', { class: 'scope-note' }, gate.retire.committed ? 'Committed: the ledger records the retirement.' : 'Not committed: the ledger read back does not record the retirement.');
  return null;
}

function hasAction(name) { return Boolean(state.lab && Array.isArray(state.lab.actions) && state.lab.actions.includes(name)); }

function latestDigest(d) {
  const plans = (d.readiness && d.readiness.plans) || [];
  const last = plans[plans.length - 1];
  return last && typeof last.digest === 'string' && /^[0-9a-f]{64}$/.test(last.digest) ? last.digest : '';
}

// submitButton is a form's submit button, so Enter submits the form.
function submitButton(props, ...children) {
  const button = el('button', props, ...children);
  button.type = 'submit';
  return button;
}
function field(id, label, input) { return el('div', { class: 'drive-field' }, el('label', { class: 'field-label', for: id }, label), input); }
// fillKeyed rebuilds node only when key changes, so an unchanged status is
// neither rebuilt nor announced again.
function fillKeyed(node, key, build) {
  if (node.dataset.key === key) return;
  node.dataset.key = key;
  fill(node, key ? build() : null);
}
function moleculeName(d) {
  const title = plainText(d.issue && d.issue.title, 'Untitled');
  const number = d.issue && Number.isInteger(d.issue.number) ? '#' + d.issue.number + ' · ' : '';
  return number + ([...title].length > 80 ? [...title].slice(0, 79).join('') + '…' : title);
}

// ensureMoleculeView builds the decision card, the danger zone, the roles and
// the steps once.
export function ensureMoleculeView() {
  if (mv.controls) return mv.controls;
  const c = {};
  c.heading = el('h2', { class: 'decision-title', id: 'mv-decision-title', tabindex: '-1' });
  c.copy = el('p', { class: 'decision-copy' });
  c.changed = el('p', { class: 'decision-changed', hidden: true });
  c.size = el('p', { class: 'decision-size', hidden: true });
  // The scope gate: Approve is the card's one primary, bound through its form to
  // the digest the plan shows; Request changes is secondary and approves nothing.
  c.approve = submitButton({ class: 'btn btn-primary btn-decision', 'aria-describedby': 'mv-gate-error', dataset: { gate: 'scope' } }, 'Approve plan & start coding');
  c.approveForm = el('form', { class: 'decision-form', id: 'mv-approve-form', novalidate: true, hidden: true, onsubmit: submitApprove }, c.approve);
  c.changes = el('button', { type: 'button', class: 'btn btn-secondary btn-decision-secondary', 'aria-expanded': 'false', 'aria-controls': 'mv-amend-form', hidden: true, dataset: { gate: 'changes' }, onclick: toggleChanges }, 'Request changes…');
  c.acceptance = el('input', { type: 'text', id: 'mv-amend-acceptance', name: 'acceptance', maxlength: '1000', autocomplete: 'off', placeholder: 'The help text names the new flag…', 'aria-describedby': 'mv-amend-error', dataset: { gate: 'acceptance' } });
  c.path = el('input', { type: 'text', id: 'mv-amend-path', name: 'path', maxlength: '300', autocomplete: 'off', spellcheck: 'false', placeholder: 'lab/internal/ui/web/app.js…', 'aria-describedby': 'mv-amend-error', dataset: { gate: 'path' } });
  c.reason = el('input', { type: 'text', id: 'mv-amend-reason', name: 'reason', maxlength: '1000', autocomplete: 'off', placeholder: 'Wire the new button…', 'aria-describedby': 'mv-amend-error', dataset: { gate: 'path-reason' } });
  c.amendError = el('p', { class: 'drive-error', id: 'mv-amend-error', tabindex: '-1', hidden: true });
  c.amendSubmit = submitButton({ class: 'btn btn-secondary', dataset: { gate: 'amend' } }, 'Record changes');
  c.amendForm = el('form', { class: 'amend-form', id: 'mv-amend-form', 'aria-labelledby': 'mv-amend-title', novalidate: true, hidden: true, onsubmit: submitChanges },
    el('h3', { class: 'amend-title', id: 'mv-amend-title' }, 'Request changes'),
    el('p', { class: 'drive-help' }, 'Forgecell records your changes as a new version of this plan. Nothing is approved now: you review and approve the new version on its own.'),
    field('mv-amend-acceptance', 'Add an acceptance note', c.acceptance),
    el('fieldset', { class: 'amend-scope' }, el('legend', { class: 'field-label' }, 'Or let it change one more file'),
      field('mv-amend-path', 'File path', c.path), field('mv-amend-reason', 'Why it needs to change', c.reason)),
    c.amendError, c.amendSubmit);
  c.amendStatus = el('div', { class: 'gate-status' });
  // The review gate: Preview is the primary until a preview is shown; then
  // Publish is, and Preview again turns secondary.
  c.preview = el('button', { type: 'button', class: 'btn btn-primary btn-decision', 'aria-describedby': 'mv-gate-error', hidden: true, dataset: { gate: 'preview' }, onclick: previewDelivery }, 'Preview delivery');
  c.previewInfo = el('div', { class: 'preview-info' });
  c.title = el('input', { type: 'text', id: 'mv-publish-title', name: 'title', maxlength: '256', autocomplete: 'off', placeholder: 'Add the Molecule view to the Lab UI…', 'aria-describedby': 'mv-publish-error', dataset: { gate: 'title' }, oninput: (e) => { state.gate.title = e.target.value; } });
  c.accept = el('input', { type: 'checkbox', id: 'mv-publish-accept', name: 'acceptUnverified', 'aria-describedby': 'mv-accept-help', dataset: { gate: 'accept' }, onchange: (e) => { state.gate.accept = e.target.checked; } });
  c.acceptField = el('div', { class: 'accept-field', hidden: true },
    el('label', { class: 'accept-label', for: 'mv-publish-accept' }, c.accept, el('span', null, 'I checked the acceptance criteria myself')),
    el('p', { class: 'drive-help', id: 'mv-accept-help' }, 'No independent check confirmed that this change meets its acceptance criteria. Tick this only once you have reviewed them yourself; your confirmation is recorded with the draft PR.'));
  c.publishError = el('p', { class: 'drive-error', id: 'mv-publish-error', tabindex: '-1', hidden: true });
  c.publish = submitButton({ class: 'btn btn-primary btn-decision', dataset: { gate: 'publish' } }, 'Publish draft PR');
  c.publishForm = el('form', { class: 'decision-form', id: 'mv-publish-form', novalidate: true, hidden: true, onsubmit: submitPublish },
    field('mv-publish-title', 'Draft PR title', c.title), c.acceptField, c.publishError, c.publish);
  c.pull = el('a', { class: 'btn btn-secondary decision-link', target: '_blank', rel: 'noopener noreferrer', hidden: true }, 'Open the pull request ↗');
  c.error = el('p', { class: 'drive-error', id: 'mv-gate-error', tabindex: '-1', hidden: true });
  c.job = el('div', { class: 'gate-status' });
  c.estimate = el('p', { class: 'decision-estimate', hidden: true });
  fill($('#mv-decision'), c.heading, c.copy, c.changed, c.size,
    c.approveForm, c.changes, c.amendForm, c.amendStatus,
    c.preview, c.previewInfo, c.publishForm, c.pull,
    c.error, c.job, c.estimate);

  // The danger zone: its own collapsed block, never beside a primary. Each
  // action opens a confirmation that names the Molecule.
  c.stopOpen = el('button', { type: 'button', class: 'btn btn-danger', 'aria-expanded': 'false', 'aria-controls': 'mv-stop-form', dataset: { gate: 'stop-open' }, onclick: () => openConfirm('stop') }, 'Stop this Molecule…');
  c.stopQuestion = el('p', { class: 'confirm-question', id: 'mv-stop-question' });
  c.stopError = el('p', { class: 'drive-error', id: 'mv-stop-error', tabindex: '-1', hidden: true });
  c.stopSubmit = submitButton({ class: 'btn btn-danger', 'aria-describedby': 'mv-stop-error', dataset: { gate: 'stop' } }, 'Stop Molecule');
  c.stopForm = el('form', { class: 'confirm-form', id: 'mv-stop-form', 'aria-labelledby': 'mv-stop-question', novalidate: true, hidden: true, onsubmit: submitStop },
    c.stopQuestion,
    el('p', { class: 'drive-help' }, 'Its process group is stopped and the ledger records the stop. Work a loop or the forgecell CLI runs is refused.'),
    c.stopError,
    el('div', { class: 'confirm-actions' }, c.stopSubmit, el('button', { type: 'button', class: 'btn btn-secondary', dataset: { gate: 'stop-cancel' }, onclick: () => closeConfirm('stop', true) }, 'Cancel')));
  c.stopStatus = el('div', { class: 'gate-status' });
  c.stopBlock = el('div', { class: 'danger-item' }, el('p', null, 'Stopping ends the work this Lab UI started for this Molecule.'), c.stopOpen, c.stopForm, c.stopStatus);
  c.retireOpen = el('button', { type: 'button', class: 'btn btn-danger', 'aria-expanded': 'false', 'aria-controls': 'mv-retire-form', dataset: { gate: 'retire-open' }, onclick: () => openConfirm('retire') }, 'Retire this Molecule…');
  c.retireQuestion = el('p', { class: 'confirm-question', id: 'mv-retire-question' });
  c.reasonInput = el('input', { type: 'text', id: 'mv-retire-reason', name: 'reason', maxlength: '200', autocomplete: 'off', placeholder: 'Replaced by a fresh Molecule…', 'aria-describedby': 'mv-retire-error', dataset: { gate: 'reason' }, oninput: (e) => { state.gate.reason = e.target.value; } });
  c.retireError = el('p', { class: 'drive-error', id: 'mv-retire-error', tabindex: '-1', hidden: true });
  c.retireSubmit = submitButton({ class: 'btn btn-danger', dataset: { gate: 'retire' } }, 'Confirm retire');
  c.retireForm = el('form', { class: 'confirm-form', id: 'mv-retire-form', 'aria-labelledby': 'mv-retire-question', novalidate: true, hidden: true, onsubmit: submitRetire },
    c.retireQuestion, field('mv-retire-reason', 'Reason', c.reasonInput), c.retireError,
    el('div', { class: 'confirm-actions' }, c.retireSubmit, el('button', { type: 'button', class: 'btn btn-secondary', dataset: { gate: 'retire-cancel' }, onclick: () => closeConfirm('retire', true) }, 'Cancel')));
  c.retireStatus = el('div', { class: 'gate-status' });
  c.retireBlock = el('div', { class: 'danger-item' }, el('p', null, 'Retiring keeps every record, but this Molecule can’t be resumed. You can start a fresh one for the Issue later.'), c.retireOpen, c.retireForm, c.retireStatus);
  c.danger = el('details', { class: 'danger-zone', id: 'mv-danger', hidden: true }, el('summary', null, 'Not needed anymore?'), el('div', { class: 'danger-body' }, c.stopBlock, c.retireBlock));
  // runnerNote replaces Stop when this Lab UI does not run the engine call.
  c.runnerNote = el('p', { class: 'drive-help runner-note', id: 'mv-runner-note', hidden: true });
  fill($('#mv-danger-slot'), c.runnerNote, c.danger);

  c.roles = { coding: el('dd'), reviewer: el('dd'), cost: el('dd', { class: 'num' }) };
  fill($('#mv-roles'),
    el('div', null, el('dt', null, 'Coding agent'), c.roles.coding),
    el('div', null, el('dt', null, 'Reviewer'), c.roles.reviewer),
    el('div', null, el('dt', null, 'Planning cost so far'), c.roles.cost));
  c.issue = el('a', { class: 'mv-issue-link', target: '_blank', rel: 'noopener noreferrer', hidden: true });
  c.issueText = el('span', { hidden: true });
  fill($('#mv-issue'), c.issue, c.issueText);
  mv.steps = STEPS.map((label) => el('li', { class: 'mv-step is-todo' }, el('span', { class: 'step-bar', 'aria-hidden': 'true' }), el('span', { class: 'mv-step-label' }, label)));
  fill($('#mv-steps'), mv.steps);
  mv.controls = c;
  return c;
}

// resetDecision clears what was typed for the previous Molecule: a title, a
// reason or a requested change never carries over to another one.
function resetDecision(c) {
  for (const input of [c.title, c.acceptance, c.path, c.reason, c.reasonInput]) input.value = '';
  c.accept.checked = false;
  c.amendForm.hidden = true;
  c.changes.setAttribute('aria-expanded', 'false');
  for (const kind of ['stop', 'retire']) { c[kind + 'Form'].hidden = true; c[kind + 'Open'].setAttribute('aria-expanded', 'false'); }
  c.danger.open = false;
  c.previewInfo.dataset.key = '';
  mv.replaced = false;
}

function submitApprove(e) {
  e.preventDefault();
  const digest = mv.controls.approveForm.dataset.digest;
  if (digest) approveScope(digest);
}
function submitChanges(e) {
  e.preventDefault();
  const c = mv.controls, digest = c.approveForm.dataset.digest;
  const acceptance = c.acceptance.value.trim(), path = c.path.value.trim(), reason = c.reason.value.trim();
  if (!digest) return;
  if (!acceptance && !path && !reason) { gateMessage('amend', 'Add an acceptance note, or a file path with the reason it needs to change.'); return; }
  if (path && !reason) { gateMessage('amend', 'Say why this file needs to change, such as the behaviour it adds.'); return; }
  if (reason && !path) { gateMessage('amend', 'Add the file path this reason is for, such as lab/internal/ui/web/app.js.'); return; }
  requestChanges(digest, acceptance, path, reason);
}
function submitPublish(e) { e.preventDefault(); publishDelivery(); }
function submitStop(e) { e.preventDefault(); stopMolecule(); }
function submitRetire(e) { e.preventDefault(); retireMolecule(); }

function toggleChanges() {
  const c = mv.controls;
  if (!c.amendForm.hidden) { closeChanges(true); return; }
  c.amendForm.hidden = false;
  c.changes.setAttribute('aria-expanded', 'true');
  c.acceptance.focus();
}
function closeChanges(focus) {
  const c = mv.controls, had = c.amendForm.contains(document.activeElement);
  c.amendForm.hidden = true;
  c.changes.setAttribute('aria-expanded', 'false');
  if (focus || had) (c.changes.hidden ? $('#mv-decision-title') : c.changes).focus({ preventScroll: true });
}
function clearChanges() {
  const c = mv.controls;
  for (const input of [c.acceptance, c.path, c.reason]) input.value = '';
  closeChanges(false);
}
function openConfirm(kind) {
  const c = mv.controls;
  c[kind + 'Form'].hidden = false;
  c[kind + 'Open'].setAttribute('aria-expanded', 'true');
  (kind === 'retire' ? c.reasonInput : c.stopSubmit).focus();
}
function closeConfirm(kind, focus) {
  const c = mv.controls, form = c[kind + 'Form'], open = c[kind + 'Open'];
  const had = form.contains(document.activeElement);
  form.hidden = true;
  open.setAttribute('aria-expanded', 'false');
  if (focus || had) (open.hidden ? c.danger.querySelector('summary') : open).focus({ preventScroll: true });
}

// boundDigest is the digest the Approve form posts: the decision's primary
// digest, only when it is the plan's, the gate's and the latest plan's too.
function boundDigest(d) {
  const v = d.view || {}, dec = v.decision || {}, primary = dec.primary || {}, plan = v.plan || {}, g = d.gate || {};
  const digest = typeof primary.digest === 'string' && /^[0-9a-f]{64}$/.test(primary.digest) ? primary.digest : '';
  return digest && primary.action === 'approve' && digest === plan.digest && digest === g.digest && digest === latestDigest(d) ? digest : '';
}

// renderDecision patches the decision card and the danger zone in place. It
// never sets the Publish title or any other typed value: only resetDecision,
// for another Molecule, clears them.
export function renderDecision(d) {
  const c = ensureMoleculeView();
  if (state.gate.id !== d.id) {
    clearTimeout(state.gate.timer);
    state.gate = newGate(d.id);
    resetDecision(c);
    if (!d.sample) fetchJob(state.gate);
  }
  const gate = state.gate;
  const v = d.view || {}, dec = v.decision || {};
  const readOnly = Boolean(d.sample || (state.lab && state.lab.readOnly));
  const running = Boolean(gate.job && gate.job.state === 'running');
  const busy = gate.pending || running;
  const digest = readOnly ? '' : boundDigest(d);
  const scope = dec.kind === 'scope' && Boolean(digest);
  const delivery = !readOnly && dec.kind === 'delivery' && Boolean(d.gate && d.gate.state === 'delivery');
  setText(c.heading, readOnly ? 'Sample data' : plainText(dec.title, 'Nothing to do'));
  setText(c.copy, readOnly ? 'Sample data has no gates. Nothing here writes.' : plainText(dec.copy, ''));

  // A plan replaced while it is open says so before it can be approved.
  if (scope) {
    const own = gate.amend && gate.amend.digest === digest;
    if (mv.lastDigest && mv.lastDigest.id === d.id && mv.lastDigest.digest !== digest && !own) mv.replaced = true;
    mv.lastDigest = { id: d.id, digest: digest };
  }
  const replaced = scope && mv.replaced ? 'This plan was replaced by version ' + count(v.plan && v.plan.revision) + ' while it was open. Review it before you approve.' : '';
  setText(c.changed, replaced);
  c.changed.hidden = !replaced;

  // The scope gate.
  c.approveForm.hidden = !scope;
  if (scope) c.approveForm.dataset.digest = digest;
  else delete c.approveForm.dataset.digest;
  c.approve.disabled = busy;
  c.approve.setAttribute('aria-disabled', String(busy));
  setText(c.approve, gate.pending && gate.where === 'decision' && scope ? 'Approving…' : 'Approve plan & start coding');
  const amendable = scope && dec.requestChanges === true && hasAction('request plan changes');
  c.changes.hidden = !amendable;
  if (!amendable && !c.amendForm.hidden) closeChanges(false);
  c.changes.disabled = busy;
  c.amendSubmit.disabled = busy;
  setText(c.amendSubmit, gate.pending && gate.where === 'amend' ? 'Recording…' : 'Record changes');
  fillKeyed(c.amendStatus, gate.amend ? JSON.stringify(gate.amend) : '', () => amendText(gate.amend));

  // The review gate: exactly one primary at a time.
  const preview = delivery ? gate.preview : null;
  c.preview.hidden = !delivery;
  setAttr(c.preview, 'class', preview ? 'btn btn-secondary btn-decision-secondary' : 'btn btn-primary btn-decision');
  setText(c.preview, gate.pending && gate.where === 'decision' && delivery ? 'Previewing…' : preview ? 'Preview again' : 'Preview delivery');
  c.preview.disabled = busy;
  c.publishForm.hidden = !preview;
  c.acceptField.hidden = !(preview && preview.acceptanceUnverified);
  c.publish.disabled = busy;
  setText(c.publish, gate.pending && gate.where === 'publish' ? 'Publishing…' : 'Publish draft PR');
  renderPreviewInfo(c, preview);

  // A published or merged Molecule: nothing to do, and its pull request.
  const pull = !readOnly && dec.kind === 'published' && typeof dec.pullUrl === 'string' && PULL_URL.test(dec.pullUrl) ? dec.pullUrl : '';
  c.pull.hidden = !pull;
  if (pull) setAttr(c.pull, 'href', pull);
  else c.pull.removeAttribute('href');

  // The diff size on both gates; the estimate before coding.
  const stats = (scope || delivery) && mv.stats && mv.stats.id === d.id ? mv.stats.data : null;
  const size = stats ? diffText(stats.diff) : '';
  setText(c.size, size);
  c.size.hidden = !size;
  const estimate = stats && scope ? estimateText(stats.estimate) : '';
  setText(c.estimate, estimate);
  c.estimate.hidden = !estimate;
  fillKeyed(c.job, gate.job ? JSON.stringify(gate.job) : '', () => jobText(gate.job));

  // The danger zone.
  const live = d.status === 'running' || d.status === 'allocating';
  const retired = d.lane === RETIRED || Boolean(gate.retire);
  // Stop is offered only when the server says this Lab UI runs the engine call.
  const control = d.view && d.view.control;
  const stoppable = live && Boolean(control && control.stoppable === true);
  const canStop = !readOnly && (stoppable || Boolean(gate.stop)) && hasAction('stop Molecule');
  const canRetire = !readOnly && !live && !retired && hasAction('retire Molecule');
  const runner = live && !stoppable && !gate.stop && control && typeof control.text === 'string' ? plainText(control.text, '') : '';
  setText(c.runnerNote, runner);
  c.runnerNote.hidden = !runner;
  c.stopBlock.hidden = !canStop;
  c.stopOpen.hidden = !stoppable || Boolean(gate.stop);
  if (c.stopOpen.hidden && !c.stopForm.hidden) closeConfirm('stop', false);
  c.stopSubmit.disabled = gate.pending;
  setText(c.stopSubmit, gate.pending && gate.where === 'stop' ? 'Stopping…' : 'Stop Molecule');
  setText(c.stopQuestion, 'Stop ' + moleculeName(d) + '?');
  fillKeyed(c.stopStatus, gate.stop ? JSON.stringify([gate.stop, live]) : '', () => stopText(gate, live));
  c.retireBlock.hidden = !(canRetire || (!readOnly && retired));
  c.retireOpen.hidden = !canRetire;
  if (!canRetire && !c.retireForm.hidden) closeConfirm('retire', false);
  c.retireSubmit.disabled = gate.pending || running;
  setText(c.retireSubmit, gate.pending && gate.where === 'retire' ? 'Retiring…' : 'Confirm retire');
  setText(c.retireQuestion, 'Retire ' + moleculeName(d) + '?');
  fillKeyed(c.retireStatus, JSON.stringify([d.lane, d.retired, gate.retire]), () => retireText(d, gate));
  c.danger.hidden = c.stopBlock.hidden && c.retireBlock.hidden;
  if (c.danger.hidden) c.danger.open = false;

  // One inline error, in the form that sent the request; focus moves to it once.
  const errors = { decision: c.error, amend: c.amendError, publish: c.publishError, stop: c.stopError, retire: c.retireError };
  let target = errors[gate.where] || c.error;
  if (target.parentElement.closest('[hidden]')) target = c.error;
  for (const node of Object.values(errors)) {
    const text = node === target ? gate.message : '';
    setText(node, text);
    node.hidden = !text;
  }
  if (gate.message && gate.announce) { gate.announce = false; target.focus(); }
  // The one live region of the view carries the outcomes read back.
  setText($('#mv-live'), [c.job, c.amendStatus, c.stopStatus, c.retireStatus].map((n) => n.textContent).filter(Boolean).join(' '));
  loadActions(d);
}

// ---- Why it's blocked · What you can do (#243) ----------------------------------
// The panel lists the actions the engine accepts now, from the server's
// /actions read. Each is a primary button posting the exact plan digest it was
// shown; none approves, and its outcome shows as committed only from the
// server's ledger read-back. Retire stays in the Danger zone below.
const acts = { id: null, key: null, token: 0, data: null, pending: false, message: '', result: null, reason: {}, confirm: {} };

function actionsKey(d) { return [d.id, d.status, d.phase, d.lane, latestDigest(d)].join(':'); }

export async function loadActions(d) {
  if (acts.id !== d.id) { acts.id = d.id; acts.result = null; acts.message = ''; acts.reason = {}; acts.confirm = {}; }
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
  const data = acts.data && acts.data.id === d.id ? acts.data.view : null;
  const list = data && Array.isArray(data.actions) ? data.actions.filter((a) => a && typeof a.kind === 'string') : [];
  const blocking = data ? plainText(data.blocking, '') : '';
  // An action this page cannot offer exactly is named with its plain reason.
  const notOffered = data && Array.isArray(data.notOffered) ? data.notOffered.filter((a) => a && typeof a.kind === 'string') : [];
  const shown = Boolean(list.length || notOffered.length || blocking || acts.result || acts.message);
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
    const id = 'mv-action-reason';
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
    const id = 'mv-action-confirm-' + a.kind;
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

function amendText(a) {
  return el('p', { class: 'scope-note' }, a.committed
    ? 'Version ' + count(a.revision) + ' of the plan is recorded and waits for your approval. Nothing was approved.'
    : 'Not committed: the ledger read back shows no new version of this plan.');
}
function stopText(gate, live) {
  if (!gate.stop) return null;
  return el('p', { class: 'scope-note' }, live ? 'Stop requested. Not committed until the ledger no longer shows this Molecule running.' : 'Stopped: the ledger no longer shows this Molecule running.');
}

// renderPreviewInfo shows the preview once per preview, so its View diff
// button is not rebuilt by a poll.
function renderPreviewInfo(c, preview) {
  const key = preview && typeof preview.digest === 'string' ? preview.digest : '';
  fillKeyed(c.previewInfo, key, () => {
    const files = Array.isArray(preview.files) ? preview.files : [];
    const bytes = count(preview.diffBytes);
    return [
      el('p', { class: 'preview-line' }, 'Branch ', el('code', null, notRecorded(preview.branch)), ' into ', el('code', null, notRecorded(preview.baseBranch)), ' · ' + counted(files.length, 'file', 'files')),
      el('button', { type: 'button', class: 'text-button', dataset: { gate: 'diff' }, onclick: () => show('Delivery diff · ' + moleculeName(state.detail || {}), typeof preview.diff === 'string' ? preview.diff : '', (preview.diffTruncated ? 'Truncated · showing the first part of ' + wholeNumber.format(bytes) + ' bytes.' : 'Complete · ' + wholeNumber.format(bytes) + ' bytes.') + ' Secrets and local paths are redacted; the CLI shows the full diff.') }, 'View diff ↗'),
    ];
  });
}

// diffText is the measured size, "+412 −96 in 4 files"; a count stopped at a
// bound says it is a lower bound. Nothing measured shows nothing.
function diffText(diff) {
  if (!diff || diff.state !== 'measured' || !Number.isInteger(diff.files) || diff.files < 1) return '';
  const text = '+' + wholeNumber.format(count(diff.added)) + ' −' + wholeNumber.format(count(diff.removed)) + ' in ' + counted(diff.files, 'file', 'files') + (count(diff.binary) ? ' (' + counted(count(diff.binary), 'binary file', 'binary files') + ')' : '');
  return diff.truncated ? 'At least ' + text + ' · the count stopped at a safety bound' : text;
}

// estimateText is the history-based estimate with its basis, or nothing when
// the server hides it.
function estimateText(e) {
  if (!e || e.shown !== true || !e.time || typeof e.basis !== 'string' || !e.basis) return '';
  let text = 'Usually ' + rangeText(count(e.time.lowMs), count(e.time.highMs));
  const cost = e.cost && money(e.cost.medianUsd);
  if (cost) text += ' · about ' + cost + (e.cost.partial ? ' (' + plainText(e.cost.coverage, 'partial cost') + ')' : '');
  return text + ', ' + e.basis + '. ' + plainText(e.note, '');
}
function rangeText(low, high) {
  const a = minutesText(low), b = minutesText(high);
  if (a === b) return 'about ' + a;
  if (/^\d+ min$/.test(a) && /^\d+ min$/.test(b)) return a.slice(0, -4) + '–' + b;
  return a + '–' + b;
}

export function renderRoles(d) {
  const c = ensureMoleculeView();
  const v = d.view;
  $('#mv-roles').hidden = !v;
  if (!v) return;
  const r = v.roles || {};
  setText(c.roles.coding, [plainText(r.coding, ''), plainText(r.model, '')].filter(Boolean).join(' · ') || 'Not recorded');
  setText(c.roles.reviewer, typeof r.reviewer === 'string' && r.reviewer ? r.reviewer + ', read-only' : 'No review step');
  const p = v.planningCost, total = p && money(p.usd);
  setText(c.roles.cost, !total ? 'Not recorded' : p.partial ? total + ' · reported for ' + count(p.reported) + ' of ' + counted(count(p.attempts), 'attempt', 'attempts') : total);
}

// loadStats reads the diff size and the estimate once per Molecule, gate and
// plan; a response for another selection is dropped.
export async function loadStats(d) {
  const v = d.view, kind = v && v.decision && v.decision.kind;
  if (d.sample || (kind !== 'scope' && kind !== 'delivery')) { mv.statsKey = null; mv.stats = null; return; }
  const key = d.id + ':' + kind + ':' + ((v.plan && v.plan.digest) || '');
  if (key === mv.statsKey) return;
  mv.statsKey = key;
  mv.stats = null;
  const token = ++mv.statsToken;
  try {
    const res = await api('/api/molecules/' + encodeURIComponent(d.id) + '/stats');
    if (token !== mv.statsToken || !res.ok) return;
    const data = await res.json();
    if (token !== mv.statsToken) return;
    mv.stats = { id: d.id, data: data };
    if (state.detail && state.detail.id === d.id) renderDecision(state.detail);
  } catch (error) { failed(error); }
}
