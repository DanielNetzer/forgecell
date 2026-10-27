// ---- Loop: start intake, start or stop the loop ----------------------------------
// The side panel's Run the loop and Start intake forms, and the header's loop
// pill. The same contract as the gates: one request in flight, the controls
// disabled meanwhile, and after an ambiguous response (none, or a 5xx) the
// state is read back and never resubmitted. A run shows as started only once
// the server's ledger read-back finds its Molecule; the loop's report comes
// from the engine.
import { $, el, fill, state, ctx, notRecorded, SessionError, tabSession, api, failed, setText, setAttr } from './core.js';
import { reasonText, refusalText } from './core-gates.js';

// where names the form (run or loop) whose status shows the pending note and
// whose inline error shows the last message; it changes only what is displayed
// where. announce moves focus to that error once, when a message is set.
const drive = { panel: null, controls: null, loaded: false, pending: false, where: 'run', message: '', announce: false, mode: 'milestone', modeChosen: false, defaultSelect: null, runIssue: null, run: null, loop: null, timer: null, token: 0, closed: null, closedReport: null, closedConfirm: false };

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
  drive.where = path === '/api/runs' ? 'run' : path === '/api/retire-closed' ? 'closed' : 'loop';
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
  const busy = (loop && (loop.state === 'running' || loop.state === 'draining' || (loop.labLoop && loop.labLoop.running))) || (run && run.state === 'running');
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

// ---- Retire attempts for closed Issues ---------------------------------------------
// List reads the exact list; Retire opens the confirmation that names the
// count; Confirm retire posts that count and the list's digest, so a list that
// changed meanwhile is refused. Each Molecule's result is shown on its own.
async function listClosed() {
  if (drive.pending) return;
  drive.where = 'closed';
  drive.closedConfirm = false;
  drive.closedReport = null;
  try {
    const res = await api('/api/retire-closed');
    drive.closed = res.ok ? await res.json() : null;
    if (!res.ok) drive.message = 'The list could not be read; nothing was retired.';
  } catch (error) { failed(error); }
  renderDrive();
}

async function retireClosed() {
  const list = drive.closed;
  if (!list || !list.available || !drive.closedConfirm) return;
  const report = await submitDrive('/api/retire-closed', JSON.stringify({ count: list.count, digest: list.digest }));
  drive.closedConfirm = false;
  if (report) { drive.closedReport = report; drive.closed = null; }
  renderDrive();
}

function closedText() {
  const list = drive.closed, report = drive.closedReport;
  if (report) {
    const results = Array.isArray(report.results) ? report.results : [];
    return el('div', { class: 'gate-job' },
      el('strong', null, 'Retired ' + notRecorded(report.retired) + ' of ' + notRecorded(report.attempted) + (report.refused ? ' · ' + report.refused + ' refused' : '') + (report.notAttempted ? ' · ' + report.notAttempted + ' not attempted (time bound reached)' : '')),
      el('ul', { class: 'diagnostics' }, results.map((r) => el('li', null, notRecorded(r.id) + ' · ' + (r.committed ? 'retired' : 'not retired: ' + (r.category ? r.category + ': ' : '') + notRecorded(r.error))))),
      report.truncated ? el('p', { class: 'scope-note' }, 'More attempts remain; list them again.') : null);
  }
  if (!list) return null;
  if (!list.available) return el('p', { class: 'scope-note' }, 'Unavailable: ' + notRecorded(list.reason) + '.');
  const items = Array.isArray(list.molecules) ? list.molecules : [];
  if (!items.length) return el('p', { class: 'scope-note' }, 'No attempt for a closed Issue needs retiring.');
  return el('div', { class: 'gate-job' },
    el('strong', null, items.length + ' attempt(s) for closed Issues' + (list.truncated ? ' (first ' + items.length + ')' : '')),
    el('ul', { class: 'diagnostics' }, items.map((m) => el('li', null, '#' + notRecorded(m.issue) + ' · ' + notRecorded(m.id) + ' · ' + notRecorded(m.lane)))),
    drive.closedConfirm
      ? el('div', { class: 'confirm-actions' },
        el('button', { type: 'button', class: 'btn btn-danger', dataset: { drive: 'closed-confirm' }, onclick: retireClosed }, 'Confirm retire ' + items.length),
        el('button', { type: 'button', class: 'btn btn-secondary', dataset: { drive: 'closed-cancel' }, onclick: () => { drive.closedConfirm = false; renderDrive(); } }, 'Cancel'))
      : el('button', { type: 'button', class: 'btn btn-danger', dataset: { drive: 'closed-retire' }, onclick: () => { drive.closedConfirm = true; renderDrive(); } }, 'Retire ' + items.length + '…'),
    el('p', { class: 'drive-help' }, 'Each is retired with the reason “Issue closed; superseded attempt”. Every record is kept.'));
}

