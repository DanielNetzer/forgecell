// Forgecell local Lab UI. It reads the Lab, and writes only through the engine:
// the two human gates (exact scope approval and draft PR publication), Request
// changes on a waiting plan (a new version, approved separately), starting
// intake or the loop, and stopping or retiring a Molecule. Every value
// from the API is untrusted and reaches the page only as a text node or a
// validated attribute: no module parses data as markup.
//
// This entry mounts each view on its existing markup, renders the shell
// (header, needs-you badge, resident and avatar), polls the Lab and fans each
// update out to the views. Views share js/core.js and never import each other.
import { $, el, fill, state, ctx, REPOSITORY, notRecorded, api, showError, failed, keepFocus, needsYou, needsYouText, plural, setText, MOLECULE_ID, viewFromURL, showView, navigate, mountDialog } from './js/core.js';
import * as inbox from './js/inbox.js';
import * as molecule from './js/molecule.js';
import * as work from './js/work.js';
import * as issues from './js/issues.js';
import * as insights from './js/insights.js';
import * as formula from './js/formula.js';
import * as loop from './js/loop.js';

async function loadLab() {
  const token = ++state.labToken;
  const res = await api('/api/lab');
  if (!res.ok) throw new Error('lab ' + res.status);
  const lab = await res.json();
  if (token !== state.labToken) return;
  state.lab = lab;
  renderLab();
}

// One poll at a time; a response older than the newest request never
// overwrites newer state.
async function poll() {
  if (document.hidden || state.polling || state.sessionLost) return;
  state.polling = true;
  const token = ++state.pollToken;
  try {
    const res = await api('/api/molecules', state.etag ? { 'If-None-Match': state.etag } : undefined);
    if (res.status === 304 || token !== state.pollToken) return;
    if (!res.ok) throw new Error('molecules ' + res.status);
    const list = await res.json();
    if (token !== state.pollToken) return;
    const previous = state.etag;
    state.list = list;
    state.etag = res.headers.get('ETag') || list.etag;
    showError('');
    keepFocus(renderList);
    if (previous && previous !== state.etag) loadLab().catch(failed);
    if (previous !== state.etag) loadAnalytics();
    molecule.update(state);
  } catch (error) { failed(error); } finally { state.polling = false; }
}

// Analytics share the list's ETag; a response older than the newest request is
// dropped, as for the list.
async function loadAnalytics() {
  const token = ++state.analyticsToken;
  try {
    const res = await api('/api/analytics', state.analyticsEtag ? { 'If-None-Match': state.analyticsEtag } : undefined);
    if (res.status === 304 || token !== state.analyticsToken) return;
    if (!res.ok) throw new Error('analytics ' + res.status);
    const analytics = await res.json();
    if (token !== state.analyticsToken) return;
    state.analytics = analytics;
    state.analyticsEtag = res.headers.get('ETag') || analytics.etag;
    ctx.renderAnalytics();
  } catch (error) { failed(error); }
}

// ---- Lab header and Formula ---------------------------------------------
function renderLab() {
  const lab = state.lab;
  const sample = Boolean(lab.sample);
  $('#sample-badge').hidden = !sample;
  document.body.classList.toggle('sample', sample);
  document.title = sample ? 'Forgecell · Sample data' : 'Forgecell · ' + notRecorded(lab.name);
  const repo = $('#lab-repo');
  if (!sample && REPOSITORY.test(lab.repository || '')) {
    fill(repo, el('a', { href: 'https://github.com/' + lab.repository, target: '_blank', rel: 'noopener noreferrer' }, lab.repository + ' ↗'));
  } else {
    fill(repo, lab.repository || 'Repository not recorded');
  }
  $('#footer-version').textContent = 'Forgecell ' + notRecorded(lab.version) + ' · Lab ' + notRecorded(lab.name) + (lab.binding ? ' · binding ' + lab.binding : '') + (lab.model ? ' · model ' + lab.model : '');
  formula.update(state);
  renderOperator(lab.operator);
  // The greeting names the operator.
  if (state.list) inbox.update(state);
  // Start intake and the loop, when the Lab drives work through the engine.
  loop.update(state);
}

// renderList fans a new list out: the Work grid, the Insights overview, the
// shell and the Inbox, in that order.
function renderList() {
  work.update(state);
  insights.update(state);
  renderShell();
  inbox.update(state);
}

