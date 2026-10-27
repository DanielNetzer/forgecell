// ---- Molecule view: the plan in plain words ----------------------------------------
// What Forgecell will do, the files it may change, the checks it must pass and
// what counts as done, from the server's plain MoleculeView: no phase code, id
// or full digest is turned into prose, and the plan's digest appears only
// short, inside Plan details.
import { $, el, fill, short, setText, plainText, count, counted, wholeNumber, minutesText } from './core.js';
import { mv } from './molecule-decision.js';

const CHANGE_BADGES = { new: ['NEW', 'Not in the base tree: a new file'], edit: ['EDIT', 'In the base tree: an edit'] };

// renderPlan rebuilds the plan sections only when the plan itself changes.
export function renderPlan(d) {
  const plan = d.view && d.view.plan;
  const key = plan ? JSON.stringify(plan) : 'none';
  if (key === mv.planKey) return;
  mv.planKey = key;
  $('#mv-plan-sections').hidden = !plan;
  const none = $('#mv-no-plan');
  none.hidden = Boolean(plan);
  if (!plan) { setText(none, d.sample ? 'Sample data has no plan.' : 'No plan is recorded for this Molecule yet.'); return; }
  setText($('#mv-summary'), plainText(plan.summary, 'No summary was recorded for this plan.'));
  fill($('#mv-chips'), planChips(plan).map((text) => el('li', { class: 'chip' }, text)));
  const note = $('#mv-note');
  setText(note, plan.note ? 'Your note for this version: ' + plan.note : '');
  note.hidden = !plan.note;
  const files = Array.isArray(plan.files) ? plan.files : [];
  setText($('#mv-files-count'), wholeNumber.format(count(plan.filesTotal)));
  fill($('#mv-files'), files.map(fileItem));
  moreLine($('#mv-files-more'), count(plan.filesTotal) - files.length, 'file', 'files');
  const groups = Array.isArray(plan.checkGroups) ? plan.checkGroups : [];
  setText($('#mv-checks-count'), wholeNumber.format(count(plan.checksTotal)));
  fill($('#mv-checks'), groups.map(groupItem));
  const acceptance = Array.isArray(plan.acceptance) ? plan.acceptance : [];
  setText($('#mv-acceptance-count'), wholeNumber.format(count(plan.acceptanceTotal)));
  fill($('#mv-acceptance'), acceptance.map((text) => el('li', null, plainText(text, ''))));
  moreLine($('#mv-acceptance-more'), count(plan.acceptanceTotal) - acceptance.length, 'criterion', 'criteria');
  $('#mv-acceptance-section').hidden = count(plan.acceptanceTotal) === 0;
  renderPlanDetails(plan);
}

function planChips(plan) {
  const chips = [];
  const questions = count(plan.questions);
  chips.push(questions ? counted(questions, 'open question', 'open questions') : 'No open questions');
  const parts = Array.isArray(plan.components) ? plan.components.filter((p) => typeof p === 'string' && p) : [];
  const extra = count(plan.componentsTotal) - parts.length;
  if (parts.length) chips.push('Touches ' + parts.join(', ') + (extra > 0 ? ' and ' + counted(extra, 'more', 'more') : ''));
  if (Number.isInteger(plan.budgetMs) && plan.budgetMs > 0) chips.push('Coding budget ' + minutesText(plan.budgetMs));
  return chips;
}

// fileItem is one scope path with its NEW or EDIT badge, which the server
// derives from the frozen base tree; without that tree there is no badge.
function fileItem(f) {
  const badge = CHANGE_BADGES[f.change];
  return el('li', { class: 'file' },
    badge ? el('span', { class: 'file-badge is-' + f.change, title: badge[1] }, badge[0]) : el('span', { class: 'file-badge is-unknown', title: 'The base tree is not recorded' }, '·'),
    el('div', { class: 'file-text' }, el('code', null, plainText(f.path, '')), f.reason ? el('p', { class: 'file-reason' }, f.reason) : null));
}

function groupItem(g) {
  const checks = Array.isArray(g.checks) ? g.checks : [];
  return el('li', { class: 'check-group' + (g.baseline ? ' is-baseline' : '') },
    el('h3', { class: 'check-name' }, plainText(g.name, 'Checks')),
    el('ul', { class: 'check-commands' }, checks.map((x) => el('li', null, el('code', null, plainText(x.command, '')), x.dir ? el('span', { class: 'check-dir' }, ' in ' + x.dir) : null))));
}

function moreLine(node, extra, one, many) {
  setText(node, extra > 0 ? counted(extra, 'more ' + one, 'more ' + many) + ' in the full plan, under Timeline · Ledger record.' : '');
  node.hidden = extra <= 0;
}

// renderPlanDetails fills the collapsed Plan details: the version, the short
// digest with a copy button and the frozen evidence.
function renderPlanDetails(plan) {
  const digest = typeof plan.digest === 'string' && /^[0-9a-f]{64}$/.test(plan.digest) ? plan.digest : '';
  fill($('#mv-details-summary'), 'Plan details · version ' + count(plan.revision) + (digest ? ' · ' : ''), digest ? el('code', null, digest.slice(0, 7)) : null);
  setText($('#mv-digest-short'), digest ? short(digest) : 'Not recorded');
  const copy = $('#mv-copy-digest');
  copy.hidden = !digest;
  copy.dataset.digest = digest;
  const evidence = Array.isArray(plan.evidence) ? plan.evidence : [];
  fill($('#mv-evidence'), evidence.map((e) => el('li', null, el('code', null, plainText(e.path, '')))));
  moreLine($('#mv-evidence-more'), count(plan.evidenceTotal) - evidence.length, 'evidence file', 'evidence files');
}

export async function copyDigest() {
  const button = $('#mv-copy-digest'), digest = button.dataset.digest || '';
  if (!/^[0-9a-f]{64}$/.test(digest)) return;
  try {
    await navigator.clipboard.writeText(digest);
    setText(button, 'Copied');
  } catch (error) { setText(button, 'Copy unavailable'); }
  setTimeout(() => setText(button, 'Copy full digest'), 2000);
}