// labLoopText names the Lab's loop: who started it, its selector, how many at
// once, when it started and what it has in flight.
function labLoopText(lab) {
  if (!lab || !lab.running) return null;
  const inFlight = (Array.isArray(lab.inFlight) ? lab.inFlight : []).concat((Array.isArray(lab.intake) ? lab.intake : []).map((i) => 'issue #' + notRecorded(i.issue) + ' · planning'));
  return el('div', { class: 'gate-job' },
    el('strong', null, notRecorded(lab.ownerText) + (lab.maxConcurrent ? ' · ' + lab.maxConcurrent + ' at once' : '') + (lab.startedAt ? ' · started ' + lab.startedAt : '')),
    inFlight.length ? el('ul', { class: 'diagnostics' }, inFlight.map((x) => el('li', null, x))) : el('p', { class: 'scope-note' }, 'Nothing in flight.'),
    lab.inFlightTruncated ? el('p', { class: 'scope-note' }, 'More are in flight than shown.') : null);
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

// chooseMode is the operator's choice: from then on the server's default
// selector never replaces it.
function chooseMode(mode) {
  if (!LOOP_MODES[mode]) return;
  drive.modeChosen = true;
  showMode(mode);
}

// applyDefaultSelect shows the server's default selector (milestone:<active
// milestone>, oldest when the repository has no open milestone, or "" when
// the active milestone is unknown: Milestone with an empty title) until the
// operator chooses or types a selector.
function applyDefaultSelect(loop) {
  const c = drive.controls;
  if (!c || drive.modeChosen || !loop || typeof loop.defaultSelect !== 'string' || loop.defaultSelect === drive.defaultSelect) return;
  drive.defaultSelect = loop.defaultSelect;
  if (loop.defaultSelect === 'oldest') { showMode('oldest'); return; }
  const title = loop.defaultSelect.startsWith('milestone:') ? loop.defaultSelect.slice('milestone:'.length) : '';
  showMode('milestone');
  c.value.value = title;
}

function showMode(mode) {
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
  const value = el('input', { type: 'text', id: 'drive-value', name: 'value', maxlength: '200', autocomplete: 'off', spellcheck: 'false', placeholder: 'v1…', 'aria-describedby': 'drive-loop-error', dataset: { drive: 'value' }, oninput: () => { drive.modeChosen = true; } });
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
  const selectNote = el('p', { class: 'drive-help', dataset: { drive: 'select-note' } }, 'Oldest: oldest open Issue, any milestone.');
  const closedList = el('button', { type: 'button', class: 'btn btn-secondary', dataset: { drive: 'closed-list' }, onclick: listClosed }, 'List attempts for closed Issues');
  const closedError = el('p', { class: 'drive-error', id: 'drive-closed-error', tabindex: '-1', hidden: true });
  const closedStatus = el('div', { class: 'drive-status', role: 'status' });
  const closedForm = el('div', { class: 'drive-panel drive-form drive-closed', id: 'drive-closed', 'aria-labelledby': 'drive-closed-title', hidden: true },
    el('h2', { class: 'drive-title', id: 'drive-closed-title' }, 'Retire attempts for closed Issues'),
    el('p', { class: 'drive-help' }, 'Lists every attempt that is not running, has no delivery in progress and whose Issue GitHub reports closed.'),
    closedList, closedError, closedStatus);
  drive.controls = { modes: modes, value: value, valueLabel: valueLabel, valueField: valueField, max: max, start: start, loopError: loopError, loopStatus: loopStatus, issue: issue, run: run, runError: runError, runStatus: runStatus, selectNote: selectNote, closedForm: closedForm, closedList: closedList, closedError: closedError, closedStatus: closedStatus };
  drive.panel = el('div', { class: 'drive-forms', id: 'drive-panel' },
    el('form', { class: 'drive-panel drive-form', id: 'drive-loop', tabindex: '-1', 'aria-labelledby': 'drive-loop-title', novalidate: true, onsubmit: submitLoop },
      el('div', null,
        el('h2', { class: 'drive-title', id: 'drive-loop-title' }, 'Run the loop'),
        el('p', { class: 'drive-help' }, 'Plans each eligible Issue, then waits for your approval.')),
      el('div', { class: 'segmented', role: 'group', 'aria-label': 'Which Issues' }, modes),
      selectNote,
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
      el('p', { class: 'drive-help' }, 'Intake plans one Issue and stops at the scope gate for your approval.')),
    closedForm);
  fill($('#drive-slot'), drive.panel);
  showMode(drive.mode);
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
  if (!loop) return null;
  // A loop started elsewhere in the Lab is named with its owner and where to stop it.
  if (loop.state === 'idle') return labLoopText(loop.labLoop);
  const r = loop.report;
  const items = r && Array.isArray(r.items) ? r.items : [];
  let status = '';
  if (loop.state === 'draining') status = 'Stopping: no new Issue is dispatched; Molecules in flight finish.';
  else if (loop.state === 'running') status = 'Running. Molecules appear in the Inbox as their ledgers are written.';
  else if (r) status = 'Final pass: ' + items.length + ' eligible Issue(s)' + (r.truncated ? ', list truncated' : '') + ' after ' + notRecorded(r.passes) + ' pass(es).';
  return el('div', { class: 'gate-job' },
    labLoopText(loop.labLoop),
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
  if (loop && typeof loop.defaultSelectNote === 'string' && loop.defaultSelectNote) setText(c.selectNote, loop.defaultSelectNote);
  applyDefaultSelect(loop);
  c.closedForm.hidden = !(state.lab && Array.isArray(state.lab.actions) && state.lab.actions.includes('retire closed Issues'));
  c.closedList.disabled = drive.pending;
  for (const [where, error] of [['run', c.runError], ['loop', c.loopError], ['closed', c.closedError]]) {
    const text = drive.where === where ? drive.message : '';
    setText(error, text);
    error.hidden = !text;
    if (text && drive.announce) { drive.announce = false; error.focus(); }
  }
  const pending = (where) => drive.pending && drive.where === where ? el('p', { class: 'scope-note' }, 'Submitting…') : null;
  fill(c.runStatus, pending('run'), runText(drive.run));
  fill(c.loopStatus, pending('loop'), loopText(loop));
  fill(c.closedStatus, pending('closed'), closedText());
  renderLoopPill();
}

// The loop pill reads any loop in this Lab (/api/loop's labLoop): idle with a
// Start loop link to the Run-the-loop form, or running with N in flight. Stop
// sits behind a disclosure and a confirmation that names the loop, and only
// when canStop; otherwise the pill's text names the owner and where to stop
// it. Stop reuses stopLoop unchanged.
function renderLoopPill() {
  const pill = $('#loop-pill');
  if (!drive.panel) { pill.hidden = true; return; }
  pill.hidden = false;
  const loop = drive.loop;
  const lab = (loop && loop.labLoop) || {};
  const own = loop && (loop.state === 'running' || loop.state === 'draining') ? loop.state : 'idle';
  const phase = own !== 'idle' ? own : lab.running === true ? 'running' : 'idle';
  const listed = (Array.isArray(lab.inFlight) ? lab.inFlight.length : 0) + (Array.isArray(lab.intake) ? lab.intake.length : 0);
  const inFlight = lab.running === true ? listed : (state.list && state.list.counts && state.list.counts.running) || 0;
  if (pill.dataset.loop !== phase) pill.dataset.loop = phase;
  const canStop = phase === 'running' && lab.canStop === true;
  const owner = lab.running === true && typeof lab.ownerText === 'string' ? lab.ownerText : '';
  // A loop this UI cannot stop is named in plain text, with where to stop it.
  const elsewhere = phase === 'running' && !canStop && owner ? ' · ' + owner : '';
  setText($('#loop-pill-text'), (phase === 'idle' ? 'Loop is idle' : (phase === 'draining' ? 'Loop stopping · ' : 'Loop running · ') + inFlight + ' in flight') + elsewhere);
  setAttr(pill, 'title', owner);
  $('#loop-start-link').hidden = phase !== 'idle';
  const stop = $('#loop-stop');
  if (!canStop && !stop.hidden) {
    if (stop.contains(document.activeElement)) pill.focus({ preventScroll: true });
    stop.open = false;
    stop.hidden = true;
  } else if (canStop) stop.hidden = false;
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
