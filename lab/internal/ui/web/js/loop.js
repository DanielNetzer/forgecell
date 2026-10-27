// ---- Loop: start intake, start or stop the loop ----------------------------------
// The side panel's Run the loop and Start intake forms, and the header's loop
// pill. The same contract as the gates: one request in flight, the controls
// disabled meanwhile, and after an ambiguous response (none, or a 5xx) the
// state is read back and never resubmitted. A run shows as started only once
// the server's ledger read-back finds its Molecule; the loop's report comes
// from the engine.
import { $, el, fill, state, ctx, notRecorded, SessionError, tabSession, api, failed, setText } from './core.js';
import { reasonText, refusalText } from './core-gates.js';

// where names the form (run or loop) whose status shows the pending note and
// whose inline error shows the last message; it changes only what is displayed
// where. announce moves focus to that error once, when a message is set.
const drive = { panel: null, controls: null, loaded: false, pending: false, where: 'run', message: '', announce: false, mode: 'oldest', runIssue: null, run: null, loop: null, timer: null, token: 0 };

// driveMessage shows text as the inline error of the form drive.where names,
// and moves focus to it.
function driveMessage(text) {
  drive.message = text;
  drive.announce = Boolean(text);
  renderDrive();
}

async function drivePost(path, body) {
  const session = tabSession();
  if (!session || state.sessionLost) throw new SessionError('no session');
  const res = await fetch(path, { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'X-Forgecell-Session': session, 'Content-Type': 'application/json' }, body: body });
  if (res.status === 403) throw new SessionError('forbidden');
  return res;
}

async function submitDrive(path, payload) {
  if (drive.pending) return null;
  drive.pending = true;
  drive.where = path === '/api/runs' ? 'run' : 'loop';
  drive.message = '';
  renderDrive();
  let res = null, body = {};
  try {
    res = await drivePost(path, payload);
    body = await res.json().catch(() => ({}));
  } catch (error) {
    if (error instanceof SessionError) { drive.pending = false; failed(error); renderDrive(); return null; }
    res = null;
  }
  drive.pending = false;
  if (!res || res.status >= 500) {
    drive.message = 'The response was lost or ambiguous. The state is read back; nothing is resubmitted.';
    drive.announce = true;
    readDrive();
    return null;
  }
  if (!res.ok) { driveMessage(refusalText(body)); return null; }
  return body;
}

// readDrive reads the loop and the tracked run back; while either runs it reads
// again, one timer at a time, and a response older than the newest is dropped.
async function readDrive() {
  clearTimeout(drive.timer);
  const token = ++drive.token;
  let loop = drive.loop, run = drive.run;
  try {
    const loopRes = await api('/api/loop');
    loop = loopRes.ok ? await loopRes.json() : null;
    if (drive.runIssue) {
      const runRes = await api('/api/runs/' + drive.runIssue);
      run = runRes.ok ? await runRes.json() : null;
    }
  } catch (error) { failed(error); }
  if (token !== drive.token) return;
  drive.loop = loop;
  drive.run = run;
  const busy = (loop && (loop.state === 'running' || loop.state === 'draining')) || (run && run.state === 'running');
  if (busy && !state.sessionLost) drive.timer = setTimeout(readDrive, 2000);
  renderDrive();
}

async function startRun(text) {
  const value = String(text).trim();
  if (!drive.pending) drive.where = 'run';
  if (!/^[1-9][0-9]{0,9}$/.test(value)) { driveMessage('Enter an Issue number, such as 245.'); return; }
  drive.runIssue = Number(value);
  drive.run = null;
  const job = await submitDrive('/api/runs', JSON.stringify({ issue: drive.runIssue }));
  if (job) { drive.run = job; readDrive(); }
}

async function startLoop(select, max) {
  const n = Number(max);
  if (!drive.pending) drive.where = 'loop';
  if (!Number.isInteger(n) || n < 1) { driveMessage('Enter how many Molecules to advance at once.'); return; }
  const loop = await submitDrive('/api/loop/start', JSON.stringify({ select: String(select).trim(), maxConcurrent: n }));
  if (loop) { drive.loop = loop; readDrive(); }
}

async function stopLoop() {
  const loop = await submitDrive('/api/loop/stop', '{}');
  if (loop) { drive.loop = loop; readDrive(); }
}