// ---- App shell: needs-you badge, resident and operator --------------------
function runningText(n) { return n + (plural.select(n) === 'one' ? ' Molecule running' : ' Molecules running'); }

// The resident's pose follows Lab state: waving when anything needs you,
// studying a cube while any Molecule runs, reading otherwise. waiting is the
// needsYou() count.
function residentPose(waiting, counts) {
  if (waiting > 0) return 'waving';
  if ((counts.running || 0) > 0) return 'studying';
  return 'reading';
}
const RESIDENT_TITLES = { waving: (w) => w + (plural.select(w) === 'one' ? ' thing is waiting for you' : ' things are waiting for you'), studying: (w, c) => runningText(c.running), reading: () => 'All quiet' };

// renderShell patches the badge and the resident in place from needsYou() and
// the list counts.
function renderShell() {
  const counts = (state.list && state.list.counts) || {};
  const waiting = needsYou().length;
  for (const badge of [$('#needs-you-badge'), $('#needs-you-tab-badge')]) {
    badge.hidden = waiting === 0;
    setText(badge, String(waiting));
    badge.setAttribute('aria-label', needsYouText(waiting));
  }
  const pose = residentPose(waiting, counts);
  const resident = $('#resident');
  if (resident.dataset.pose !== pose) resident.dataset.pose = pose;
  setText($('#resident-title'), RESIDENT_TITLES[pose](waiting, counts));
  ctx.renderLoopPill();
}

// The avatar shows the operator's initials from GET /api/lab, or a generic
// glyph for the fallback; the name is only an accessible label and a title.
function renderOperator(operator) {
  const o = operator && typeof operator === 'object' ? operator : {};
  const name = typeof o.name === 'string' && o.name ? o.name : 'Operator';
  const initials = typeof o.initials === 'string' && /^[\p{L}\p{N}]{1,2}$/u.test(o.initials) ? o.initials : '';
  const avatar = $('#operator-avatar');
  avatar.setAttribute('aria-label', 'Operator: ' + name);
  avatar.title = name;
  setText($('#operator-initials'), initials);
  // The glyph is an <svg>: hidden is an HTMLElement property that SVG ignores,
  // so the attribute is toggled instead.
  $('#operator-glyph').toggleAttribute('hidden', Boolean(initials));
}

// ---- Mount, route and poll ------------------------------------------------
// Each view mounts on its existing markup: the five views on their
// data-view-panel, the Molecule view on #selected under Work and the loop on
// the side panel's #drive-slot.
for (const [view, root] of [
  [work, $('[data-view-panel="work"]')],
  [molecule, $('#selected')],
  [inbox, $('[data-view-panel="inbox"]')],
  [issues, $('[data-view-panel="issues"]')],
  [insights, $('[data-view-panel="insights"]')],
  [formula, $('[data-view-panel="formula"]')],
  [loop, $('#drive-slot')],
]) view.mount(root, ctx);

document.addEventListener('click', (e) => {
  const link = e.target.closest('a[data-view]');
  if (!link || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  e.preventDefault();
  navigate(link.dataset.view, true);
});
window.addEventListener('popstate', () => { showView(viewFromURL()); ctx.restoreSelection(); });
document.addEventListener('click', (e) => {
  const link = e.target.closest('a[data-molecule]');
  if (!link || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  if (!MOLECULE_ID.test(link.dataset.molecule)) return;
  e.preventDefault();
  ctx.openMolecule(link.dataset.molecule);
});

showView(viewFromURL());
ctx.restoreSelection();

// ---- Dialog and visibility -------------------------------------------------
mountDialog();
document.addEventListener('visibilitychange', () => {
  document.body.classList.toggle('page-hidden', document.hidden);
  if (!document.hidden) poll();
});
if ('IntersectionObserver' in window) {
  // The resident's working pose pauses while it is offscreen.
  new IntersectionObserver(([entry]) => document.body.classList.toggle('resident-offscreen', !entry.isIntersecting)).observe($('#resident'));
}

loadLab().then(poll).catch(failed);
setInterval(poll, 5000);
// An unchanged list answers 304, so the Inbox's elapsed and relative times are
// patched on their own interval.
setInterval(() => { if (!document.hidden && state.list) inbox.update(state); }, 30000);
// The Lab header also changes without a ledger change, as when a Formula is
// activated, so it is refreshed on its own slower interval.
setInterval(() => { if (!document.hidden && !state.sessionLost) loadLab().catch(failed); }, 30000);
