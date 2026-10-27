// ---- Insights: the Lab overview and analytics ----------------------------------
// The overview counts the live Molecules by lane and lists the ones waiting at a
// human gate; analytics show, top to bottom, the summary tiles, how each model
// performs in each Atom role, a weekly chart and where the time goes. Every
// value comes from GET /api/analytics; a missing one reads "not reported",
// never 0, and every total carries its coverage.
import { $, el, fill, add, state, ctx, LANES, ALL_LANES, RETIRED, notRecorded, molecules, live, money, wholeNumber, plural, hoursText, queryParam, setQueryParam, keepFocus } from './core.js';

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

// ---- Readable values -------------------------------------------------------------
// Money goes through core.js money() (Intl, two decimals, never $0 for a nonzero
// amount), durations through hoursText (hours and minutes), counts through Intl.
const NOT_REPORTED = 'not reported';
const percent = new Intl.NumberFormat('en-US', { style: 'percent', maximumFractionDigits: 0 });
const oneDecimal = new Intl.NumberFormat('en-US', { maximumFractionDigits: 1 });
const isCount = (n) => Number.isInteger(n) && n >= 0;
function num(n) { return typeof n === 'number' && Number.isFinite(n) ? n : null; }
function countText(n) { return isCount(n) ? wholeNumber.format(n) : 'Not recorded'; }
function words(n, one, many) { return isCount(n) ? wholeNumber.format(n) + ' ' + (plural.select(n) === 'one' ? one : many) : 'Not recorded'; }
function usdText(value, notFinite) { return money(num(value)) || (notFinite ? 'unavailable (sum not finite)' : NOT_REPORTED); }
function tokenText(value, overflow) { return num(value) !== null && value >= 0 ? wholeNumber.format(value) : overflow ? 'unavailable (sum overflowed)' : NOT_REPORTED; }
function timeText(ms) { return hoursText(num(ms)) || NOT_REPORTED; }
function shareText(value) { return num(value) !== null && value >= 0 && value <= 1 ? percent.format(value) : NOT_REPORTED; }
function coverage(n, m, what) { return 'reported for ' + countText(n) + ' of ' + words(m, what, what + 's'); }
const MODEL_STATES = { recorded: '', unknown: 'Unknown', 'not-recorded': 'Not recorded' };
function modelLabel(r) { return MODEL_STATES[r.modelState] || notRecorded(r.model); }
const ROLES = { analysis: 'Analysis', coding: 'Coding', review: 'Review', meta: 'Meta' };
function roleLabel(role) { return ROLES[role] || 'Other'; }

// ---- Sort: allowlisted keys in ?view=insights&sort=… -------------------------------
// value reads one row's sort value; null sorts last in either direction. Ties
// fall back to role, then model, then the server's order, so the sort is stable.
const ROLE_ORDER = { analysis: 0, coding: 1, review: 2 };
const SORTS = {
  model: { label: 'Model', text: true, value: (r) => modelLabel(r).toLowerCase() },
  role: { label: 'Role', text: true, value: (r) => ROLE_ORDER[r.role] ?? 9 },
  attempts: { label: 'Attempts', value: (r) => num(r.attempts) },
  molecules: { label: 'Molecules', value: (r) => num(r.molecules) },
  cost: { label: 'Reported cost', value: (r) => num(r.costUsd) },
  median: { label: 'Median cost', value: (r) => num(r.medianCostUsd) },
  in: { label: 'Tokens in', value: (r) => num(r.tokensIn) },
  out: { label: 'Tokens out', value: (r) => num(r.tokensOut) },
  cache: { label: 'Cache read', value: (r) => num(r.tokensCacheRead) },
  time: { label: 'Median time', value: (r) => num(r.medianWallMs) },
  outcome: { label: 'Outcome', value: (r) => num(r.passRate) ?? (r.findings ? num(r.findings.high) : null) ?? num(r.questionsPerMolecule) },
};
const DEFAULT_SORT = 'role';
const SORT_VALUE = /^([a-z]{2,9})(?:-(asc|desc))?$/;
function validSort(value) { const m = SORT_VALUE.exec(value); return Boolean(m && Object.hasOwn(SORTS, m[1])); }
function currentSort() {
  const m = SORT_VALUE.exec(queryParam('sort', validSort) || DEFAULT_SORT);
  return { key: m[1], dir: m[2] || (SORTS[m[1]].text ? 'asc' : 'desc') };
}
function sortValue(key, dir) { return key + '-' + dir; }
function sortRows(rows, sort) {
  const s = SORTS[sort.key], sign = sort.dir === 'asc' ? 1 : -1;
  const cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  return rows.map((r, i) => ({ r, i, v: s.value(r) })).sort((a, b) => {
    if (a.v !== b.v) {
      if (a.v === null) return 1;
      if (b.v === null) return -1;
      const c = sign * cmp(a.v, b.v);
      if (c) return c;
    }
    return cmp(ROLE_ORDER[a.r.role] ?? 9, ROLE_ORDER[b.r.role] ?? 9) || cmp(modelLabel(a.r), modelLabel(b.r)) || a.i - b.i;
  }).map((x) => x.r);
}
function chooseSort(key) {
  const now = currentSort();
  const dir = now.key === key ? (now.dir === 'asc' ? 'desc' : 'asc') : (SORTS[key].text ? 'asc' : 'desc');
  setQueryParam('sort', key === DEFAULT_SORT && dir === 'asc' ? null : sortValue(key, dir));
  keepFocus(renderAnalytics);
}

