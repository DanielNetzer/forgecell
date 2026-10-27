// ---- Molecule view ---------------------------------------------------------------
// The Molecule view (?view=work&m=…) replaces the grid under Work: a breadcrumb
// back, the state pill, the seven steps, the plan in plain words and a sticky
// decision card with one primary action, bound to the exact digest the plan
// shows. The plan, the decision card and the Timeline are its sub-modules.
import { $, add, el, fill, state, ctx, newGate, ISSUE_URL, laneLabel, cells, molecules, api, failed, keepFocus, VIEWS, navigate, showView, moleculeHref, moleculeFromURL, setText, setAttr, plainText, whenText, dateTitle, withAttempt, STEPS } from './core.js';
import { mv, ensureMoleculeView, renderDecision, renderRoles, loadStats } from './molecule-decision.js';
import { renderPlan, copyDigest } from './molecule-plan.js';
import { renderTimeline, mountTimeline } from './molecule-timeline.js';

const TONES = ['needs', 'running', 'blocked', 'merged', 'neutral'];
const STEP_STATES = ['done', 'current', 'blocked', 'todo'];
const VIEW_NAMES = { inbox: 'Inbox', issues: 'Issues', work: 'Work', insights: 'Insights', formula: 'Formula' };

async function loadDetail(id, reset) {
  const token = ++state.detailToken;
  try {
    const res = await api('/api/molecules/' + encodeURIComponent(id));
    if (token !== state.detailToken) return;
    if (!res.ok) {
      state.detail = null;
      state.detailText = null;
      renderMissingDetail(id, res.status);
      return;
    }
    const text = await res.text();
    if (token !== state.detailToken) return;
    // An unchanged detail is not re-rendered, so focus and scroll stay put.
    if (!reset && state.detail && text === state.detailText) return;
    const detail = JSON.parse(text);
    if (reset || !state.detail || state.detail.id !== detail.id) state.atomIndex = null;
    state.detail = detail;
    state.detailText = text;
    keepFocus(renderDetail);
  } catch (error) { failed(error); }
}

// ---- Selection --------------------------------------------------------------
// The selected Molecule opens as the Molecule view, which replaces the grid
// under Work until the user goes back.
function select(id) {
  // A Molecule opens under Work, from any view (the Insights queue, Start Work).
  const elsewhere = state.view !== 'work';
  if (elsewhere) navigate('work', false);
  if (id === state.selectedId && !$('#selected').hidden) { if (!elsewhere) overview(); return; }
  state.selectedId = id;
  state.detail = null;
  state.atomIndex = null;
  // The selection lives in the URL (?view=work&m=…), so reload and copied links
  // restore it.
  if (moleculeFromURL() !== id) history.replaceState({ view: 'work', m: id }, '', moleculeHref(id));
  showMoleculeView(true);
  cells().forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.id === id)));
  const m = molecules().find((x) => x.id === id);
  $('#selection-label').textContent = m ? 'Selected #' + m.number + ' · ' + (m.title || m.id) : 'Selected ' + id;
  renderLoadingDetail(id);
  ctx.applyFilter();
  loadDetail(id, true);
}

function overview() {
  state.selectedId = null;
  state.detail = null;
  state.detailToken++;
  clearTimeout(state.gate.timer);
  state.gate = newGate(null);
  if (moleculeFromURL()) history.replaceState({ view: state.view }, '', '?view=' + state.view);
  showMoleculeView(false);
  cells().forEach((b) => b.setAttribute('aria-pressed', 'false'));
  $('#selection-label').textContent = 'All Molecules · select a cell to inspect';
  ctx.applyFilter();
}

// showMoleculeView swaps the grid for the Molecule view, or back. The shell's
// side panel steps aside while a Molecule is open, as in the mockup.
function showMoleculeView(open) {
  $('#selected').hidden = !open;
  $('.shelf').hidden = open;
  $('.shell').classList.toggle('has-molecule', open);
  if (!open) { mv.planKey = null; mv.statsKey = null; mv.stats = null; mv.lastDigest = null; chooseTab('plan', false); }
}

