// ---- Inbox --------------------------------------------------------------------
// The home view: what needs you first, then work in progress, then the last
// drafts recorded. Every entry is the server's InboxView, in plain words, so
// no phase code, id, digest or timestamp is turned into text here. Polling
// patches entries in place, keyed by Molecule id: an entry already shown keeps
// its node and has only its changed text and attributes updated.
import { $, el, fill, state, molecules, live, inboxOf, newestFirst, needsYou, needsYouText, moleculeHref, setText, setAttr, plainText, count, counted, ago, dateTitle, STEPS, withAttempt, syncOf, syncNotes, closedInProgress, inHistory, historyFromURL, historyRow, followHistory, syncCounts, syncCountsText } from './core.js';

const CARD_KINDS = { scope: ['is-needs', '◇'], publish: ['is-ready', '✓'], blocked: ['is-blocked', '!'], closed: ['is-needs', '◇'] };

// closedView is the Needs-you card of a row-14 Molecule (its Issue closed while
// it was live): the server's label and fixed notes, never an Inbox kind of its own.
function closedView(m) {
  if (!closedInProgress(m)) return null;
  const s = syncOf(m.id);
  return { kind: 'closed', title: s.label, summary: syncNotes(s).join(' · '), action: 'Open' };
}
// Bounds: at most this many cards or rows each, and the last five recorded.
const MAX_ENTRIES = 20, MAX_RECENT = 5;

function numberText(m) { return Number.isInteger(m.number) ? '#' + m.number : ''; }
function titleText(m) { return (state.list && state.list.sample ? '[Sample] ' : '') + plainText(m.title, 'Untitled'); }

function stepIndex(v) { return Number.isInteger(v.step) && v.step >= 0 && v.step < STEPS.length ? v.step : 0; }
// stepText is the current step with its elapsed time: "Coding · 18 min".
function stepText(v) {
  if (v.queued) return 'Queued for checks';
  return withAttempt(plainText(v.stepLabel, STEPS[stepIndex(v)]), v.stepSince, v.stepNote);
}
function detailText(v) {
  const parts = [];
  if (v.kind === 'scope') {
    if (typeof v.summary === 'string' && v.summary) parts.push(v.summary);
    if (v.planned) parts.push(counted(count(v.files), 'file', 'files'), counted(count(v.checks), 'check', 'checks'));
  } else if (v.kind === 'publish') {
    if (v.verified) parts.push(count(v.passed) + ' of ' + counted(count(v.ran), 'check', 'checks') + ' passed');
    if (v.captured) parts.push(counted(count(v.changed), 'file', 'files') + ' changed');
  } else if (typeof v.summary === 'string' && v.summary) parts.push(v.summary);
  return parts.join(' · ');
}
function metaText(m, v) {
  const when = v.kind === 'scope' ? ago(m.startedAt) : ago(m.finishedAt || m.startedAt);
  const verb = { scope: 'planned ', publish: 'finished ', blocked: 'stopped ' }[v.kind] || '';
  return [numberText(m), when ? verb + when : ''].filter(Boolean).join(' · ');
}

function slot(node, name) { return node.querySelector('[data-slot="' + name + '"]'); }
// setLink points a Molecule link at its ?view=work&m= URL, labelled with its action.
function setLink(a, m, text) {
  setAttr(a, 'href', moleculeHref(m.id));
  if (a.dataset.molecule !== m.id) a.dataset.molecule = m.id;
  setText(a, text);
  setAttr(a, 'aria-label', text + ': ' + [numberText(m), titleText(m)].filter(Boolean).join(' '));
}

// patchList keeps container's children in items' order, keyed by Molecule id:
// the node of an id already shown is kept and updated in place, a new id gets
// a new node, and the node of an id no longer listed is removed.
function patchList(container, items, create, update) {
  const existing = new Map([...container.children].map((node) => [node.dataset.inboxId, node]));
  let at = container.firstElementChild;
  for (const m of items) {
    let node = existing.get(m.id);
    existing.delete(m.id);
    if (!node) { node = create(); node.dataset.inboxId = m.id; }
    update(node, m, inboxOf(m));
    if (node === at) at = at.nextElementSibling;
    else container.insertBefore(node, at);
  }
  for (const node of existing.values()) {
    if (node.contains(document.activeElement)) $('#main').focus({ preventScroll: true });
    node.remove();
  }
}