// ---- Sections ----------------------------------------------------------------------
function tile(label, value, note) {
  return el('div', { class: 'stat-tile' }, el('h3', { class: 'stat-label' }, label), el('p', { class: 'stat-value' }, value), el('p', { class: 'stat-note' }, note));
}
function facts(pairs) {
  return el('dl', { class: 'insight-facts' }, pairs.filter(Boolean).map(([dt, dd]) => el('div', null, el('dt', null, dt), el('dd', null, dd))));
}

function summarySection(a) {
  const s = a.summary || {}, week = s.costThisWeek || {}, total = s.costTotal || {}, first = s.firstTryGreen || {}, draft = s.timeToDraftPr || {}, plan = a.planning || {}, planCost = plan.cost || {}, counts = a.counts || {};
  return el('section', { class: 'insight-section', 'aria-labelledby': 'insight-summary-title' },
    el('h2', { id: 'insight-summary-title', class: 'insight-title' }, 'At a glance'),
    el('div', { class: 'stat-tiles' },
      tile('Reported cost this week', usdText(week.totalUsd, week.notFinite),
        coverage(week.reported, week.attempts, 'attempt') + ' this week · ' + usdText(total.totalUsd, total.notFinite) + ' in total, ' + coverage(total.reported, total.attempts, 'attempt')),
      tile('Molecules finished', countText(s.moleculesFinished), 'Draft published. Merged and deployed are separate outcomes.'),
      tile('Median time to draft PR', 'Not reported', notRecorded(draft.reason)),
      tile('First-try green', shareText(first.value),
        'Checks passed on the first coding attempt for ' + countText(first.count) + ' of ' + words(first.of, 'Molecule', 'Molecules') + ' whose first attempt was checked' +
        (first.unchecked ? ' · ' + words(first.unchecked, 'Molecule', 'Molecules') + ' left out: first attempt not checked' : ''))),
    facts([
      ['Clarifying questions per Molecule', num(plan.questionsPerMolecule) !== null ? oneDecimal.format(plan.questionsPerMolecule) + ' · ' + words(plan.questions, 'question', 'questions') + ' from ' + countText(plan.molecules) + ' of ' + words(plan.of, 'planned Molecule', 'planned Molecules') : NOT_REPORTED],
      ['Planning (analysis) cost', usdText(planCost.totalUsd, planCost.notFinite) + ' · ' + coverage(planCost.reported, planCost.attempts, 'analysis attempt')],
      ['Outcomes by lane', Object.entries(ALL_LANES).map(([id, lane]) => lane.label + ' ' + countText(id === RETIRED ? counts[id] || 0 : counts[id])).join(' · ')],
    ]));
}