// The Run-the-loop selector: Oldest, or a milestone title or label name from
// the value field, as the same selector string the loop takes; the server
// parses and validates it again.
const LOOP_MODES = {
  oldest: { label: 'Oldest', field: '', placeholder: '' },
  milestone: { label: 'Milestone', field: 'Milestone title', placeholder: 'v1…' },
  label: { label: 'Label', field: 'Label name', placeholder: 'ready…' },
};
function loopSelector(mode, value) { return mode === 'oldest' ? 'oldest' : mode + ':' + String(value).trim(); }

// submitLoop is the Run-the-loop form's submit handler, so Enter submits. The
// CSP sets form-action 'none': the form never navigates, it posts through
// startLoop exactly as before.
function submitLoop(e) {
  e.preventDefault();
  const c = drive.controls;
  if (drive.mode !== 'oldest' && !c.value.value.trim()) {
    if (!drive.pending) drive.where = 'loop';
    driveMessage('Enter a ' + LOOP_MODES[drive.mode].field.toLowerCase() + ', or choose Oldest.');
    return;
  }
  startLoop(loopSelector(drive.mode, c.value.value), c.max.value);
}

function submitRun(e) {
  e.preventDefault();
  startRun(drive.controls.issue.value);
}

function chooseMode(mode) {
  const c = drive.controls, m = LOOP_MODES[mode];
  if (!c || !m) return;
  drive.mode = mode;
  for (const b of c.modes) b.setAttribute('aria-pressed', String(b.dataset.drive === 'mode-' + mode));
  c.valueField.hidden = mode === 'oldest';
  setText(c.valueLabel, m.field || 'Value');
  c.value.placeholder = m.placeholder;
}

// ensureDrive builds the side panel's two forms once: Run the loop, which the
// header's Start loop link points at, and Start intake by Issue number. Their
// controls persist, so a poll never rebuilds an input being typed in. Stopping
// the loop is behind the header pill's disclosure and confirmation only.
function ensureDrive() {
  if (drive.panel) return;
  const modes = Object.entries(LOOP_MODES).map(([mode, m]) => el('button', { type: 'button', class: 'seg', 'aria-pressed': String(mode === drive.mode), dataset: { drive: 'mode-' + mode }, onclick: () => chooseMode(mode) }, m.label));
  const value = el('input', { type: 'text', id: 'drive-value', name: 'value', maxlength: '200', autocomplete: 'off', spellcheck: 'false', placeholder: 'v1…', 'aria-describedby': 'drive-loop-error', dataset: { drive: 'value' } });
  const valueLabel = el('label', { class: 'field-label', for: 'drive-value' }, 'Milestone title');
  const valueField = el('div', { class: 'drive-field' }, valueLabel, value);
  const max = el('input', { type: 'number', id: 'drive-max', name: 'maxConcurrent', min: '1', max: '8', step: '1', value: '1', inputmode: 'numeric', autocomplete: 'off', 'aria-describedby': 'drive-loop-error', dataset: { drive: 'max' } });
  // The stepper clamps to 1 and the input's max; startLoop still validates.
  const step = (delta) => {
    const limit = Number(max.max), n = Number(max.value);
    const next = Number.isInteger(n) ? n + delta : 1;
    max.value = String(Math.max(1, Math.min(Number.isInteger(limit) ? limit : 8, next)));
  };
  const fewer = el('button', { type: 'button', class: 'stepper-button', 'aria-label': 'Fewer at once', 'aria-controls': 'drive-max', dataset: { drive: 'fewer' }, onclick: () => step(-1) }, '−');
  const more = el('button', { type: 'button', class: 'stepper-button', 'aria-label': 'More at once', 'aria-controls': 'drive-max', dataset: { drive: 'more' }, onclick: () => step(1) }, '+');
  const start = el('button', { type: 'submit', class: 'btn btn-primary drive-submit', dataset: { drive: 'start' } }, 'Start loop');
  const loopError = el('p', { class: 'drive-error', id: 'drive-loop-error', tabindex: '-1', hidden: true });
  const loopStatus = el('div', { class: 'drive-status', role: 'status' });
  const issue = el('input', { type: 'text', id: 'drive-issue', name: 'issue', inputmode: 'numeric', maxlength: '10', autocomplete: 'off', placeholder: '245…', 'aria-describedby': 'drive-run-error', dataset: { drive: 'issue' } });
  const run = el('button', { type: 'submit', class: 'btn btn-secondary', dataset: { drive: 'run' } }, 'Start intake');
  const runError = el('p', { class: 'drive-error', id: 'drive-run-error', tabindex: '-1', hidden: true });
  const runStatus = el('div', { class: 'drive-status', role: 'status' });
  drive.controls = { modes: modes, value: value, valueLabel: valueLabel, valueField: valueField, max: max, start: start, loopError: loopError, loopStatus: loopStatus, issue: issue, run: run, runError: runError, runStatus: runStatus };
  drive.panel = el('div', { class: 'drive-forms', id: 'drive-panel' },
    el('form', { class: 'drive-panel drive-form', id: 'drive-loop', tabindex: '-1', 'aria-labelledby': 'drive-loop-title', novalidate: true, onsubmit: submitLoop },
      el('div', null,
        el('h2', { class: 'drive-title', id: 'drive-loop-title' }, 'Run the loop'),
        el('p', { class: 'drive-help' }, 'Plans each eligible Issue, then waits for your approval.')),
      el('div', { class: 'segmented', role: 'group', 'aria-label': 'Which Issues' }, modes),
      valueField,
      el('div', { class: 'drive-at-once' },
        el('label', { class: 'field-label', for: 'drive-max' }, 'At once'),
        el('div', { class: 'stepper' }, fewer, max, more)),
      start, loopError, loopStatus,
      el('p', { class: 'drive-help' }, 'Coding runs in parallel. Checks wait their turn, one at a time. The loop approves only what the Formula policy approves.')),
    el('form', { class: 'drive-panel drive-form', id: 'drive-run', 'aria-labelledby': 'drive-run-title', novalidate: true, onsubmit: submitRun },
      el('h2', { class: 'drive-title', id: 'drive-run-title' }, 'Start intake'),
      el('div', { class: 'drive-row' },
        el('div', { class: 'drive-field' }, el('label', { class: 'field-label', for: 'drive-issue' }, 'Issue number'), issue),
        run),
      runError, runStatus,
      el('p', { class: 'drive-help' }, 'Intake plans one Issue and stops at the scope gate for your approval.')));
  fill($('#drive-slot'), drive.panel);
  chooseMode(drive.mode);
}