function createCard() {
  return el('article', { class: 'inbox-card glass' },
    el('div', { class: 'inbox-icon', 'aria-hidden': 'true' }, el('span', { dataset: { slot: 'icon' } })),
    el('div', { class: 'inbox-body' },
      el('div', { class: 'inbox-head' }, el('span', { class: 'inbox-badge', dataset: { slot: 'badge' } }), el('span', { class: 'inbox-meta', dataset: { slot: 'meta' } })),
      el('h3', { class: 'inbox-title', dataset: { slot: 'title' } }),
      el('p', { class: 'inbox-detail', dataset: { slot: 'detail' } })),
    el('a', { class: 'btn btn-primary inbox-action', dataset: { slot: 'action' } }));
}
function updateCard(node, m, v) {
  v = closedView(m) || v;
  const [cls, icon] = CARD_KINDS[v.kind] || CARD_KINDS.scope;
  setAttr(node, 'class', 'inbox-card glass ' + cls);
  setText(slot(node, 'icon'), icon);
  setText(slot(node, 'badge'), plainText(v.title, 'Needs you'));
  const meta = slot(node, 'meta');
  setText(meta, metaText(m, v));
  setAttr(meta, 'title', dateTitle(v.kind === 'scope' ? m.startedAt : m.finishedAt || m.startedAt));
  setText(slot(node, 'title'), titleText(m));
  const detail = slot(node, 'detail'), text = detailText(v);
  setText(detail, text);
  detail.hidden = !text;
  // One primary action per card; each opens the Molecule view (?view=work&m=…).
  setLink(slot(node, 'action'), m, plainText(v.action, 'Open'));
}

function createRow() {
  return el('article', { class: 'inbox-row' },
    el('div', { class: 'inbox-row-head' },
      el('span', { class: 'inbox-number', dataset: { slot: 'number' } }),
      el('h3', { class: 'inbox-title', dataset: { slot: 'title' } }),
      el('span', { class: 'inbox-step', dataset: { slot: 'step' } }),
      el('a', { class: 'btn btn-ghost inbox-watch', dataset: { slot: 'action' } })),
    el('ol', { class: 'inbox-steps', 'aria-label': 'Progress' }, STEPS.map((label) => el('li', null, el('span', { class: 'step-bar', 'aria-hidden': 'true' }), label))));
}
function updateRow(node, m, v) {
  setText(slot(node, 'number'), numberText(m));
  setText(slot(node, 'title'), titleText(m));
  setText(slot(node, 'step'), stepText(v));
  setLink(slot(node, 'action'), m, plainText(v.action, 'Watch'));
  const at = stepIndex(v);
  node.querySelectorAll('.inbox-steps li').forEach((li, i) => {
    const cls = i < at ? 'is-done' : i === at ? 'is-current' : '';
    if (li.className !== cls) li.className = cls;
    if (i === at) setAttr(li, 'aria-current', 'step');
    else if (li.hasAttribute('aria-current')) li.removeAttribute('aria-current');
  });
}

function createRecent() {
  return el('a', { class: 'inbox-recent-row' },
    el('span', { class: 'recent-dot', 'aria-hidden': 'true' }),
    el('span', { class: 'inbox-number', dataset: { slot: 'number' } }),
    el('span', { class: 'recent-title', dataset: { slot: 'title' } }),
    el('span', { class: 'inbox-meta', dataset: { slot: 'meta' } }));
}
function updateRecent(node, m, v) {
  setAttr(node, 'href', moleculeHref(m.id));
  if (node.dataset.molecule !== m.id) node.dataset.molecule = m.id;
  setText(slot(node, 'number'), numberText(m));
  setText(slot(node, 'title'), titleText(m));
  const when = ago(m.finishedAt || m.startedAt), s = syncOf(m.id);
  // GitHub's state, when it was read, names the outcome: Merged, Draft PR open, …
  setText(slot(node, 'meta'), (s && s.checked ? s.label : plainText(v.title, 'Draft published')) + (when ? ' · ' + when : ''));
  setAttr(node, 'title', dateTitle(m.finishedAt || m.startedAt));
}