function outcomeText(r) {
  if (r.role === 'coding') return r.checksRan ? shareText(r.passRate) + ' checks passed · ' + countText(r.checksPassed) + ' of ' + words(r.checksRan, 'checked attempt', 'checked attempts') : NOT_REPORTED;
  if (r.role === 'review') return r.findings ? countText(r.findings.high) + ' high · ' + countText(r.findings.medium) + ' medium · ' + countText(r.findings.low) + ' low, from ' + words(r.reviewsReported, 'pass', 'passes') : NOT_REPORTED;
  if (r.role === 'analysis') return num(r.questionsPerMolecule) !== null ? oneDecimal.format(r.questionsPerMolecule) + ' clarifying questions per Molecule · ' + words(r.questions, 'question', 'questions') : NOT_REPORTED;
  return NOT_REPORTED;
}

function byModelSection(a) {
  const by = a.byModel || {}, sort = currentSort();
  const rows = Array.isArray(by.rows) ? by.rows : [];
  const head = (key) => {
    const on = sort.key === key;
    return el('th', { scope: 'col', 'aria-sort': on ? (sort.dir === 'asc' ? 'ascending' : 'descending') : 'none' },
      el('button', { type: 'button', class: 'sort-button', id: 'sort-' + key, onclick: () => chooseSort(key) }, SORTS[key].label, el('span', { class: 'sort-mark', 'aria-hidden': 'true' }, on ? (sort.dir === 'asc' ? '↑' : '↓') : '')));
  };
  const cell = (main, sub) => el('td', null, el('span', { class: 'cell-main' }, main), sub ? el('span', { class: 'cell-sub' }, sub) : null);
  const body = sortRows(rows, sort).map((r) => el('tr', null,
    el('th', { scope: 'row', class: 'model-name' }, modelLabel(r)),
    el('td', null, roleLabel(r.role)),
    cell(countText(r.attempts)),
    cell(countText(r.molecules)),
    cell(usdText(r.costUsd, r.costNotFinite), coverage(r.costReported, r.attempts, 'attempt')),
    cell(usdText(r.medianCostUsd), 'per attempt'),
    cell(tokenText(r.tokensIn, r.tokenOverflow), coverage(r.usageReported, r.attempts, 'attempt')),
    cell(tokenText(r.tokensOut, r.tokenOverflow)),
    cell(tokenText(r.tokensCacheRead, r.tokenOverflow)),
    cell(timeText(r.medianWallMs), r.medianBounded ? 'median beyond the sample bound' : coverage(r.wallReported, r.attempts, 'attempt')),
    cell(outcomeText(r))));
  const meta = by.meta || {};
  body.push(el('tr', { class: 'meta-row' }, el('th', { scope: 'row', class: 'model-name' }, 'Any model'), el('td', null, roleLabel('meta')),
    el('td', { colspan: '9' }, NOT_REPORTED + ' · ' + notRecorded(meta.reason))));
  return el('section', { class: 'insight-section', 'aria-labelledby': 'insight-models-title' },
    el('h2', { id: 'insight-models-title', class: 'insight-title' }, 'By model'),
    el('p', { class: 'scope-note' }, words(by.attempts, 'attempt', 'attempts') + ' in total. ' + notRecorded(by.note)),
    el('div', { class: 'table-scroll', role: 'region', 'aria-labelledby': 'insight-models-title', tabindex: '0' },
      el('table', { class: 'model-table' },
        el('caption', { class: 'sr-only' }, 'Each effective model in each Atom role, sorted by ' + SORTS[sort.key].label + (sort.dir === 'asc' ? ', ascending' : ', descending')),
        el('thead', null, el('tr', null, Object.keys(SORTS).map(head))),
        el('tbody', null, body))),
    rows.length ? null : el('p', { class: 'scope-note' }, 'No harness attempts recorded.'),
    by.truncated ? el('p', { class: 'scope-note' }, words(by.ungrouped, 'attempt', 'attempts') + ' beyond the row bound are not shown; the table is truncated.') : null);
}

// ---- Weekly chart: inline SVG with a text table for screen readers ---------------
const SVG_NS = 'http://www.w3.org/2000/svg';
function svg(tag, attrs, ...children) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const [key, value] of Object.entries(attrs || {})) node.setAttribute(key, String(value));
  return add(node, ...children);
}
const WEEK = /^\d{4}-\d{2}-\d{2}$/;
const weekDay = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' });
function weekText(start) { return typeof start === 'string' && WEEK.test(start) && Number.isFinite(Date.parse(start)) ? 'Week of ' + weekDay.format(Date.parse(start)) : 'Week not recorded'; }

