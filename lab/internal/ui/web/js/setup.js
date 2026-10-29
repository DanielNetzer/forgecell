// Set up your Lab (#263): the view forgecell ui serves in a checkout with no
// ready Lab. Every step is one engine call on the server (GET /api/setup and
// POST /api/setup/…); this page never writes Lab files. The step always comes
// from the Lab's own state, so a reload resumes where the Lab is. One request is
// in flight at a time; after a lost or ambiguous response the state is read back
// and nothing is resubmitted. Approve Formula sends the exact proposal and
// digest shown, and nothing is approved without that click. Every served value
// is untrusted and reaches the page only as a text node.
import { $, el, uid, fill, setText, state, short, counted, SessionError, tabSession, api, failed, showView } from './core.js';

const STEPS = [
  ['repository', 'Repository'],
  ['agent', 'Coding agent'],
  ['create', 'Create the Lab'],
  ['review', 'Approve the Formula'],
  ['done', 'Done'],
];
const STEP_INDEX = { choose: 0, repository: 0, agent: 1, review: 3, done: 4, problem: 0 };
const KNOWN_STEPS = ['choose', 'repository', 'agent', 'review', 'done', 'problem'];
const AGENT_IDS = ['claude-code', 'codex', 'cursor'];
const ASSAY_ID = /^assay-[a-f0-9]{64}$/;
const DIGEST = /^[a-f0-9]{64}$/;

// setup is this view's state: the last view served, the action in flight, the
// inline error and the form it belongs to, and each card's last rendered key.
const setup = { root: null, view: null, pending: '', error: '', errorFor: '', keys: {}, timer: null, review: false, live: '' };

const text = (value, fallback) => (typeof value === 'string' && value ? value : fallback);

// fieldId is one uid() per setup input, kept across card rebuilds so its label
// stays bound and refocus finds the same id. Keys come from served values, so
// the map stays bounded.
const fieldIds = new Map();
function fieldId(key) {
  if (!fieldIds.has(key)) {
    if (fieldIds.size >= 64) fieldIds.clear();
    fieldIds.set(key, uid('setup-field'));
  }
  return fieldIds.get(key);
}

export function mount(root, ctx) {
  setup.root = root;
  ctx.setupActive = () => Boolean(state.setup);
}

// update re-renders from the last served view; the entry calls it like any view.
export function update(state) {
  if (state && state.setup && setup.view) render();
}

// begin reads GET /api/setup once at start-up. It resolves true when this
// launch is setting up its Lab, and the entry then loads nothing else.
export async function begin() {
  const view = await read();
  if (!view || !inSetup(view)) return false;
  state.setup = true;
  document.body.classList.add('setup-mode');
  window.addEventListener('popstate', () => { if (state.setup) showView('setup'); });
  showView('setup');
  setURL(view.step, false);
  show(view);
  // The repository and coding agents are read once on arrival, never on a poll.
  if (!view.checked && (view.step === 'repository' || view.step === 'agent')) submit('check', {}, 'repository');
  return true;
}

// A Lab approved before this load opens the app; a proposal still waiting,
// even after approval (the review Atom), stays here.
function inSetup(view) {
  return KNOWN_STEPS.includes(view.step) && (view.active === true || view.step === 'review');
}

async function read() {
  try {
    const res = await api('/api/setup');
    if (!res.ok) return null;
    return await res.json();
  } catch (error) { failed(error); return null; }
}

function setURL(step, push) {
  const query = '?view=setup&step=' + encodeURIComponent(step);
  if (location.search === query) return;
  if (push) history.pushState({ view: 'setup', step: step }, '', query);
  else history.replaceState({ view: 'setup', step: step }, '', query);
}

function show(view) {
  const previous = setup.view && setup.view.step;
  setup.view = view;
  if (previous && previous !== view.step) setURL(view.step, true);
  render();
  // Another tab may run a step: read back while one is in flight.
  clearTimeout(setup.timer);
  if (view.busy) setup.timer = setTimeout(async () => { const next = await read(); if (next) show(next); }, 2000);
}

