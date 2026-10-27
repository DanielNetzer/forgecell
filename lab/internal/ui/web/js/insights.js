// ---- Insights: the Lab overview and analytics ----------------------------------
// The overview counts the live Molecules by lane and lists the ones waiting at a
// human gate; analytics show reported time and cost with their coverage.
import { $, el, fill, add, state, ctx, LANES, ALL_LANES, RETIRED, notRecorded, durationLabel, molecules, live } from './core.js';

function renderOverview() {
  const all = molecules().filter(live);
  const counts = (state.list && state.list.counts) || {};
  const sample = Boolean(state.list && state.list.sample);
  $('#aggregate .panel-head .mono').textContent = sample ? 'LAB OVERVIEW · SAMPLE DATA' : 'LAB OVERVIEW';
  $('#aggregate-count').textContent = 'All ' + all.length + ' Molecules';
  const waiting = all.filter((m) => m.lane === 'decision');
  fill($('#review-queue'), (waiting.length
    ? waiting.slice(0, 5).map((m) => el('button', { type: 'button', class: 'review-row', dataset: { review: m.id }, onclick: () => ctx.openMolecule(m.id) },
      el('span', { class: 'review-id' }, '#' + m.number), el('strong', null, m.title || m.id), el('span', null, notRecorded(m.phase)), el('span', { 'aria-hidden': 'true' }, '→')))
    : [el('p', { class: 'scope-note' }, 'No Molecules wait at a human gate.')]));
  if (waiting.length > 5) add($('#review-queue'), el('p', { class: 'scope-note' }, (waiting.length - 5) + ' more under Needs my decision.'));
  fill($('#outcome-bars'), Object.entries(LANES).map(([id, lane]) => {
    const n = counts[id] || 0;
    const bar = el('span', { class: 'bar ' + lane.cls });
    bar.style.width = (all.length ? (n / all.length) * 100 : 0) + '%';
    return el('div', { class: 'outcome-row' }, el('span', null, lane.label), el('div', { class: 'bar-track' }, bar), el('strong', null, String(n)));
  }), el('div', { class: 'outcome-row' }, el('span', null, 'Paused'), el('div', { class: 'bar-track' }), el('strong', { title: 'The engine has no pause semantics' }, 'n/a')));
  renderAnalytics();
}

// ---- Analytics -----------------------------------------------------------------
// A null measure is shown as text (Not recorded, Unknown, Unavailable), never as
// 0, and every total carries its coverage.
// Six decimals, like the engine's formatUSD, so a small reported cost never reads as $0.
function usd(value) { return typeof value === 'number' && Number.isFinite(value) ? '$' + value.toFixed(6) : null; }
function countText(value) { return typeof value === 'number' ? String(value) : 'Not recorded'; }
function measureText(m) {
  if (!m || typeof m.totalMs !== 'number') return m && m.overflow ? 'Unavailable (sum overflowed)' : 'Not recorded';
  return durationLabel(m.totalMs);
}
function measureCoverage(m) {
  return m ? 'recorded for ' + countText(m.recorded) + ' of ' + countText(m.molecules) + ' Molecules' : 'coverage not recorded';
}
function groupCost(g) {
  const total = usd(g.costUsd);
  const coverage = 'reported for ' + countText(g.costReported) + ' of ' + countText(g.attempts);
  if (total) return total + ' (' + notRecorded(g.costBasis) + ') · ' + coverage;
  return (g.costNotFinite ? 'Unavailable (sum not finite)' : 'Unknown') + ' · ' + coverage;
}

function renderAnalytics() {
  const box = $('#analytics');
  const a = state.analytics;
  if (!a) { fill(box, el('span', { class: 'label' }, 'ANALYTICS'), el('strong', null, 'Loading…')); return; }
  const t = a.time || {}, c = a.cost || {}, models = a.models || {}, counts = a.counts || {};
  const groups = Array.isArray(models.groups) ? models.groups : [];
  const total = usd(c.totalUsd);
  const measure = (label, m) => el('div', null, el('dt', null, label), el('dd', null, measureText(m) + ' · ' + measureCoverage(m)));
  // fill skips an absent note instead of rendering it as text.
  fill(box,
    el('span', { class: 'label' }, a.sample ? 'ANALYTICS · SAMPLE DATA' : 'ANALYTICS · REPORTED COST'),
    el('strong', null, total || (c.notFinite ? 'Unavailable (sum not finite)' : 'Unknown')),
    el('p', null, notRecorded(c.coverage) + (total ? ' · ' + notRecorded(c.basisLabel) : '') + '. Unknown is not zero.'),
    a.complete ? null : el('p', { class: 'scope-note' }, (a.truncated ? 'Inventory truncated by safety bounds' : 'Inventory incomplete') + '; skipped ledgers are not counted.'),
    el('dl', { class: 'atom-meta' },
      el('div', null, el('dt', null, 'Outcomes by lane'), el('dd', null, Object.entries(ALL_LANES).map(([id, lane]) => lane.label + ' ' + countText(id === RETIRED ? counts[id] || 0 : counts[id])).join(' · '))),
      measure('Wall-clock', t.wallClock),
      measure('Active Atom time', t.activeAtom),
      measure('Waiting time', t.waiting),
      el('div', null, el('dt', null, 'Lab elapsed'), el('dd', null, typeof t.elapsedMs === 'number' ? durationLabel(t.elapsedMs) : 'Not recorded'))),
    t.note ? el('p', { class: 'scope-note' }, t.note) : null,
    el('span', { class: 'label' }, 'HARNESS / MODEL · ' + countText(models.attempts) + ' ATTEMPTS'),
    groups.length
      ? el('ul', { class: 'record-list' }, groups.map((g) => el('li', null,
        el('strong', null, notRecorded(g.harness) + ' · ' + notRecorded(g.model)),
        el('span', null, countText(g.attempts) + (g.attempts === 1 ? ' attempt' : ' attempts')),
        el('span', null, 'cost ' + groupCost(g)))))
      : el('p', { class: 'scope-note' }, 'No harness attempts recorded.'),
    models.truncated ? el('p', { class: 'scope-note' }, countText(models.ungrouped) + ' attempts beyond the group bound are not shown; the breakdown is truncated.') : null);
}

// Insights mounts on its data-view-panel; analytics arrive on their own, so the
// entry renders them through ctx.renderAnalytics.
export function mount(root, ctx) {
  ctx.renderAnalytics = renderAnalytics;
}

// update renders the Lab overview, analytics included, from the list.
export function update(state) { renderOverview(); }