// stackChart draws one bar per week, stacked by series; values(week) gives the
// per-series amounts, null or absent read as nothing to draw.
function stackChart(title, weeks, series, values) {
  const W = 320, H = 120, pad = 4, base = H - 16;
  const totals = weeks.map((w) => values(w).reduce((s, v) => s + (num(v) !== null && v > 0 ? v : 0), 0));
  const top = Math.max(0, ...totals);
  if (!(top > 0)) return el('p', { class: 'scope-note' }, title + ': nothing reported in these weeks.');
  const slot = (W - 2 * pad) / weeks.length, bar = Math.max(2, slot * 0.7);
  const marks = [];
  weeks.forEach((w, i) => {
    let y = base;
    values(w).forEach((v, k) => {
      if (!(num(v) !== null && v > 0)) return;
      const h = (v / top) * (base - pad);
      y -= h;
      marks.push(svg('rect', { class: 'series-' + Math.min(k, 6), x: (pad + i * slot + (slot - bar) / 2).toFixed(2), y: y.toFixed(2), width: bar.toFixed(2), height: h.toFixed(2) }));
    });
  });
  return el('figure', { class: 'chart' },
    el('figcaption', { class: 'chart-title' }, title),
    svg('svg', { viewBox: '0 0 ' + W + ' ' + H, class: 'chart-svg', 'aria-hidden': 'true', focusable: 'false' },
      svg('line', { class: 'chart-axis', x1: pad, x2: W - pad, y1: base, y2: base }), marks,
      svg('text', { class: 'chart-label', x: pad, y: H - 3 }, weekText(weeks[0].start).replace('Week of ', '')),
      svg('text', { class: 'chart-label', x: W - pad, y: H - 3, 'text-anchor': 'end' }, weekText(weeks[weeks.length - 1].start).replace('Week of ', ''))));
}

function weeklySection(a) {
  const w = a.weekly || {};
  const weeks = Array.isArray(w.weeks) ? w.weeks : [], series = Array.isArray(w.series) ? w.series.map((s) => notRecorded(s)) : [];
  const breakdown = (list, fmt) => series.map((s, k) => [s, list && list[k]]).filter(([, v]) => num(v) !== null && v > 0).map(([s, v]) => s + ' ' + fmt(v)).join(', ');
  const section = el('section', { class: 'insight-section', 'aria-labelledby': 'insight-weekly-title' },
    el('h2', { id: 'insight-weekly-title', class: 'insight-title' }, 'Over time'));
  if (!weeks.length) return add(section, el('p', { class: 'scope-note' }, 'No dated cost or finished Molecule is recorded yet.'));
  return add(section,
    el('div', { class: 'charts' },
      stackChart('Reported cost per week', weeks, series, (wk) => (Array.isArray(wk.cost) ? wk.cost : [])),
      stackChart('Molecules finished per week', weeks, series, (wk) => (Array.isArray(wk.finished) ? wk.finished : []))),
    el('ul', { class: 'chart-legend', 'aria-hidden': 'true' }, series.map((s, k) => el('li', null, el('span', { class: 'swatch series-' + Math.min(k, 6) }), s))),
    el('table', { class: 'sr-only' },
      el('caption', null, 'Reported cost and Molecules finished per week, by model'),
      el('thead', null, el('tr', null, el('th', { scope: 'col' }, 'Week'), el('th', { scope: 'col' }, 'Reported cost'), el('th', { scope: 'col' }, 'Molecules finished'))),
      el('tbody', null, weeks.map((wk) => el('tr', null,
        el('th', { scope: 'row' }, weekText(wk.start)),
        el('td', null, usdText(wk.costTotal, wk.costNotFinite) + (breakdown(wk.cost, money) ? ': ' + breakdown(wk.cost, money) : '')),
        el('td', null, countText(wk.finishedTotal) + (breakdown(wk.finished, String) ? ': ' + breakdown(wk.finished, String) : '')))))),
    el('p', { class: 'scope-note' }, notRecorded(w.note) +
      (w.truncated ? ' Older weeks are left out.' : '') + (w.folded ? ' Models past the sixth are summed as Other models.' : '') +
      (w.undatedCost ? ' ' + words(w.undatedCost, 'cost has', 'costs have') + ' no recorded start and no week.' : '') +
      (w.undatedFinished ? ' ' + words(w.undatedFinished, 'finished Molecule has', 'finished Molecules have') + ' no recorded finish and no week.' : '')));
}