function runText(run) {
  if (!run) return null;
  const c = run.canonical || {};
  let status;
  if (c.unreadable) status = 'The ledger could not be read back; nothing is shown as started.';
  else if (run.committed) status = 'Started: the ledger records a Molecule for this Issue' + (c.lane === 'decision' ? ', waiting for your approval.' : '.');
  else if (run.state === 'running') status = 'Running. Not shown as started until a ledger for this Issue appears' + (c.incomplete ? '; the ledger inventory is incomplete' : '') + '.';
  else status = 'No ledger from this run was found' + (c.incomplete ? '; the ledger inventory is incomplete' : '') + '.';
  return el('div', { class: 'gate-job' },
    el('strong', null, 'Intake · Issue #' + notRecorded(run.issue) + ' · ' + notRecorded(run.state)),
    el('p', { class: 'scope-note' }, status),
    run.error ? el('p', { class: 'scope-note' }, (run.category ? run.category + ': ' : '') + run.error + '.' + reasonText(run)) : null,
    run.committed && c.moleculeId ? el('button', { type: 'button', class: 'text-button', dataset: { drive: 'open' }, onclick: () => ctx.openMolecule(c.moleculeId) }, 'Open Molecule') : null);
}

function loopText(loop) {
  if (!loop || loop.state === 'idle') return null;
  const r = loop.report;
  const items = r && Array.isArray(r.items) ? r.items : [];
  let status = '';
  if (loop.state === 'draining') status = 'Stopping: no new Issue is dispatched; Molecules in flight finish.';
  else if (loop.state === 'running') status = 'Running. Molecules appear in the Inbox as their ledgers are written.';
  else if (r) status = 'Final pass: ' + items.length + ' eligible Issue(s)' + (r.truncated ? ', list truncated' : '') + ' after ' + notRecorded(r.passes) + ' pass(es).';
  return el('div', { class: 'gate-job' },
    el('strong', null, 'Loop · ' + notRecorded(loop.state) + (loop.select ? ' · select ' + loop.select : '') + (loop.maxConcurrent ? ' · ' + loop.maxConcurrent + ' at once' : '')),
    status ? el('p', { class: 'scope-note' }, status) : null,
    loop.error ? el('p', { class: 'scope-note' }, (loop.category ? loop.category + ': ' : '') + loop.error + '.' + reasonText(loop)) : null,
    items.length ? el('ul', { class: 'diagnostics' }, items.slice(0, 20).map((i) => el('li', null, '#' + notRecorded(i.issue) + ' · ' + notRecorded(i.state) + (i.started ? ' · started intake' : '') + (i.approved ? ' · approved by policy' : '')))) : null);
}

