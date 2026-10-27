// ---- Work: grid, filters and search -------------------------------------------
// The contribution-style Molecule grid under Work, its lane bar, search and
// tooltip. A cell opens the Molecule view, which ctx.openMolecule owns.
import { $, el, fill, add, state, LANES, RETIRED, notRecorded, molecules, laneOf, live, laneLabel, cells } from './core.js';

// The lane bar. Paused is not a filter: the engine has no pause semantics.
const FILTERS = [
  ['all', 'All'], ['running', 'Running'], ['decision', 'Needs my decision'],
  ['blocked', 'Blocked'], ['finished', 'Finished · Draft published'], ['other', 'Other'], ['retired', 'Retired'],
];

// AXIS_COLUMNS is the narrowest grid, in columns, whose width fits the axis labels.
const AXIS_COLUMNS = 8;

function statusLabel(m) {
  return laneLabel(m) + ' · ' + notRecorded(m.status) + (m.phase ? ' · ' + m.phase : '');
}

function matches(m) {
  const inLane = state.filter === 'all' ? live(m) : m.lane === state.filter;
  const terms = state.query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  const haystack = ('#' + m.number + ' ' + (m.title || '') + ' ' + (m.formulaId || '') + ' ' + m.id).toLowerCase();
  return inLane && terms.every((term) => haystack.includes(term));
}

function renderGrid() {
  const list = state.list;
  const all = molecules();
  const counts = list.counts || {};
  const sample = Boolean(list.sample);
  $('#shelf-count').textContent = (sample ? 'Sample data · ' : '') + all.filter(live).length + ' Molecules · ' +
    Object.entries(LANES).map(([id, lane]) => (counts[id] || 0) + ' ' + lane.short).join(' · ') +
    (counts[RETIRED] ? ' · ' + counts[RETIRED] + ' retired (not counted)' : '');
  renderNote(list);
  const cubes = $('#cubes');
  fill(cubes, all.map((m, i) => {
    const lane = laneOf(m);
    const button = el('button', {
      type: 'button', class: 'cube-button ' + lane.cls, dataset: { id: m.id },
      'aria-pressed': String(m.id === state.selectedId),
      'aria-label': 'Molecule ' + m.id + ': #' + m.number + ' ' + (m.title || 'untitled') + ', ' + statusLabel(m),
      tabindex: '-1',
    }, el('span', { class: 'micro-cube', 'aria-hidden': 'true' }, lane.symbol));
    button.style.setProperty('--delay', '-' + ((i % 9) * 0.47).toFixed(2) + 's');
    return button;
  }));
  renderFilters();
  applyFilter();
}

// The note is a status region: it is rebuilt, and so announced, only when its
// content changes, and an open Diagnostics list stays open.
function renderNote(list) {
  const diagnostics = list.complete ? [] : (list.diagnostics || []).slice(0, 50);
  const key = JSON.stringify([Boolean(list.complete), Boolean(list.truncated), (list.diagnostics || []).length, diagnostics]);
  if (key === state.noteKey) return;
  state.noteKey = key;
  const note = $('#inventory-note');
  const wasOpen = Boolean(note.querySelector('details[open]'));
  fill(note);
  if (!list.complete) {
    const count = (list.diagnostics || []).length;
    add(note, el('strong', null, list.truncated ? 'Inventory truncated by safety bounds. ' : 'Inventory incomplete. '),
      count + ' ledger problem' + (count === 1 ? '' : 's') + ' reported; those Molecules are not shown and are not counted.',
      count ? el('details', { open: wasOpen }, el('summary', null, 'Diagnostics'), el('ul', null, diagnostics.map((d) => el('li', null, d)))) : null);
  }
  note.hidden = Boolean(list.complete);
}