function renderLoadingDetail(id) {
  setText($('#mv-title'), 'Loading…');
  setText($('#mv-state-text'), 'Loading');
  setAttr($('#mv-state'), 'data-tone', 'neutral');
  setText($('#mv-crumb'), 'Molecule');
  renderBack();
  mv.planKey = null;
  // The previous Molecule's plan and decision card never show under this one's title.
  $('#mv-aside').hidden = true;
  $('#mv-plan-sections').hidden = true;
  $('#mv-no-plan').hidden = true;
  $('#mv-steps').hidden = true;
  $('#mv-planned').hidden = true;
  for (const s of ['#run-origin', '#molecule-totals', '#atoms', '#readiness', '#attempts']) fill($(s));
}

function renderMissingDetail(id, status) {
  renderLoadingDetail(id);
  $('#mv-plan-sections').hidden = true;
  $('#mv-no-plan').hidden = false;
  setText($('#mv-no-plan'), status === 404 ? 'This Molecule is no longer in the Lab.' : 'This ledger was refused: it is invalid, unsafe, oversized or not this Lab\'s.');
  setText($('#mv-title'), status === 404 ? 'Molecule not found' : 'Ledger refused');
  add($('#run-origin'), el('button', { type: 'button', class: 'text-button', onclick: overview }, 'Clear selection'));
}

// renderDetail patches the Molecule view: its header, plan, decision card and
// roles in place, and the Timeline tab, which keeps the Atoms, evidence and
// logs as before.
function renderDetail() {
  const d = state.detail;
  $('#mv-aside').hidden = false;
  renderHeader(d);
  renderPlan(d);
  renderDecision(d);
  renderRoles(d);
  loadStats(d);
  renderTimeline(d);
}

// ---- Molecule view: header and steps ----------------------------------------------
function renderBack() {
  const from = VIEW_NAMES[mv.from] ? mv.from : 'work';
  const back = $('#mv-back');
  setAttr(back, 'href', '?view=' + from);
  if (back.dataset.view !== from) back.dataset.view = from;
  setText(back, VIEW_NAMES[from]);
}
function crumbText(d) {
  const kind = d.view && d.view.decision && d.view.decision.kind;
  const number = d.issue && Number.isInteger(d.issue.number) ? '#' + d.issue.number + ' · ' : '';
  return number + ({ scope: 'Review plan', delivery: 'Review & publish' }[kind] || 'Molecule');
}
// pillText is the state in plain words, with the minutes of a running step.
function pillText(d) {
  const s = d.view && d.view.state;
  if (!s || typeof s.label !== 'string' || !s.label) return laneLabel(d);
  return withAttempt(s.label, s.since, s.note);
}

function renderHeader(d) {
  const c = ensureMoleculeView();
  const v = d.view || {}, s = v.state || {};
  setText($('#mv-title'), (d.sample ? '[Sample] ' : '') + plainText(d.issue && d.issue.title, 'Untitled'));
  setText($('#mv-crumb'), crumbText(d));
  renderBack();
  setAttr($('#mv-state'), 'data-tone', TONES.includes(s.tone) ? s.tone : 'neutral');
  setText($('#mv-state-text'), pillText(d));
  const number = d.issue && Number.isInteger(d.issue.number) ? '#' + d.issue.number : '';
  const url = d.issue && typeof d.issue.url === 'string' && ISSUE_URL.test(d.issue.url) ? d.issue.url : '';
  c.issue.hidden = !url;
  c.issueText.hidden = Boolean(url) || !number;
  if (url) setAttr(c.issue, 'href', url);
  setText(c.issue, 'Issue ' + number + ' ↗');
  setText(c.issueText, 'Issue ' + number);
  const plan = v.plan;
  const when = plan ? whenText(plan.plannedAt) : '';
  const planned = $('#mv-planned');
  setText(planned, when ? '· planned ' + when + ' from the Issue as written then' : '');
  setAttr(planned, 'title', when ? dateTitle(plan.plannedAt) : '');
  planned.hidden = !when;
  renderSteps(v);
}