function moreText(node, total, where) {
  const extra = total - MAX_ENTRIES;
  setText(node, extra > 0 ? counted(extra, 'more', 'more') + ' under ' + where + '.' : '');
  node.hidden = extra <= 0;
}

function partOfDay(hour) { return hour < 12 ? 'morning' : hour < 18 ? 'afternoon' : 'evening'; }
// firstName is the first word of a configured operator name, or '' for the
// fallback.
function firstName() {
  const o = state.lab && state.lab.operator;
  if (!o || !o.set || typeof o.name !== 'string') return '';
  const word = o.name.trim().split(/\s+/)[0] || '';
  return /\p{L}/u.test(word) && !word.includes('[') ? word : '';
}

function renderInbox() {
  const counts = (state.list && state.list.counts) || {};
  // Live work first: merged and closed history collapses into one row, shown
  // only with ?history=1; a Molecule under Needs you is listed only there.
  const history = molecules().filter((m) => inHistory(m) && inboxOf(m)).sort(newestFirst);
  const entries = molecules().filter((m) => live(m) && inboxOf(m) && !inHistory(m));
  const kind = (k) => entries.filter((m) => inboxOf(m).kind === k && !needs.includes(m));
  const needs = needsYou();
  const running = kind('progress').sort(newestFirst);
  const recent = kind('recorded').sort(newestFirst).slice(0, MAX_RECENT);
  patchList($('#inbox-needs'), needs.slice(0, MAX_ENTRIES), createCard, updateCard);
  patchList($('#inbox-progress'), running.slice(0, MAX_ENTRIES), createRow, updateRow);
  patchList($('#inbox-recent'), recent, createRecent, updateRecent);
  const showHistory = historyFromURL() && history.length > 0;
  historyRow($('#inbox-history'), history);
  patchList($('#inbox-history-list'), showHistory ? history.slice(0, MAX_ENTRIES) : [], createRecent, updateRecent);
  $('#inbox-history-list').hidden = !showHistory;
  setText($('#inbox-sync-counts'), syncCountsText());
  $('#inbox-needs-empty').hidden = needs.length > 0;
  $('#inbox-progress-empty').hidden = running.length > 0;
  $('#inbox-recent-empty').hidden = recent.length > 0;
  moreText($('#inbox-needs-more'), needs.length, 'Work');
  moreText($('#inbox-progress-more'), running.length, 'Work, Running');

  const name = firstName();
  setText($('#inbox-title'), 'Good ' + partOfDay(new Date().getHours()) + (name ? ', ' + name : ''));
  // Once /api/sync is loaded the headline counts its buckets, as the counts
  // header does; the ledger lanes stand in only until then.
  const served = syncCounts(), lane = (k, b) => served ? count(served[b]) : counts[k] || 0;
  const waiting = needs.length, inFlight = lane('running', 'running'), blocked = lane('blocked', 'blocked'), drafts = lane('finished', 'draft-pr');
  // Merged counts need verified merge attribution; until then drafts are counted.
  const parts = [
    waiting ? needsYouText(waiting) : 'Nothing needs you',
    inFlight ? inFlight + ' running' : 'nothing running',
    blocked ? blocked + ' blocked' : 'nothing blocked',
    drafts ? counted(drafts, 'draft published', 'drafts published') : 'no draft published yet',
  ];
  const line = $('#inbox-counts');
  if (line.textContent !== parts.join(' · ')) fill(line, el('strong', null, parts[0]), ' · ' + parts.slice(1).join(' · '));
  // One persistent live region, changed only when its text changes.
  setText($('#inbox-live'), waiting ? needsYouText(waiting) + '.' : 'Nothing needs you right now.');
  const note = $('#inbox-note');
  const complete = !state.list || state.list.complete;
  setText(note, complete ? '' : 'Some ledgers could not be read, so this Inbox may be missing Molecules. See Work for the diagnostics.');
  note.hidden = complete;
}

// The Inbox mounts on its data-view-panel; its markup stays in index.html. The
// history row toggles ?history=1 through history.pushState.
export function mount(root, ctx) { $('#inbox-history').addEventListener('click', followHistory); }

// update patches the Inbox from the list and the Lab in place.
export function update(state) { renderInbox(); }