// Each lane is a segment of one bar: its cube-legend dot, label and count.
// Retired sits last, after a hairline divider.
function renderFilters() {
  const counts = (state.list && state.list.counts) || {};
  const bar = $('#run-filters');
  const left = bar.scrollLeft;
  fill($('#run-filters'), FILTERS.flatMap(([id, label]) => [
    id === RETIRED ? el('span', { class: 'lane-divider', 'aria-hidden': 'true' }) : null,
    el('button', {
      type: 'button', class: 'lane-segment', dataset: { filter: id }, 'aria-pressed': String(state.filter === id),
      onclick: () => { hideTip(); state.filter = id; renderFilters(); applyFilter(); $('[data-filter="' + id + '"]').focus({ preventScroll: true }); },
    }, id === 'all' ? null : el('span', { class: 'lane-dot lane-dot-' + id, 'aria-hidden': 'true' }),
    el('span', { class: 'lane-name' }, label),
    el('span', { class: 'lane-count' }, String(id === 'all' ? counts.all || 0 : counts[id] || 0))),
  ]));
  keepLaneStart(bar, left);
}

// keepLaneStart restores the bar's scroll after a rebuild, then scrolls only as
// far as needed to show the pressed segment whole, so the bar never opens on a
// clipped first segment.
function keepLaneStart(bar, left) {
  bar.scrollLeft = left;
  const on = bar.querySelector('[aria-pressed="true"]');
  if (!on) return;
  const box = bar.getBoundingClientRect(), seg = on.getBoundingClientRect(), pad = 4;
  if (seg.left < box.left + pad) bar.scrollLeft -= box.left + pad - seg.left;
  else if (seg.right > box.right - pad) bar.scrollLeft += seg.right - (box.right - pad);
}

function applyFilter() {
  const byId = new Map(molecules().map((m) => [m.id, m]));
  const all = cells();
  all.forEach((b) => { b.hidden = !matches(byId.get(b.dataset.id)); b.tabIndex = -1; });
  const shown = all.filter((b) => !b.hidden);
  const chosen = shown.find((b) => b.dataset.id === state.selectedId) || shown[0];
  if (chosen) chosen.tabIndex = 0;
  // The grid has as many rows as it shows cells, up to seven, and the axis labels
  // show only once the grid is wide enough to carry them.
  $('#cubes').style.setProperty('--rows', String(Math.max(1, Math.min(7, shown.length))));
  $('.grid-axis').hidden = Math.ceil(shown.length / 7) < AXIS_COLUMNS;
  const empty = $('#filter-empty');
  empty.hidden = shown.length > 0;
  empty.textContent = molecules().length === 0 ? 'No Molecules are recorded in this Lab yet.' : state.query.trim() ? 'No matching Molecules in this filter. Clear search or choose All.' : 'No Molecules in this lane.';
  $('#clear-search').hidden = !state.query;
  const label = (FILTERS.find(([id]) => id === state.filter) || FILTERS[0])[1];
  $('#search-status').textContent = state.query.trim() ? shown.length + ' matching ' + (shown.length === 1 ? 'Molecule' : 'Molecules') + ' · ' + (state.filter === 'all' ? 'all lanes' : label) : '';
  const outside = $('#outside-filter');
  const selected = state.selectedId && byId.get(state.selectedId);
  outside.hidden = !(state.selectedId && !$('#selected').hidden && !(selected && matches(selected)));
  outside.textContent = selected ? 'The selected Molecule ' + selected.id + ' is outside this filter; its details stay open below.' : 'The selected Molecule is no longer in the inventory.';
}

function clearSearch() { state.query = ''; $('#molecule-search').value = ''; applyFilter(); $('#molecule-search').focus({ preventScroll: true }); }

