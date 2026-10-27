// ---- Molecule view: the Timeline tab ------------------------------------------------
// The Atom list, evidence and logs, as before: the origin links, the totals,
// the Formula snapshot, each Atom's record, the readiness revisions and the
// harness attempts.
import { $, el, fill, state, ctx, ISSUE_URL, PULL_URL, externalLink, notRecorded, short, durationLabel, elapsed, api, failed, show } from './core.js';
import { mv } from './molecule-decision.js';

function attemptsById(d) { return new Map((d.attempts || []).map((a) => [a.id, a])); }
function costText(usage) {
  if (!usage || typeof usage.totalCostUsd !== 'number') return 'Not recorded';
  const basis = usage.costBasis === 'list' ? 'CLI list-price estimate' : usage.costBasis ? 'basis ' + usage.costBasis : 'basis unavailable';
  return '$' + usage.totalCostUsd.toFixed(4) + ' (' + basis + ')';
}

// renderTimeline is the Atom list, evidence and logs. While focus is inside it,
// a poll does not rebuild it; it is brought up to date once focus leaves.
export function renderTimeline(d) {
  const panel = $('#mv-timeline');
  if (panel.contains(document.activeElement) && mv.timelineId === d.id) { mv.timelineDirty = true; return; }
  mv.timelineDirty = false;
  mv.timelineId = d.id;
  const issueLink = externalLink(d.issue.url, ISSUE_URL, 'Issue #' + d.issue.number + ' ↗');
  const pullLink = d.delivery ? externalLink(d.delivery.url, PULL_URL, 'Pull request ↗') : null;
  fill($('#run-origin'),
    el('span', null, notRecorded(d.issue.repository) + ' #' + d.issue.number + (d.issue.state ? ' · ' + d.issue.state : '')),
    issueLink || el('span', { class: 'muted' }, 'Issue link unavailable'),
    pullLink || el('span', { class: 'muted' }, d.delivery ? 'PR link unavailable' : 'PR · not created'),
    el('button', { type: 'button', class: 'text-button', dataset: { origin: 'issue-text' }, onclick: () => show('Issue text · ' + d.id, d.issue.body || 'No Issue text recorded.') }, 'Issue text ↗'),
    d.sample ? null : el('button', { type: 'button', class: 'text-button', dataset: { origin: 'ledger' }, onclick: () => showLedger(d.id) }, 'Ledger record ↗'),
    el('button', { type: 'button', class: 'text-button', id: 'clear-selection', dataset: { origin: 'clear' }, onclick: ctx.overview }, 'Clear selection'));
  if (issueLink) issueLink.dataset.origin = 'issue';
  if (pullLink) pullLink.dataset.origin = 'pull';

  const wall = elapsed(d.startedAt, d.finishedAt);
  const attempts = d.attempts || [];
  const costs = attempts.filter((a) => a.usage && typeof a.usage.totalCostUsd === 'number');
  fill($('#molecule-totals'),
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
  fill(diagnostics, (d.diagnostics || []).map((text) => el('li', null, text)));
  diagnostics.hidden = !(d.diagnostics || []).length;
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
    fill(box, el('p', { class: 'scope-note' }, 'No Atoms are recorded for this Molecule.'));
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
  fill(box, flow, atomDetail(d, atoms[state.atomIndex], state.atomIndex));
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
  if (!r) { fill(box, el('p', { class: 'scope-note' }, 'No readiness state recorded.')); return; }
  fill(box,
    el('p', { class: 'scope-note' }, 'Phase ' + notRecorded(r.phase)),
    el('ul', { class: 'record-list' }, (r.plans || []).map((p) => el('li', null, el('strong', null, 'Revision ' + p.revision), el('code', { title: p.digest }, 'digest ' + short(p.digest)), el('code', { title: p.parentDigest || '' }, p.parentDigest ? 'parent ' + short(p.parentDigest) : 'no parent')))),
    el('ul', { class: 'record-list events' }, (r.events || []).map((e) => el('li', null, el('span', { class: 'mono-text' }, notRecorded(e.at)), el('strong', null, notRecorded(e.kind)), e.detail ? el('span', null, e.detail) : null))));
}

function renderAttempts(d) {
  const attempts = d.attempts || [];
  fill($('#attempts'), attempts.length
    ? el('ul', { class: 'record-list' }, attempts.map((a) => {
      const u = a.usage, t = (u && u.total) || {};
      const tokens = u ? 'tokens in ' + notRecorded(t.input) + ' · out ' + notRecorded(t.output) : 'usage unavailable';
      return el('li', null, el('strong', null, a.kind + ' ' + a.id), el('span', null, notRecorded(a.status)), el('span', null, 'model ' + notRecorded(a.model || (u && (u.effectiveModels || []).join(', ')))), el('span', null, tokens), el('span', null, 'cost ' + costText(u)));
    }))
    : el('p', { class: 'scope-note' }, 'No harness attempts recorded.'));
}

// mountTimeline wires the Formula snapshot's View YAML button.
export function mountTimeline() {
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
}