function announce(message) {
  if (setup.live === message) return;
  setup.live = message;
  setText($('#setup-live'), message);
}

// ---- Requests ---------------------------------------------------------------
async function post(action, payload) {
  const session = tabSession();
  if (!session || state.sessionLost) throw new SessionError('no session');
  const res = await fetch('/api/setup/' + action, { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'X-Forgecell-Session': session, 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
  if (res.status === 403) throw new SessionError('forbidden');
  return res;
}

const PROGRESS = { check: 'Checking…', choose: 'Opening…', create: 'Creating your Lab…', approve: 'Approving…', review: 'Proposing the review Atom…' };

// submit posts once. A refusal shows inline on the form; a lost or 5xx
// response reads the state back instead of resubmitting.
async function submit(action, payload, form) {
  if (setup.pending) return false;
  setup.pending = action;
  setup.error = '';
  setup.errorFor = form;
  render();
  announce(PROGRESS[action]);
  let res = null, body = {};
  try {
    res = await post(action, payload);
    body = await res.json().catch(() => ({}));
  } catch (error) {
    if (error instanceof SessionError) { setup.pending = ''; failed(error); render(); return false; }
    res = null;
  }
  setup.pending = '';
  if (!res || res.status >= 500) {
    setup.error = 'The response was lost. Your Lab’s state was read back; nothing was resubmitted.';
    const next = await read();
    if (next) show(next); else render();
    focusError(form);
    return false;
  }
  if (!res.ok) {
    setup.error = text(body.error, 'Refused') + '.' + (typeof body.reason === 'string' && body.reason ? ' ' + body.reason + '.' : '');
    if (body.category === 'stale' || body.category === 'busy') {
      const next = await read();
      if (next) show(next);
    }
    render();
    focusError(form);
    return false;
  }
  show(body);
  announce(stepTitle(body));
  return true;
}

function focusError(form) {
  const node = document.getElementById('setup-error-' + form);
  if (node && !node.hidden) node.focus({ preventScroll: false });
}

// ---- Rendering -----------------------------------------------------------------
function stepTitle(view) {
  switch (view.step) {
    case 'choose': return 'Choose a Lab for this repository';
    case 'repository': return 'Step 1 of 5: check your repository';
    case 'agent': return 'Step 2 of 5: pick a coding agent';
    case 'review': return 'Step 4 of 5: Formula ready for your approval';
    case 'done': return 'Your Lab is ready';
  }
  return 'Your Lab needs attention';
}

function render() {
  const view = setup.view;
  if (!view) return;
  renderSteps(view);
  const cards = [];
  if (view.step === 'choose') cards.push(['choose', chooseCard]);
  else if (view.step === 'problem') cards.push(['problem', problemCard]);
  else {
    cards.push(['repository', repositoryCard]);
    if (view.step === 'agent') cards.push(['agent', agentCard]);
    if (view.step === 'review') cards.push(['review', reviewCard]);
    if (view.step === 'done') cards.push(['done', doneCard]);
  }
  const body = $('#setup-body');
  const wanted = cards.map(([name]) => 'setup-card-' + name);
  for (const node of [...body.children]) if (!wanted.includes(node.id)) { node.remove(); delete setup.keys[node.id]; }
  let previous = null;
  for (const [name, build] of cards) {
    const id = 'setup-card-' + name;
    const key = JSON.stringify([view.step, view.busy, view[name === 'agent' ? 'agents' : name] || null, view.repository || null, view.proposal || null, view.reviewAtom || '', view.problem || '', view.labs || null, setup.pending, setup.error, setup.errorFor, setup.review]);
    let node = document.getElementById(id);
    if (!node || setup.keys[id] !== key) {
      const focused = node && node.contains(document.activeElement) ? document.activeElement : null;
      const next = build(view);
      next.id = id;
      if (node) node.replaceWith(next); else if (previous) previous.after(next); else body.prepend(next);
      setup.keys[id] = key;
      node = next;
      if (focused) refocus(node, focused);
    }
    previous = node;
  }
}

// refocus finds the rebuilt control the operator was on, by id or by name.
function refocus(card, focused) {
  const next = (focused.id && card.querySelector('#' + CSS.escape(focused.id))) || (focused.name && card.querySelector('[name="' + CSS.escape(focused.name) + '"]'));
  if (next && !next.disabled) next.focus({ preventScroll: true });
}

function renderSteps(view) {
  const list = $('#setup-steps');
  const current = view.step === 'agent' && setup.pending === 'create' ? 2 : STEP_INDEX[view.step] || 0;
  if (list.children.length !== STEPS.length) fill(list, STEPS.map(([key]) => el('li', { class: 'setup-step', dataset: { step: key } })));
  STEPS.forEach(([, label], i) => {
    const item = list.children[i];
    const status = i < current ? 'done' : i === current ? 'current' : 'next';
    if (item.dataset.status !== status) item.dataset.status = status;
    if (status === 'current') item.setAttribute('aria-current', 'step'); else item.removeAttribute('aria-current');
    setText(item, (i + 1) + '. ' + label + (status === 'done' ? ' · done' : ''));
  });
}

function card(title, ...children) {
  return el('section', { class: 'glass setup-card', 'aria-label': title }, el('h2', { class: 'setup-card-title' }, title), ...children);
}

// errorLine is a form's inline error, linked by aria-describedby.
function errorLine(form) {
  const shown = setup.errorFor === form && Boolean(setup.error);
  return el('p', { id: 'setup-error-' + form, class: 'setup-error', tabindex: '-1', hidden: !shown }, shown ? setup.error : '');
}

// SUBMIT_TYPE makes every setup button submit its form, so Enter submits.
const SUBMIT_TYPE = 'submit';

function submitButton(action, label, primary) {
  const busy = setup.pending === action;
  return el('button', { type: SUBMIT_TYPE, class: 'btn ' + (primary ? 'btn-primary' : 'btn-secondary'), disabled: Boolean(setup.pending) || (setup.view && setup.view.busy), 'aria-busy': busy ? 'true' : null }, busy ? PROGRESS[action] : label);
}

function form(name, onsubmit, ...children) {
  return el('form', { class: 'setup-form', autocomplete: 'off', 'aria-describedby': 'setup-error-' + name, onsubmit: (e) => { e.preventDefault(); onsubmit(e.currentTarget); } }, ...children, errorLine(name));
}

function chooseCard(view) {
  const labs = Array.isArray(view.labs) ? view.labs : [];
  return card('Choose a Lab',
    el('p', { class: 'setup-note' }, counted(labs.length, 'Lab matches', 'Labs match') + ' this repository. Pick the one to open; the others stay as they are.'),
    form('choose', (f) => { const picked = f.querySelector('input[name="lab"]:checked'); if (picked) submit('choose', { lab: Number(picked.value) }, 'choose'); },
      el('fieldset', { class: 'setup-options' }, el('legend', { class: 'sr-only' }, 'Labs for this repository'),
        labs.map((lab, i) => el('label', { class: 'setup-option', for: fieldId('lab-' + i) },
          el('input', { type: 'radio', id: fieldId('lab-' + i), name: 'lab', value: String(Number.isInteger(lab.index) ? lab.index : i), required: true, checked: i === 0 }),
          el('span', { class: 'setup-option-text' }, el('strong', null, text(lab.name, 'Lab')), el('small', null, lab.formula ? 'Formula ' + lab.formula + (lab.binding ? ' · ' + lab.binding : '') : 'No Formula yet'))))),
      el('div', { class: 'setup-actions' }, submitButton('choose', 'Open Lab', true))));
}

function problemCard(view) {
  return card('Your Lab needs attention', el('p', { class: 'setup-note', role: 'alert' }, text(view.problem, 'The Lab could not be read.')));
}

function repositoryCard(view) {
  const repo = view.repository;
  const rows = el('dl', { class: 'setup-facts' });
  let note;
  if (!view.checked) note = el('p', { class: 'setup-note' }, setup.pending === 'check' ? 'Reading your repository and coding agents…' : 'Not checked yet.');
  else if (repo && repo.ok) {
    fill(rows,
      el('dt', null, 'Repository'), el('dd', null, el('code', null, text(repo.repo, 'Not recorded'))),
      el('dt', null, 'Default branch'), el('dd', null, el('code', null, text(repo.defaultBranch, 'Not recorded'))),
      el('dt', null, 'GitHub'), el('dd', null, text(repo.message, 'Signed in')));
  } else note = el('p', { class: 'setup-note setup-warn' }, text(repo && repo.message, 'The repository could not be read.'));
  const done = view.step !== 'repository';
  return card('1. Repository', note, rows.children.length ? rows : null,
    el('p', { class: 'setup-hint' }, 'Read-only. Sign-in stays in the gh CLI; Forgecell never asks for credentials.'),
    form('repository', () => submit('check', {}, 'repository'), el('div', { class: 'setup-actions' }, submitButton('check', 'Check Again', !done))));
}

function agentCard(view) {
  const agents = (Array.isArray(view.agents) ? view.agents : []).filter((a) => a && AGENT_IDS.includes(a.id));
  const firstReady = agents.find((a) => a.ready === true);
  const ready = agents.filter((a) => a.ready === true).length;
  return card('2. Coding agent',
    el('p', { class: 'setup-note' }, ready ? 'Pick the coding agent that writes changes. You can change it later with a reviewed Formula change.' : 'No coding agent is ready yet. Install one and sign in with its own CLI, then Check Again.'),
    form('agent', (f) => { const picked = f.querySelector('input[name="harness"]:checked'); if (picked) submit('create', { harness: picked.value }, 'agent'); },
      el('fieldset', { class: 'setup-options' }, el('legend', { class: 'sr-only' }, 'Coding agents'),
        agents.map((a) => el('label', { class: 'setup-option', for: fieldId('harness-' + a.id), dataset: { ready: a.ready === true ? 'yes' : 'no' } },
          el('input', { type: 'radio', id: fieldId('harness-' + a.id), name: 'harness', value: a.id, required: true, disabled: a.ready !== true, checked: a === firstReady, 'aria-describedby': 'setup-agent-' + a.id }),
          el('span', { class: 'setup-option-text' },
            el('strong', null, text(a.name, a.id)),
            el('small', { id: 'setup-agent-' + a.id }, agentLine(a)))))),
      el('p', { class: 'setup-hint' }, '3. Create the Lab: Forgecell saves the Lab and proposes its first Formula, as forgecell init --harness does. Nothing is approved yet.'),
      el('div', { class: 'setup-actions' }, ready ? submitButton('create', 'Create Lab', true) : null)));
}

const SIGN_IN = { verified: 'Signed in', 'login-required': 'Sign-in needed', unknown: 'Sign-in not verified' };
function agentLine(a) {
  const parts = [a.ready === true ? 'Ready' : 'Not available', SIGN_IN[a.signIn] || 'Sign-in not verified'];
  return parts.join(' · ') + (a.ready === true ? '' : '. ' + text(a.detail, ''));
}

function reviewCard(view) {
  const p = view.proposal || {};
  const valid = ASSAY_ID.test(p.id || '') && DIGEST.test(p.digest || '');
  const atoms = Array.isArray(p.atoms) ? p.atoms : [];
  const checks = Array.isArray(p.checks) ? p.checks : [];
  const gates = Array.isArray(p.gates) ? p.gates : [];
  const title = p.kind === 'enable-review' ? 'Add the review Atom' : '4. Review and approve the first Formula';
  const intro = p.resume ? 'An approval of this Formula started and did not finish. Approving again finishes exactly that approval.'
    : p.kind === 'enable-review' ? 'This change inserts one review Atom after the checks. Everything else stays as you approved it.'
    : 'Formula ready for your approval. Approving binds the coding agent; every run still stops for your plan approval and your review.';
  return card(title,
    el('p', { class: 'setup-note' }, intro),
    el('p', { class: 'setup-formula' }, text(p.formula, 'Formula'), ' · coding agent ', el('code', null, text(p.binding, 'Not recorded'))),
    el('h3', { class: 'setup-sub' }, 'Atoms, in order'),
    el('ol', { class: 'setup-atoms' }, atoms.map((a) => el('li', null, text(a.label, 'Atom'), ' ', el('small', null, text(a.type, ''))))),
    el('h3', { class: 'setup-sub' }, 'Baseline checks from CI ', el('span', { class: 'setup-count' }, String(checks.length))),
    checks.length ? el('ul', { class: 'setup-checks' }, checks.map((c) => el('li', null, el('code', null, text(c.command, '')), c.dir && c.dir !== '.' ? el('small', null, ' in ' + c.dir) : null)))
      : el('p', { class: 'setup-hint' }, 'No pull-request checks were found in CI, so no baseline is required.'),
    el('h3', { class: 'setup-sub' }, 'Gate policy'),
    el('ul', { class: 'setup-gates' }, gates.map((g) => el('li', null, gateLine(g)))),
    p.truncated ? el('p', { class: 'setup-hint' }, 'Only the first part is shown. Run forgecell init --review to read it all before approving.') : null,
    el('details', { class: 'setup-details' }, el('summary', null, 'Details'),
      el('p', { class: 'setup-hint' }, 'Approval binds this exact proposal. If it changes before you click, nothing is approved.'),
      el('div', { class: 'digest-row' }, el('span', { class: 'field-label' }, 'Digest'), el('code', null, short(p.digest)),
        el('button', { type: 'button', class: 'btn btn-secondary btn-small', onclick: () => { if (navigator.clipboard && valid) navigator.clipboard.writeText(p.digest).catch(() => {}); } }, 'Copy Digest'))),
    form('review', (f) => {
      if (!valid) return;
      setup.review = p.kind === 'bootstrap' && Boolean(f.querySelector('input[name="review-atom"]:checked'));
      approve(p.id, p.digest, setup.review);
    },
      p.kind === 'bootstrap' ? el('label', { class: 'setup-toggle', for: fieldId('review-atom') }, el('input', { type: 'checkbox', id: fieldId('review-atom'), name: 'review-atom', checked: setup.review }), ' Also propose the review Atom after this approval. You approve it separately.') : null,
      el('div', { class: 'setup-actions' }, valid ? submitButton('approve', p.resume ? 'Finish Approval' : 'Approve Formula', true) : null)));
}

function gateLine(g) {
  const what = g.purpose === 'scope' ? 'Plan approval' : g.purpose === 'review' ? 'Review before a draft PR' : 'Gate ' + text(g.purpose, '');
  return what + ': ' + (g.mode === 'human' ? 'you decide' : 'policy ' + text(g.mode, ''));
}

// approve sends exactly the shown proposal and digest. Only after it is
// approved, and only if you asked, the review Atom is proposed: a second
// proposal, which waits for its own Approve Formula click.
async function approve(id, digest, withReview) {
  const ok = await submit('approve', { proposal: id, digest: digest }, 'review');
  if (ok && withReview && setup.view && setup.view.step === 'done' && setup.view.reviewAtom === 'available') {
    setup.review = false;
    await submit('review', {}, 'done');
  }
}

function doneCard(view) {
  const offer = view.reviewAtom === 'available';
  return card('5. Done',
    el('p', { class: 'setup-note' }, 'Your Lab is ready. Pick a first small Issue to try the whole loop: plan, your approval, coding, checks and your review.'),
    view.problem ? el('p', { class: 'setup-note setup-warn' }, view.problem) : null,
    el('div', { class: 'setup-actions' }, el('a', { class: 'btn btn-primary', href: '?view=issues' }, 'Go to Issues')),
    offer ? form('done', () => submit('review', {}, 'done'),
      el('p', { class: 'setup-hint' }, 'Optional: a review Atom has a second agent review each change before you do. It is a Formula change you approve separately.'),
      el('div', { class: 'setup-actions' }, submitButton('review', 'Propose Review Atom', false))) : null);
}