// ---- Tooltip --------------------------------------------------------------
let tipTimer = null, tipAnchor = null;
const tip = $('#cube-tooltip');
function hideTip() { clearTimeout(tipTimer); tip.hidden = true; if (tipAnchor) tipAnchor.removeAttribute('aria-describedby'); tipAnchor = null; }
function queueHideTip() { clearTimeout(tipTimer); tipTimer = setTimeout(hideTip, 180); }
function showTip(button) {
  const m = molecules().find((x) => x.id === button.dataset.id);
  if (!m) return;
  clearTimeout(tipTimer);
  if (tipAnchor) tipAnchor.removeAttribute('aria-describedby');
  tipAnchor = button;
  fill(tip,
    el('span', { class: 'label' }, 'MOLECULE #' + m.number + ' · ' + m.id),
    el('strong', null, m.title || 'Untitled'),
    el('dl', null, el('dt', null, 'Current Atom'), el('dd', null, m.currentAtom || 'None'), el('dt', null, 'Lane'), el('dd', null, laneOf(m).label), el('dt', null, 'Status'), el('dd', null, notRecorded(m.status) + (m.phase ? ' · ' + m.phase : ''))));
  tip.hidden = false;
  button.setAttribute('aria-describedby', 'cube-tooltip');
  const box = button.getBoundingClientRect();
  const width = tip.offsetWidth;
  tip.style.left = Math.max(12, Math.min(innerWidth - width - 12, box.left + box.width / 2 - width / 2)) + 'px';
  tip.style.top = (box.top > tip.offsetHeight + 16 ? box.top - tip.offsetHeight - 8 : box.bottom + 8) + 'px';
}

// Work mounts on its data-view-panel: the search, grid and tooltip listeners,
// the Motion checkbox and the offscreen pause.
export function mount(root, ctx) {
  ctx.hideTip = hideTip;
  ctx.applyFilter = applyFilter;

  $('#molecule-search').addEventListener('input', (e) => { hideTip(); state.query = e.target.value; applyFilter(); });
  $('#clear-search').addEventListener('click', clearSearch);
  $('#molecule-search').addEventListener('keydown', (e) => { if (e.key === 'Escape' && state.query) { e.preventDefault(); clearSearch(); } });

  const cubes = $('#cubes');
  cubes.addEventListener('click', (e) => {
    const button = e.target.closest('[data-id]');
    // A cell opens the Molecule view through history.pushState, so Back returns
    // to the grid.
    if (button) { hideTip(); ctx.openMolecule(button.dataset.id); }
  });
  // Seven-row, column-major movement among visible cells, as in the prototype.
  cubes.addEventListener('keydown', (e) => {
    const button = e.target.closest('[data-id]');
    if (!button) return;
    const shown = cells().filter((b) => !b.hidden);
    const at = shown.indexOf(button);
    const delta = { ArrowRight: 7, ArrowLeft: -7, ArrowDown: 1, ArrowUp: -1 }[e.key];
    let next = null;
    if (delta !== undefined) next = shown[Math.max(0, Math.min(shown.length - 1, at + delta))];
    if (e.key === 'Home') next = shown[0];
    if (e.key === 'End') next = shown[shown.length - 1];
    if (next) {
      e.preventDefault();
      button.tabIndex = -1;
      next.tabIndex = 0;
      next.focus();
    }
  });
  cubes.addEventListener('pointerover', (e) => { const b = e.target.closest('[data-id]'); if (b) showTip(b); });
  cubes.addEventListener('pointerout', (e) => { if (e.target.closest('[data-id]')) queueHideTip(); });
  cubes.addEventListener('focusin', (e) => { const b = e.target.closest('[data-id]'); if (b) showTip(b); });
  cubes.addEventListener('focusout', hideTip);

  tip.addEventListener('pointerenter', () => clearTimeout(tipTimer));
  tip.addEventListener('pointerleave', hideTip);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideTip(); });
  window.addEventListener('resize', hideTip);
  document.addEventListener('scroll', hideTip, true);

  $('#motion-toggle').addEventListener('change', (e) => document.body.classList.toggle('motion-paused', !e.target.checked));
  if ('IntersectionObserver' in window) {
    new IntersectionObserver(([entry]) => document.body.classList.toggle('shelf-offscreen', !entry.isIntersecting)).observe($('.shelf'));
  }
}

// update renders the grid, its count, the inventory note and the lane bar from
// the list.
export function update(state) { renderGrid(); }