// renderSteps marks each of the seven steps from the server's states, with
// aria-current on the current one.
function renderSteps(v) {
  const steps = Array.isArray(v.steps) && v.steps.length === STEPS.length ? v.steps : null;
  $('#mv-steps').hidden = !steps;
  if (!steps) return;
  mv.steps.forEach((li, i) => {
    const s = steps[i] && STEP_STATES.includes(steps[i].state) ? steps[i].state : 'todo';
    const current = i === v.current;
    setAttr(li, 'class', 'mv-step is-' + s + (current ? ' is-now' : ''));
    if (current) setAttr(li, 'aria-current', 'step');
    else if (li.hasAttribute('aria-current')) li.removeAttribute('aria-current');
    setText(li.lastChild, STEPS[i] + (s === 'done' ? ' · done' : s === 'blocked' ? ' · stopped' : current ? ' · now' : ''));
  });
}

// The Plan and Timeline tabs. The Timeline keeps the Atom list, evidence and
// logs unchanged.
function chooseTab(tab, focus) {
  const plan = tab !== 'timeline';
  for (const [tabId, panelId, on] of [['#mv-tab-plan', '#mv-plan', plan], ['#mv-tab-timeline', '#mv-timeline', !plan]]) {
    const button = $(tabId);
    button.setAttribute('aria-selected', String(on));
    button.tabIndex = on ? 0 : -1;
    $(panelId).hidden = !on;
    if (on && focus) button.focus();
  }
  if (!plan && mv.timelineDirty && state.detail) renderTimeline(state.detail);
}

// ---- Molecule links and history ----------------------------------------------------
// restoreSelection follows the URL: ?m= opens that Molecule's view, and a URL
// without one closes it, so Back and Forward move between the two.
function restoreSelection() {
  const id = moleculeFromURL();
  const from = history.state && history.state.from;
  if (VIEWS.includes(from)) mv.from = from;
  if (state.view === 'work' && id && id !== state.selectedId) select(id);
  else if (!id && state.selectedId) overview();
}
function openMolecule(id) {
  const from = state.view || 'work';
  if (location.search !== moleculeHref(id)) history.pushState({ view: 'work', m: id }, '', moleculeHref(id));
  rememberFrom(from);
  showView('work');
  if (id !== state.selectedId || $('#selected').hidden) select(id);
  window.scrollTo(0, 0);
  $('#mv-title').focus({ preventScroll: true });
}
// rememberFrom keeps the view a Molecule was opened from in its history entry,
// so its breadcrumb leads back there after Back, Forward or a reload.
function rememberFrom(from) {
  mv.from = VIEWS.includes(from) ? from : 'work';
  renderBack();
  try { history.replaceState(Object.assign({}, history.state, { from: mv.from }), ''); } catch (error) { /* the breadcrumb falls back to Work */ }
}

// The Molecule view mounts on #selected, inside the Work panel. It registers
// the calls other modules make: opening a Molecule, following the URL, reading
// a detail back after a gate, re-rendering the decision card and closing the
// view.
export function mount(root, ctx) {
  Object.assign(ctx, { openMolecule, restoreSelection, loadDetail, renderDecision, overview });
  $('#mv-tabs').addEventListener('click', (e) => { const b = e.target.closest('[data-tab]'); if (b) chooseTab(b.dataset.tab, false); });
  $('#mv-tabs').addEventListener('keydown', (e) => {
    const b = e.target.closest('[data-tab]');
    if (!b || !['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
    e.preventDefault();
    chooseTab(e.key === 'Home' ? 'plan' : e.key === 'End' ? 'timeline' : b.dataset.tab === 'plan' ? 'timeline' : 'plan', true);
  });
  $('#mv-timeline').addEventListener('focusout', (e) => {
    if (mv.timelineDirty && state.detail && !$('#mv-timeline').contains(e.relatedTarget)) renderTimeline(state.detail);
  });
  $('#mv-copy-digest').addEventListener('click', copyDigest);
  // A running step's minutes move on without a ledger change.
  setInterval(() => { if (!document.hidden && state.detail && !$('#selected').hidden) setText($('#mv-state-text'), pillText(state.detail)); }, 30000);
  mountTimeline();
}

// update reads the selected Molecule back after a poll; an unchanged detail is
// not re-rendered.
export function update(state) { if (state.selectedId) loadDetail(state.selectedId, false); }