// ---- Where time goes: one bar with labelled segments --------------------------------
function timeSection(a) {
  const t = a.timeSplit || {};
  const parts = [['Active Atom time', t.active, 'time-active'], ['Waiting for you', t.waitingForYou, 'time-you'], ['Waiting on checks', t.waitingOnChecks, 'time-checks']];
  const total = parts.reduce((s, [, m]) => s + (m && num(m.totalMs) !== null ? m.totalMs : 0), 0);
  const cov = t.active || {};
  const section = el('section', { class: 'insight-section', 'aria-labelledby': 'insight-time-title' },
    el('h2', { id: 'insight-time-title', class: 'insight-title' }, 'Where time goes'));
  if (!(total > 0)) return add(section, el('p', { class: 'scope-note' }, 'Time split ' + NOT_REPORTED + ' · recorded for ' + countText(cov.recorded) + ' of ' + words(cov.molecules, 'Molecule', 'Molecules') + '.'));
  const bar = el('div', { class: 'time-bar', 'aria-hidden': 'true' }, parts.map(([label, m, cls]) => {
    const ms = m && num(m.totalMs) !== null ? m.totalMs : 0;
    const seg = el('span', { class: 'time-seg ' + cls, title: label }, ms / total >= 0.12 ? label : '');
    seg.style.flexGrow = String(ms / total);
    return seg;
  }));
  return add(section, bar,
    facts(parts.map(([label, m, cls]) => [el('span', null, el('span', { class: 'swatch ' + cls, 'aria-hidden': 'true' }), label),
      m && m.overflow ? 'unavailable (sum overflowed)' : timeText(m && m.totalMs) + (m && num(m.totalMs) !== null ? ' · ' + percent.format(m.totalMs / total) : '')])),
    el('p', { class: 'scope-note' }, 'Recorded for ' + countText(cov.recorded) + ' of ' + words(cov.molecules, 'Molecule', 'Molecules') + '. ' + notRecorded(t.note)));
}

// renderAnalytics rebuilds only when the analytics or the sort changed, so a poll
// never rebuilds the table under the user.
let shownKey = null;
function renderAnalytics() {
  const box = $('#analytics');
  const a = state.analytics;
  if (!a) { shownKey = null; fill(box, el('span', { class: 'label' }, 'ANALYTICS'), el('strong', null, 'Loading…')); return; }
  const sort = currentSort();
  const key = String(a.etag) + '|' + sortValue(sort.key, sort.dir);
  if (key === shownKey && box.childElementCount) return;
  shownKey = key;
  fill(box,
    a.sample ? el('p', { class: 'scope-note' }, 'Sample data: no model, cost or time is recorded for sample Molecules.') : null,
    a.complete ? null : el('p', { class: 'scope-note' }, (a.truncated ? 'Inventory truncated by safety bounds' : 'Inventory incomplete') + '; skipped ledgers are not counted.'),
    summarySection(a),
    byModelSection(a),
    weeklySection(a),
    timeSection(a),
    el('p', { class: 'scope-note' }, 'Every value comes from this Lab\'s ledgers, and only costs the harness reported are summed. Unknown is not zero.'));
}

// Insights mounts on its data-view-panel; analytics arrive on their own, so the
// entry renders them through ctx.renderAnalytics. Back and Forward restore the
// sort from the URL.
export function mount(root, ctx) {
  ctx.renderAnalytics = () => keepFocus(renderAnalytics);
  window.addEventListener('popstate', () => keepFocus(renderAnalytics));
}

// update renders the Lab overview, analytics included, from the list.
export function update(state) { renderOverview(); }