function renderDrive() {
  if (!drive.panel) return;
  const c = drive.controls, loop = drive.loop;
  const looping = Boolean(loop && (loop.state === 'running' || loop.state === 'draining'));
  // A submit button stays enabled until its request starts, then shows progress.
  c.run.disabled = drive.pending || Boolean(drive.run && drive.run.state === 'running');
  c.start.disabled = drive.pending || looping;
  for (const b of [c.run, c.start]) b.setAttribute('aria-disabled', String(b.disabled));
  setText(c.run, drive.pending && drive.where === 'run' ? 'Starting…' : 'Start intake');
  setText(c.start, drive.pending && drive.where === 'loop' ? 'Starting…' : looping ? 'Loop is running' : 'Start loop');
  if (loop && Number.isInteger(loop.maxConcurrentLimit)) c.max.max = String(loop.maxConcurrentLimit);
  // The message is the inline error of the form it belongs to, linked from its
  // inputs by aria-describedby; focus moves to it once when it is set.
  for (const [where, error] of [['run', c.runError], ['loop', c.loopError]]) {
    const text = drive.where === where ? drive.message : '';
    setText(error, text);
    error.hidden = !text;
    if (text && drive.announce) { drive.announce = false; error.focus(); }
  }
  const pending = (where) => drive.pending && drive.where === where ? el('p', { class: 'scope-note' }, 'Submitting…') : null;
  fill(c.runStatus, pending('run'), runText(drive.run));
  fill(c.loopStatus, pending('loop'), loopText(loop));
  renderLoopPill();
}

// The loop pill reads the loop this UI runs: idle with a Start loop link to the
// Run-the-loop form, or running with N in flight and Stop behind a disclosure
// and a confirmation that names the loop. Stop reuses stopLoop unchanged.
function renderLoopPill() {
  const pill = $('#loop-pill');
  if (!drive.panel) { pill.hidden = true; return; }
  pill.hidden = false;
  const loop = drive.loop;
  const phase = loop && (loop.state === 'running' || loop.state === 'draining') ? loop.state : 'idle';
  const inFlight = (state.list && state.list.counts && state.list.counts.running) || 0;
  if (pill.dataset.loop !== phase) pill.dataset.loop = phase;
  setText($('#loop-pill-text'), phase === 'idle' ? 'Loop is idle' : (phase === 'draining' ? 'Loop stopping · ' : 'Loop running · ') + inFlight + ' in flight');
  $('#loop-start-link').hidden = phase !== 'idle';
  const stop = $('#loop-stop');
  if (phase !== 'running' && !stop.hidden) {
    if (stop.contains(document.activeElement)) pill.focus({ preventScroll: true });
    stop.open = false;
    stop.hidden = true;
  } else if (phase === 'running') stop.hidden = false;
  setText($('#loop-stop-question'), 'Stop the loop' + (loop && loop.select ? ' on “' + loop.select + '”' : '') + '?');
  const confirm = $('#loop-stop-confirm');
  confirm.disabled = drive.pending;
  setText(confirm, drive.pending && drive.where === 'loop' ? 'Stopping…' : 'Stop loop');
}

// The loop mounts on the side panel's #drive-slot; it also owns the header's
// loop pill, which the shell repaints through ctx.renderLoopPill.
export function mount(root, ctx) {
  ctx.renderLoopPill = renderLoopPill;
  $('#loop-stop-confirm').addEventListener('click', stopLoop);
  $('#loop-start-link').addEventListener('click', () => {
    // The form is in the side panel, beside every view; focus its chosen scope.
    const input = document.querySelector('#drive-loop [aria-pressed="true"]');
    if (input) setTimeout(() => input.focus({ preventScroll: true }), 0);
  });
}

// update shows the forms and the pill when the Lab drives work through the
// engine, and reads the loop back once.
export function update(state) {
  const lab = state.lab;
  const sample = Boolean(lab.sample);
  const drives = !sample && Array.isArray(lab.actions) && lab.actions.length > 0;
  $('.readonly-badge').hidden = drives;
  if (!drives) $('#loop-pill').hidden = true;
  if (drives) {
    ensureDrive();
    renderDrive();
    if (!drive.loaded) { drive.loaded = true; readDrive(); }
  }
}
