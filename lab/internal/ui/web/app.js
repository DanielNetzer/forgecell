// Forgecell local Lab UI. It reads the Lab, and writes only through the engine:
// the two human gates (exact scope approval and draft PR publication), starting
// intake or the loop, and stopping work this UI started. Every value
// from the API is untrusted and reaches the page only as a text node or a
// validated attribute: this file never parses data as markup.
'use strict';
(() => {
  const $ = (selector) => document.querySelector(selector);

  // el builds an element. Strings among children become text nodes; props are
  // fixed by this file, never taken from data.
  function el(tag, props, ...children) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (value === undefined || value === null || value === false) continue;
      if (key === 'class') node.className = value;
      else if (key === 'text') node.textContent = String(value);
      else if (key === 'dataset') Object.assign(node.dataset, value);
      else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
      else node.setAttribute(key, value === true ? '' : String(value));
    }
    return add(node, ...children);
  }

  // add appends children to node: nodes as they are, strings as text nodes, and
  // null, undefined and false not at all, so no child ever renders as "null".
  function add(node, ...children) {
    for (const child of children.flat()) {
      if (child === undefined || child === null || child === false) continue;
      node.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return node;
  }

  // fill replaces node's children, skipping null children as add does. Every
  // render goes through fill, never through replaceChildren with arguments.
  function fill(node, ...children) {
    node.replaceChildren();
    return add(node, ...children);
  }

  const ISSUE_URL = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/issues\/[1-9][0-9]*$/;
  const PULL_URL = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/[1-9][0-9]*$/;
  const REPOSITORY = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

  function externalLink(url, pattern, label) {
    if (typeof url !== 'string' || !pattern.test(url)) return null;
    return el('a', { href: url, target: '_blank', rel: 'noopener noreferrer' }, label);
  }

  // One lane per Molecule, assigned by the server; Paused has no engine semantics.
  const LANES = {
    running: { label: 'Running', short: 'running', symbol: '', cls: 'working' },
    decision: { label: 'Needs my decision', short: 'need you', symbol: '◇', cls: 'decision' },
    blocked: { label: 'Blocked', short: 'blocked', symbol: '!', cls: 'failed' },
    finished: { label: 'Draft published', short: 'draft published', symbol: '✓', cls: 'finished' },
    other: { label: 'Other', short: 'other', symbol: '·', cls: 'other' },
  };
  // Retired is finished history: its own lane and filter, never part of All,
  // Needs my decision or the headline counts, but shown in analytics.
  const RETIRED = 'retired';
  const ALL_LANES = Object.assign({}, LANES, { retired: { label: 'Retired', short: 'retired', symbol: '–', cls: 'other' } });
  // The lane bar. Paused is not a filter: the engine has no pause semantics.
  const FILTERS = [
    ['all', 'All'], ['running', 'Running'], ['decision', 'Needs my decision'],
    ['blocked', 'Blocked'], ['finished', 'Finished · Draft published'], ['other', 'Other'], ['retired', 'Retired'],
  ];

  const state = { lab: null, list: null, etag: null, analytics: null, analyticsEtag: null, analyticsToken: 0, filter: 'all', query: '', selectedId: null, detail: null, detailText: null, atomIndex: null, detailToken: 0, labToken: 0, pollToken: 0, polling: false, noteKey: null, sessionLost: false, gate: newGate(null) };

  // gate is the selected Molecule's gate state: one request in flight at most,
  // the last preview shown, and the last job read back from the server.
  function newGate(id) { return { id: id, pending: false, preview: null, job: null, stop: null, retire: null, reason: '', message: '', title: '', accept: false, timer: null }; }

  function notRecorded(value) { return value === undefined || value === null || value === '' ? 'Not recorded' : String(value); }
  function short(digest) { return typeof digest === 'string' && digest.length > 12 ? digest.slice(0, 12) + '…' : notRecorded(digest); }
  function durationLabel(ms) {
    const total = Math.round(ms / 1000), h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60), s = total % 60;
    return [h ? h + 'h' : '', m ? m + 'm' : '', s || !total ? s + 's' : ''].filter(Boolean).join(' ');
  }
  function elapsed(start, end) {
    const a = Date.parse(start), b = Date.parse(end);
    return Number.isFinite(a) && Number.isFinite(b) && b >= a ? b - a : null;
  }

  // ---- API ----------------------------------------------------------------
  // Every API request carries this tab's session value, kept in sessionStorage
  // by session.js at launch; the cookie alone is not enough. Without it, or once
  // the server refuses it, the page stops asking instead of looping.
  class SessionError extends Error {}
  function tabSession() {
    try { return sessionStorage.getItem('forgecell-session') || ''; } catch (error) { return ''; }
  }
  async function api(path, headers) {
    const session = tabSession();
    if (!session || state.sessionLost) throw new SessionError('no session');
    const res = await fetch(path, { credentials: 'same-origin', cache: 'no-store', headers: Object.assign({ 'X-Forgecell-Session': session }, headers || {}) });
    if (res.status === 403) throw new SessionError('forbidden');
    return res;
  }
  function showError(message) {
    const banner = $('#load-error');
    banner.textContent = message;
    banner.hidden = !message;
  }
  function failed(error) {
    if (error instanceof SessionError) state.sessionLost = true;
    showError(error instanceof SessionError
      ? 'This tab has no Lab UI session. Each launch link works once: run forgecell ui again (or restart it) and open the new link it prints.'
      : 'The Lab could not be read. The page keeps the last data it loaded and will retry.');
  }

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
      if (state.selectedId) loadDetail(state.selectedId, false);
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
      renderAnalytics();
    } catch (error) { failed(error); }
  }

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

  // ---- Focus across re-renders ----------------------------------------------
  // Re-rendered controls are found again by these data attributes, or else by
  // their id, so keyboard focus survives a poll that rebuilds them.
  const FOCUS_KEYS = ['id', 'filter', 'atom', 'origin', 'review', 'evidence', 'gate', 'drive'];
  function keepFocus(render) {
    const active = document.activeElement;
    const key = active && active.dataset ? FOCUS_KEYS.find((k) => active.dataset[k] !== undefined) : undefined;
    const value = key && active.dataset[key];
    const id = active && active.id;
    render();
    if ((!key && !id) || document.contains(active)) return;
    const next = key
      ? [...document.querySelectorAll('[data-' + key + ']')].find((node) => node.dataset[key] === value && !node.hidden && !node.disabled)
      : document.getElementById(id);
    if (!next) return;
    if (key === 'id') { cells().forEach((b) => { b.tabIndex = -1; }); next.tabIndex = 0; }
    next.focus({ preventScroll: true });
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
    const f = lab.formula || {};
    const states = {
      approved: 'Approved for execution',
      none: 'No active Formula',
      'activation-locked': 'Activation in progress or interrupted; execution refuses it until resumed',
      unapproved: 'Not approved for execution: missing, stale or unfinished decision',
      unreadable: 'Formula unreadable',
      sample: 'Sample data · not a real Formula',
    };
    const box = $('#active-formula');
    fill(box,
      el('div', null,
        el('span', { class: 'label', text: 'NEW MOLECULES · ACTIVE FORMULA' }),
        el('strong', null, f.formulaId || 'None', f.sha256 ? el('span', { class: 'version-tag', title: f.sha256 }, 'sha256 ' + short(f.sha256)) : null),
        el('small', null, (states[f.state] || notRecorded(f.state)) + (f.decisionId ? ' · decision ' + f.decisionId : '') + '. Applies to future Molecules; existing Molecules keep their saved snapshot.')),
      el('div', { class: 'formula-actions' }, el('span', { class: 'unavailable-tag', title: 'Version history and rollback are not connected in this slice' }, 'Versions & history · Unavailable')));
    renderOperator(lab.operator);
    // The greeting names the operator.
    if (state.list) renderInbox();
    const drives = !sample && Array.isArray(lab.actions) && lab.actions.length > 0;
    $('.readonly-badge').hidden = drives;
    if (!drives) $('#loop-pill').hidden = true;
    if (drives) {
      ensureDrive();
      renderDrive();
      if (!drive.loaded) { drive.loaded = true; readDrive(); }
    }
  }

  // ---- Drive: start intake, start or stop the loop ---------------------------
  // The same contract as the gates: one request in flight, the controls disabled
  // meanwhile, and after an ambiguous response (none, or a 5xx) the state is read
  // back and never resubmitted. A run shows as started only once the server's
  // ledger read-back finds its Molecule; the loop's report comes from the engine.
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
      run.committed && c.moleculeId ? el('button', { type: 'button', class: 'text-button', dataset: { drive: 'open' }, onclick: () => select(c.moleculeId) }, 'Open Molecule') : null);
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

  // ---- Grid, filters and search -------------------------------------------
  function molecules() { return (state.list && state.list.molecules) || []; }
  function laneOf(m) { return ALL_LANES[m.lane] || LANES.other; }
  function live(m) { return m.lane !== RETIRED; }
  // A running Molecule waiting for the Lab's verification slot reads as queued,
  // not running. Only the one known activity is shown; any other value is ignored.
  const QUEUED = 'queued for checks';
  function queued(m) { return m.lane === 'running' && m.activity === QUEUED; }
  function laneLabel(m) { return queued(m) ? 'Queued for checks' : laneOf(m).label; }
  function statusLabel(m) {
    return laneLabel(m) + ' · ' + notRecorded(m.status) + (m.phase ? ' · ' + m.phase : '');
  }

  function matches(m) {
    const inLane = state.filter === 'all' ? live(m) : m.lane === state.filter;
    const terms = state.query.toLowerCase().trim().split(/\s+/).filter(Boolean);
    const haystack = ('#' + m.number + ' ' + (m.title || '') + ' ' + (m.formulaId || '') + ' ' + m.id).toLowerCase();
    return inLane && terms.every((term) => haystack.includes(term));
  }

  function renderList() {
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
    renderOverview();
    renderShell();
    renderInbox();
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
    fill($('#run-filters'), FILTERS.flatMap(([id, label]) => [
      id === RETIRED ? el('span', { class: 'lane-divider', 'aria-hidden': 'true' }) : null,
      el('button', {
        type: 'button', class: 'lane-segment', dataset: { filter: id }, 'aria-pressed': String(state.filter === id),
        onclick: () => { hideTip(); state.filter = id; renderFilters(); applyFilter(); $('[data-filter="' + id + '"]').focus({ preventScroll: true }); },
      }, id === 'all' ? null : el('span', { class: 'lane-dot lane-dot-' + id, 'aria-hidden': 'true' }),
      el('span', { class: 'lane-name' }, label),
      el('span', { class: 'lane-count' }, String(id === 'all' ? counts.all || 0 : counts[id] || 0))),
    ]));
  }

  function cells() { return [...document.querySelectorAll('#cubes [data-id]')]; }

  function applyFilter() {
    const byId = new Map(molecules().map((m) => [m.id, m]));
    const all = cells();
    all.forEach((b) => { b.hidden = !matches(byId.get(b.dataset.id)); b.tabIndex = -1; });
    const shown = all.filter((b) => !b.hidden);
    const chosen = shown.find((b) => b.dataset.id === state.selectedId) || shown[0];
    if (chosen) chosen.tabIndex = 0;
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

  $('#molecule-search').addEventListener('input', (e) => { hideTip(); state.query = e.target.value; applyFilter(); });
  function clearSearch() { state.query = ''; $('#molecule-search').value = ''; applyFilter(); $('#molecule-search').focus({ preventScroll: true }); }
  $('#clear-search').addEventListener('click', clearSearch);
  $('#molecule-search').addEventListener('keydown', (e) => { if (e.key === 'Escape' && state.query) { e.preventDefault(); clearSearch(); } });

  const cubes = $('#cubes');
  cubes.addEventListener('click', (e) => {
    const button = e.target.closest('[data-id]');
    if (button) { hideTip(); select(button.dataset.id); }
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
  tip.addEventListener('pointerenter', () => clearTimeout(tipTimer));
  tip.addEventListener('pointerleave', hideTip);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideTip(); });
  window.addEventListener('resize', hideTip);
  document.addEventListener('scroll', hideTip, true);

  // ---- Selection --------------------------------------------------------------
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
    $('#selected').hidden = false;
    cells().forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.id === id)));
    const m = molecules().find((x) => x.id === id);
    $('#selection-label').textContent = m ? 'Selected #' + m.number + ' · ' + (m.title || m.id) : 'Selected ' + id;
    renderLoadingDetail(id);
    applyFilter();
    loadDetail(id, true);
  }

  function overview() {
    state.selectedId = null;
    state.detail = null;
    state.detailToken++;
    clearTimeout(state.gate.timer);
    state.gate = newGate(null);
    if (moleculeFromURL()) history.replaceState({ view: state.view }, '', '?view=' + state.view);
    $('#selected').hidden = true;
    cells().forEach((b) => b.setAttribute('aria-pressed', 'false'));
    $('#selection-label').textContent = 'All Molecules · select a cell to inspect';
    applyFilter();
  }

  function renderLoadingDetail(id) {
    $('#run-id').textContent = id;
    $('#run-title').textContent = 'Loading…';
    $('#run-state').textContent = '';
    for (const s of ['#run-origin', '#molecule-totals', '#atoms', '#readiness', '#attempts']) fill($(s));
  }

  function renderMissingDetail(id, status) {
    renderLoadingDetail(id);
    $('#run-title').textContent = status === 404 ? 'This Molecule is no longer in the Lab.' : 'This ledger was refused: it is invalid, unsafe, oversized or not this Lab\'s.';
    add($('#run-origin'), el('button', { type: 'button', class: 'text-button', onclick: overview }, 'Clear selection'));
  }

  // ---- Selected Molecule -------------------------------------------------------
  function attemptsById(d) { return new Map((d.attempts || []).map((a) => [a.id, a])); }
  function costText(usage) {
    if (!usage || typeof usage.totalCostUsd !== 'number') return 'Not recorded';
    const basis = usage.costBasis === 'list' ? 'CLI list-price estimate' : usage.costBasis ? 'basis ' + usage.costBasis : 'basis unavailable';
    return '$' + usage.totalCostUsd.toFixed(4) + ' (' + basis + ')';
  }

  function renderDetail() {
    const d = state.detail;
    $('#run-id').textContent = d.id;
    const pill = $('#run-state');
    pill.textContent = statusLabel(d);
    pill.className = 'pill' + (d.lane === 'decision' ? ' current' : '');
    $('#run-title').textContent = (d.sample ? '[Sample] ' : '') + (d.issue.title || 'Untitled');
    const issueLink = externalLink(d.issue.url, ISSUE_URL, 'Issue #' + d.issue.number + ' ↗');
    const pullLink = d.delivery ? externalLink(d.delivery.url, PULL_URL, 'Pull request ↗') : null;
    fill($('#run-origin'),
      el('span', null, notRecorded(d.issue.repository) + ' #' + d.issue.number + (d.issue.state ? ' · ' + d.issue.state : '')),
      issueLink || el('span', { class: 'muted' }, 'Issue link unavailable'),
      pullLink || el('span', { class: 'muted' }, d.delivery ? 'PR link unavailable' : 'PR · not created'),
      el('button', { type: 'button', class: 'text-button', dataset: { origin: 'issue-text' }, onclick: () => show('Issue text · ' + d.id, d.issue.body || 'No Issue text recorded.') }, 'Issue text ↗'),
      d.sample ? null : el('button', { type: 'button', class: 'text-button', dataset: { origin: 'ledger' }, onclick: () => showLedger(d.id) }, 'Ledger record ↗'),
      el('button', { type: 'button', class: 'text-button', id: 'clear-selection', dataset: { origin: 'clear' }, onclick: overview }, 'Clear selection'));
    if (issueLink) issueLink.dataset.origin = 'issue';
    if (pullLink) pullLink.dataset.origin = 'pull';

    const wall = elapsed(d.startedAt, d.finishedAt);
    const attempts = d.attempts || [];
    const costs = attempts.filter((a) => a.usage && typeof a.usage.totalCostUsd === 'number');
    fill($('#molecule-totals'),
      el('div', null, el('span', { class: 'label' }, 'WALL-CLOCK DURATION'), el('strong', null, wall !== null ? durationLabel(wall) : d.startedAt ? 'Not finished' : 'Not recorded'),
        el('small', null, 'Started ' + notRecorded(d.startedAt) + (d.finishedAt ? ' · finished ' + d.finishedAt : ''))),
      el('div', null, el('span', { class: 'label' }, 'REPORTED COST · PARTIAL'),
        el('strong', null, costs.length ? '$' + costs.reduce((n, a) => n + a.usage.totalCostUsd, 0).toFixed(4) : 'Not recorded'),
        el('small', null, 'Cost reported for ' + costs.length + ' of ' + attempts.length + ' harness attempts · unknown is not zero')),
      el('div', null, el('span', { class: 'label' }, 'WORKSPACE'), el('strong', { class: 'mono-text' }, notRecorded(d.workspace.branch)),
        el('small', { title: d.workspace.baseCommit || '' }, 'Base commit ' + short(d.workspace.baseCommit))));

    $('#formula-name').textContent = notRecorded(d.formulaId) + (d.formula.approved ? ' · approved snapshot' : ' · snapshot not marked approved');
    const sha = $('#formula-sha');
    sha.textContent = 'sha256 ' + notRecorded(d.formula.sha256);
    const delivery = $('#run-delivery');
    delivery.hidden = !d.delivery;
    if (d.delivery) {
      delivery.textContent = d.delivery.label + (d.delivery.published ? ' · merge and production unverified' : '') + (d.delivery.error ? ' · ' + d.delivery.error : '');
    }

    renderAtoms(d);
    renderReadiness(d);
    renderAttempts(d);
    const diagnostics = $('#run-diagnostics');
    fill(diagnostics, (d.diagnostics || []).map((text) => el('li', null, text)));
    diagnostics.hidden = !(d.diagnostics || []).length;
    renderAction(d);
  }

  // NEXT ACTION and GATES AND DECISIONS render the server's one gate state:
  // d.next is derived from d.gate, so it names Gates and decisions only while a
  // gate there is open.
  function renderAction(d) {
    const next = d.next || {};
    $('#action-title').textContent = typeof next.title === 'string' && next.title ? next.title : 'Recorded';
    $('#action-copy').textContent = typeof next.copy === 'string' ? next.copy : '';
    renderGate(d);
  }

  // ---- Human gates ---------------------------------------------------------------
  // Each gate posts the exact digest this page showed. One request is in flight
  // at a time and the controls are disabled meanwhile. After an ambiguous
  // response (no response, or a 5xx) the page reads the canonical state back
  // and never resubmits. A decision shows as committed only when the server's
  // read-back of the ledger or the receipt says so, never because a job ended.
  const GATE_COPY = {
    scope: 'Approving runs coding and verification for exactly this plan. It never publishes, merges or deploys.',
    delivery: 'Publication creates a draft pull request only. It never merges, deploys or changes permissions.',
  };

  async function gatePost(id, action, body) {
    const session = tabSession();
    if (!session || state.sessionLost) throw new SessionError('no session');
    const res = await fetch('/api/molecules/' + encodeURIComponent(id) + action, { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'X-Forgecell-Session': session, 'Content-Type': 'application/json' }, body: body });
    if (res.status === 403) throw new SessionError('forbidden');
    return res;
  }

  // reasonText is the server's bounded, redacted next step for a refusal, or ''.
  function reasonText(body) {
    return body && typeof body.reason === 'string' && body.reason ? ' ' + body.reason + '.' : '';
  }

  function refusalText(body) {
    const category = body && typeof body.category === 'string' ? body.category : 'refused';
    return 'Refused (' + category + '): ' + notRecorded(body && body.error) + '.' + reasonText(body) + ' Nothing was resubmitted.';
  }

  function rerenderGate(gate) {
    if (state.gate === gate && state.detail && state.detail.id === gate.id) keepFocus(() => renderGate(state.detail));
  }

  // submitGate posts once. It returns the accepted body, or null after a refusal
  // or an ambiguous response, which is read back instead of retried.
  async function submitGate(action, payload) {
    if (state.gate.pending) return null;
    const gate = state.gate;
    gate.pending = true;
    gate.message = '';
    rerenderGate(gate);
    let res = null, body = {};
    try {
      res = await gatePost(gate.id, action, payload);
      body = await res.json().catch(() => ({}));
    } catch (error) {
      if (error instanceof SessionError) { gate.pending = false; failed(error); rerenderGate(gate); return null; }
      res = null;
    }
    gate.pending = false;
    if (state.gate !== gate) return null;
    if (!res || res.status >= 500) {
      gate.message = 'The response was lost or ambiguous. The canonical state is read back; nothing is resubmitted.';
      readBack(gate);
      return null;
    }
    if (!res.ok) { gate.message = refusalText(body); rerenderGate(gate); return null; }
    return body;
  }

  // fetchJob reads the job and its canonical read-back; while the job runs it
  // reads again, one timer at a time.
  async function fetchJob(gate) {
    clearTimeout(gate.timer);
    try {
      const res = await api('/api/molecules/' + encodeURIComponent(gate.id) + '/job');
      const job = res.ok ? await res.json() : null;
      if (state.gate !== gate) return;
      // A Molecule with no job from this Lab UI answers 200 with state none.
      gate.job = job && job.state !== 'none' ? job : null;
      if (job && job.state === 'running') gate.timer = setTimeout(() => readBack(gate), 2000);
    } catch (error) { failed(error); }
    rerenderGate(gate);
  }

  function readBack(gate) {
    if (state.gate !== gate) return;
    fetchJob(gate);
    loadDetail(gate.id, false);
  }

  async function approveScope(digest) {
    const job = await submitGate('/scope', JSON.stringify({ digest: digest }));
    if (job) { state.gate.job = job; readBack(state.gate); }
  }

  async function previewDelivery() {
    const gate = state.gate;
    const preview = await submitGate('/delivery/preview', '{}');
    if (preview && state.gate === gate) { gate.preview = preview; rerenderGate(gate); }
  }

  async function publishDelivery() {
    const gate = state.gate;
    const preview = gate.preview, title = gate.title.trim(), accept = gate.accept;
    if (!preview || !title) { gate.message = 'Enter a one-line title for the draft PR.'; rerenderGate(gate); return; }
    if (preview.acceptanceUnverified && !accept) { gate.message = 'Independent acceptance is unverified: confirm your own acceptance review first.'; rerenderGate(gate); return; }
    const job = await submitGate('/delivery/publish', JSON.stringify({ digest: preview.digest, title: title, acceptUnverified: accept }));
    if (job && state.gate === gate) { gate.job = job; readBack(gate); }
  }

  // stopMolecule posts once; the stop shows as done only when the ledger read back
  // no longer reports the Molecule running.
  async function stopMolecule() {
    const gate = state.gate;
    const stop = await submitGate('/stop', '{}');
    if (stop && state.gate === gate) { gate.stop = stop; rerenderGate(gate); loadDetail(gate.id, false); }
  }

  // retireMolecule posts once with the typed reason; the retirement shows as
  // committed only when the response's ledger read-back records it.
  async function retireMolecule() {
    const gate = state.gate;
    const reason = gate.reason.trim();
    if (!reason || /[\r\n]/.test(reason) || [...reason].length > 200) { gate.message = 'Enter a one-line reason of at most 200 characters.'; rerenderGate(gate); return; }
    const retired = await submitGate('/retire', JSON.stringify({ reason: reason }));
    if (retired && state.gate === gate) { gate.retire = retired; rerenderGate(gate); loadDetail(gate.id, false); }
  }

  function retireText(d, gate) {
    const r = d.retired;
    if (d.lane === RETIRED && r) {
      return el('p', { class: 'scope-note', role: gate.retire ? 'status' : null }, 'Retired at ' + notRecorded(r.at) + ' (was ' + notRecorded(r.priorStatus) + '). Reason: ' + notRecorded(r.reason) + '. Every record is kept.');
    }
    if (gate.retire) return el('p', { class: 'scope-note', role: 'status' }, gate.retire.committed ? 'Committed: the ledger records the retirement.' : 'Not committed: the ledger read back does not record the retirement.');
    return null;
  }

  function hasAction(name) { return Boolean(state.lab && Array.isArray(state.lab.actions) && state.lab.actions.includes(name)); }

  function latestDigest(d) {
    const plans = (d.readiness && d.readiness.plans) || [];
    const last = plans[plans.length - 1];
    return last && typeof last.digest === 'string' && /^[0-9a-f]{64}$/.test(last.digest) ? last.digest : '';
  }

  function jobText(job) {
    if (!job) return null;
    const c = job.canonical || {};
    const kind = job.kind === 'scope' ? 'Scope approval' : 'Draft publication';
    let status;
    if (c.unreadable) status = 'The ' + (c.source || 'record') + ' could not be read back; nothing is shown as committed.';
    else if (job.committed && job.kind === 'scope') status = 'Committed: the ledger records approval of plan ' + short(job.digest) + '.';
    else if (job.committed) status = 'Committed: receipt ' + notRecorded(c.receipt) + ', lifecycle ' + notRecorded(c.lifecycle) + (c.published ? '. Draft published; merge and production are not verified.' : '. Not published as a draft yet.');
    else if (job.state === 'running') status = 'Running. Not committed until the ' + (job.kind === 'scope' ? 'ledger records the approval.' : 'receipt records it.');
    else status = 'Not committed: the ' + (job.kind === 'scope' ? 'ledger does not record this approval' : 'receipt does not record a committed delivery') + (c.receipt ? ' (receipt ' + c.receipt + (c.lifecycle ? ', lifecycle ' + c.lifecycle : '') + ')' : '') + '.';
    return el('div', { class: 'gate-job', role: 'status' },
      el('strong', null, kind + ' · ' + notRecorded(job.state)),
      el('p', { class: 'scope-note' }, status),
      job.error ? el('p', { class: 'scope-note' }, (job.category ? job.category + ': ' : '') + job.error + '.' + reasonText(job)) : null);
  }

  function renderGate(d) {
    const card = document.querySelector('.decision-panel .action-card');
    if (!card) return;
    if (state.gate.id !== d.id) {
      clearTimeout(state.gate.timer);
      state.gate = newGate(d.id);
      if (!d.sample) fetchJob(state.gate);
    }
    const gate = state.gate;
    const label = el('span', { class: 'label' }, 'GATES AND DECISIONS');
    if (d.sample || (state.lab && state.lab.readOnly)) {
      fill(card, label, el('p', { class: 'unavailable' }, 'Unavailable'), el('p', { class: 'scope-note' }, 'Sample data has no gates. Nothing here writes.'));
      return;
    }
    const running = Boolean(gate.job && gate.job.state === 'running');
    const parts = [label];
    const g = d.gate || { state: 'none' };
    const digest = typeof g.digest === 'string' && /^[0-9a-f]{64}$/.test(g.digest) && g.digest === latestDigest(d) ? g.digest : '';
    if (g.state === 'scope' && digest) {
      parts.push(el('div', { class: 'gate' },
        el('strong', null, 'Approve exact scope'),
        el('code', { title: digest }, 'plan ' + digest),
        el('p', { class: 'scope-note' }, GATE_COPY.scope),
        el('button', { type: 'button', class: 'text-button', dataset: { gate: 'scope' }, disabled: state.gate.pending || running, onclick: () => approveScope(digest) }, gate.pending ? 'Submitting…' : 'Approve this plan')));
    }
    if (g.state === 'published') {
      parts.push(el('div', { class: 'gate' },
        el('strong', null, 'Review gate · resolved by draft publication'),
        el('p', { class: 'scope-note' }, 'Receipt ' + notRecorded(g.receipt) + (g.lifecycle ? ' · lifecycle ' + g.lifecycle : '') + '. Merge on GitHub; merge and production are not verified by Forgecell.'),
        externalLink(g.url, PULL_URL, 'Pull request ↗') || el('span', { class: 'muted' }, 'PR link unavailable')));
    }
    if (g.state === 'delivery') {
      const preview = gate.preview;
      parts.push(el('div', { class: 'gate' },
        el('strong', null, 'Publish a draft PR'),
        el('p', { class: 'scope-note' }, GATE_COPY.delivery),
        el('button', { type: 'button', class: 'text-button', dataset: { gate: 'preview' }, disabled: state.gate.pending || running, onclick: previewDelivery }, preview ? 'Preview again' : 'Preview delivery'),
        preview ? [
          el('code', { title: notRecorded(preview.digest) }, 'preview ' + notRecorded(preview.digest)),
          el('p', { class: 'scope-note' }, 'Base ' + notRecorded(preview.baseBranch) + ' · branch ' + notRecorded(preview.branch) + ' · ' + (preview.files || []).length + ' files: ' + (preview.files || []).join(', ')),
          el('button', { type: 'button', class: 'text-button', dataset: { gate: 'diff' }, onclick: () => show('Delivery diff · ' + d.id, typeof preview.diff === 'string' ? preview.diff : '', (preview.diffTruncated ? 'Truncated · showing the first part of ' + preview.diffBytes + ' bytes.' : 'Complete · ' + preview.diffBytes + ' bytes.') + ' Secrets and local paths are redacted; the CLI shows the full diff.') }, 'View diff ↗'),
          el('label', null, 'Title ', el('input', { type: 'text', maxlength: '256', value: gate.title, dataset: { gate: 'title' }, oninput: (e) => { gate.title = e.target.value; } })),
          preview.acceptanceUnverified ? el('label', null, el('input', { type: 'checkbox', checked: gate.accept, dataset: { gate: 'accept' }, onchange: (e) => { gate.accept = e.target.checked; } }), ' Independent acceptance is unverified; I reviewed acceptance myself') : null,
          el('button', { type: 'button', class: 'text-button', dataset: { gate: 'publish' }, disabled: state.gate.pending || running, onclick: publishDelivery }, gate.pending ? 'Submitting…' : 'Publish draft PR'),
        ] : null));
    }
    const live = d.status === 'running' || d.status === 'allocating';
    if ((live || gate.stop) && hasAction('stop Molecule')) {
      let note = 'Stops the engine call this Lab UI runs for this Molecule; its process group is killed and the ledger records the stop. Work run by a loop or the CLI is refused.';
      if (gate.stop) note = live ? 'Stop requested. Not committed until the ledger no longer shows this Molecule running.' : 'Stopped: the ledger shows status ' + notRecorded(d.status) + (d.phase ? ' · phase ' + d.phase : '') + '. Inspect or re-run it with the forgecell CLI.';
      parts.push(el('div', { class: 'gate' },
        el('strong', null, 'Stop this Molecule'),
        el('p', { class: 'scope-note', role: gate.stop ? 'status' : null }, note),
        live ? el('button', { type: 'button', class: 'text-button', dataset: { gate: 'stop' }, disabled: gate.pending || Boolean(gate.stop), onclick: stopMolecule }, gate.pending ? 'Submitting…' : 'Stop Molecule') : null));
    }
    if (d.lane === RETIRED || gate.retire) {
      parts.push(el('div', { class: 'gate' }, el('strong', null, 'Retired'), retireText(d, gate)));
    } else if (!live && hasAction('retire Molecule')) {
      parts.push(el('div', { class: 'gate' },
        el('strong', null, 'Retire this Molecule'),
        el('p', { class: 'scope-note' }, 'Marks a stale Molecule retired: it leaves All and Needs my decision, and every record is kept. The loop never resumes it and may start a fresh Molecule for its Issue. The engine refuses a running, locked or delivering Molecule.'),
        el('label', null, 'Reason ', el('input', { type: 'text', maxlength: '200', value: gate.reason, dataset: { gate: 'reason' }, oninput: (e) => { gate.reason = e.target.value; } })),
        el('button', { type: 'button', class: 'text-button', dataset: { gate: 'retire' }, disabled: gate.pending || running, onclick: retireMolecule }, gate.pending ? 'Submitting…' : 'Retire Molecule')));
    }
    if (parts.length === 1) parts.push(el('p', { class: 'scope-note' }, 'No human gate is open for this Molecule. This page updates from the ledger.'));
    if (gate.message) parts.push(el('p', { class: 'scope-note', role: 'alert' }, gate.message));
    parts.push(jobText(gate.job));
    fill(card, parts);
  }

  // Atom statuses the engine writes (done, failed, active, waiting, skipped,
  // blocked, pending) and the sample fixture's labels, mapped to a tab mark and
  // class; anything else shows its step number.
  const ATOM_MARKS = {
    done: ['✓', 'is-done'], completed: ['✓', 'is-done'], complete: ['✓', 'is-done'], passed: ['✓', 'is-done'], succeeded: ['✓', 'is-done'],
    approved: ['✓', 'is-done'], recorded: ['✓', 'is-done'], 'draft published': ['✓', 'is-done'],
    failed: ['✕', 'is-failed'], blocked: ['!', 'is-failed'], skipped: ['–', 'is-skipped'],
    waiting: ['◇', 'is-waiting'], 'needs you': ['◇', 'is-waiting'],
    active: [null, 'is-running'], running: [null, 'is-running'],
  };
  function atomMark(status, i) {
    const [mark, cls] = ATOM_MARKS[String(status || '').toLowerCase()] || [null, ''];
    return { mark: mark || String(i + 1).padStart(2, '0'), cls };
  }
  function defaultAtom(d) {
    const atoms = d.atoms || [];
    const current = atoms.findIndex((a) => a.id === d.currentAtom);
    if (current >= 0) return current;
    const failedAt = atoms.findIndex((a) => /fail|block/i.test(a.status || ''));
    return failedAt >= 0 ? failedAt : 0;
  }

  function renderAtoms(d) {
    const atoms = d.atoms || [];
    const box = $('#atoms');
    if (!atoms.length) {
      fill(box, el('p', { class: 'scope-note' }, 'No Atoms are recorded for this Molecule.'));
      return;
    }
    if (state.atomIndex === null || state.atomIndex >= atoms.length) state.atomIndex = defaultAtom(d);
    const flow = el('div', { class: 'atom-flow', role: 'tablist', 'aria-label': 'Molecule Atoms' }, atoms.map((a, i) => {
      const { mark, cls } = atomMark(a.status, i);
      return el('button', {
        type: 'button', role: 'tab', id: 'atom-tab-' + i, class: 'atom-tab' + (cls ? ' ' + cls : ''),
        'aria-selected': String(i === state.atomIndex), 'aria-controls': 'atom-detail', tabindex: i === state.atomIndex ? '0' : '-1', dataset: { atom: String(i) },
      }, el('span', { class: 'atom-number', 'aria-hidden': 'true' }, mark), el('strong', null, a.id), el('small', null, notRecorded(a.status)));
    }));
    flow.addEventListener('click', (e) => { const b = e.target.closest('[data-atom]'); if (b) chooseAtom(Number(b.dataset.atom)); });
    flow.addEventListener('keydown', (e) => {
      const b = e.target.closest('[data-atom]');
      if (!b) return;
      const i = Number(b.dataset.atom), n = atoms.length;
      const next = { ArrowRight: (i + 1) % n, ArrowLeft: (i + n - 1) % n, Home: 0, End: n - 1 }[e.key];
      if (next !== undefined) { e.preventDefault(); chooseAtom(next); }
    });
    fill(box, flow, atomDetail(d, atoms[state.atomIndex], state.atomIndex));
  }

  function chooseAtom(i) {
    state.atomIndex = i;
    renderAtoms(state.detail);
    const tab = $('#atom-tab-' + i);
    if (tab) tab.focus({ preventScroll: true });
  }

  function tags(values, cls) { return (values || []).filter(Boolean).map((v) => el('span', { class: 'access-tag' + (cls ? ' ' + cls : '') }, v)); }

  function atomDetail(d, a, i) {
    const p = a.provenance || {};
    const actions = p.actions || [];
    const byId = attemptsById(d);
    const attempt = actions.map((x) => byId.get(x.attemptId)).filter(Boolean).pop();
    const binding = actions.map((x) => x.executedBinding).filter(Boolean).pop() || p.declaredBinding;
    const ms = a.elapsedMs || elapsed(a.startedAt, a.finishedAt);
    const observed = actions.filter((x) => x.observed);
    return el('div', { id: 'atom-detail', class: 'atom-detail', role: 'tabpanel', 'aria-labelledby': 'atom-tab-' + i, tabindex: '0' },
      el('div', { class: 'atom-detail-head' }, el('strong', null, a.id), el('span', null, notRecorded(a.status)), el('span', { class: 'source' }, notRecorded(a.type))),
      el('dl', { class: 'atom-meta' },
        el('div', null, el('dt', null, 'Harness / binding'), el('dd', null, notRecorded(binding))),
        el('div', null, el('dt', null, 'Model'), el('dd', null, attempt && attempt.model ? attempt.model : 'Not recorded')),
        el('div', null, el('dt', null, 'Duration'), el('dd', null, ms ? durationLabel(ms) : 'Not recorded')),
        el('div', null, el('dt', null, 'Cost'), el('dd', null, costText(attempt && attempt.usage)))),
      // Planned capabilities: what the Formula and plan declared. Nothing here
      // says the Atom used them.
      el('div', { class: 'atom-access' },
        el('span', { class: 'label' }, 'PLANNED CAPABILITIES · DECLARED, NOT OBSERVED'),
        el('div', null, tags([p.declaredSource && 'source ' + p.declaredSource, p.declaredBinding && 'binding ' + p.declaredBinding]), tags(p.declaredWorkflows, 'source-tag'), (!p.declaredSource && !p.declaredBinding && !(p.declaredWorkflows || []).length) ? el('span', { class: 'muted' }, 'None declared') : null)),
      el('dl', { class: 'atom-behavior' },
        el('dt', null, 'Purpose'), el('dd', null, notRecorded(p.declaredPurpose)),
        el('dt', null, 'Planned action'), el('dd', null, notRecorded(p.plannedAction)),
        el('dt', null, 'Detail'), el('dd', null, notRecorded(a.detail)),
        el('dt', null, 'Execution'), el('dd', null, notRecorded(p.execution), p.skipReason ? el('small', null, 'Skip reason: ' + p.skipReason) : null),
        typeof a.exitCode === 'number' ? [el('dt', null, 'Exit code'), el('dd', null, String(a.exitCode))] : null),
      // Actions taken: only what an observation recorded, with its evidence.
      el('div', { class: 'atom-actions' },
        el('span', { class: 'label' }, 'ACTIONS TAKEN · OBSERVED'),
        observed.length ? observed.map((x) => el('div', { class: 'atom-action' },
          el('p', null, el('strong', null, notRecorded(x.observed.action) + ' → ' + notRecorded(x.observed.result)), el('small', null, 'Attempt ' + notRecorded(x.attemptId) + ' · ' + notRecorded(x.startedAt) + ' → ' + notRecorded(x.observed.finishedAt))),
          x.observed.externalAction ? el('p', { class: 'scope-note' }, 'External: ' + x.observed.externalAction + ' → ' + notRecorded(x.observed.externalResult)) : null,
          (x.observed.evidence || []).length
            ? el('ul', { class: 'evidence-list' }, x.observed.evidence.map((e) => evidenceItem(d, e)))
            : el('p', { class: 'scope-note' }, 'No evidence references recorded.')))
          : el('p', { class: 'scope-note' }, actions.length ? 'Actions started; no observation recorded yet.' : 'No observed actions recorded.')),
      // Context sources: what each started action resolved and ran under, as
      // recorded, kept apart from the declarations above.
      el('div', { class: 'atom-actions' },
        el('span', { class: 'label' }, 'CONTEXT SOURCES · AS RECORDED PER ACTION'),
        actions.length
          ? el('ul', { class: 'record-list' }, actions.map((x) => el('li', null,
            el('strong', null, 'Attempt ' + notRecorded(x.attemptId)),
            el('span', null, 'resolved source ' + notRecorded(x.resolvedSource)),
            el('span', null, 'binding ' + notRecorded(x.executedBinding)),
            el('span', null, 'planned ' + notRecorded(x.plannedAction)),
            el('code', { title: x.planDigest || '' }, 'plan ' + short(x.planDigest)))))
          : el('p', { class: 'scope-note' }, 'No actions started; no context source recorded.')),
      el('div', { class: 'atom-buttons' },
        el('button', { type: 'button', class: 'text-button', dataset: { origin: 'atom-record' }, onclick: () => show('Atom record · ' + a.id + ' · ' + d.id, JSON.stringify(a, null, 2)) }, 'Atom record ↗'),
        el('button', { type: 'button', class: 'text-button', dataset: { origin: 'atom-ledger' }, onclick: () => showLedger(d.id) }, 'Ledger record ↗')));
  }

  // One evidence reference. A file the ledger lists with a SHA-256 opens as
  // bounded, redacted text; the server checks the digest and the file again.
  function evidenceItem(d, e) {
    return el('li', null,
      el('code', null, e.path || e.pointer || 'reference'),
      e.sha256 ? el('small', { title: e.sha256 }, 'sha256 ' + short(e.sha256)) : null,
      e.note ? el('small', null, e.note) : null,
      e.viewable && e.path
        ? el('button', { type: 'button', class: 'text-button', dataset: { evidence: e.path }, onclick: () => showText('Evidence · ' + e.path + ' · ' + d.id, '/api/molecules/' + encodeURIComponent(d.id) + '/evidence?path=' + encodeURIComponent(e.path)) }, 'View ↗')
        : null);
  }

  function showLedger(id) { showText('Ledger record · ' + id, '/api/molecules/' + encodeURIComponent(id) + '/ledger'); }

  // showText fetches a bounded file view and shows it as text, with its size and
  // whether it was cut; a refusal shows the server's category message.
  async function showText(title, path) {
    try {
      const res = await api(path);
      const body = await res.json().catch(() => ({}));
      if (!res.ok) { show(title, 'Not served: ' + notRecorded(body && body.error), ''); return; }
      const size = typeof body.size === 'number' ? body.size : null;
      const note = body.truncated
        ? 'Truncated · showing the first part of ' + (size === null ? 'an unknown number of' : size) + ' bytes. Secrets and local paths are redacted.'
        : (size === null ? 'Size not recorded' : size + ' bytes') + ' · complete. Secrets and local paths are redacted.';
      show(title + (body.sha256 ? ' · sha256 ' + short(body.sha256) : ''), typeof body.text === 'string' ? body.text : '', note);
    } catch (error) { failed(error); }
  }

  function renderReadiness(d) {
    const r = d.readiness;
    const box = $('#readiness');
    if (!r) { fill(box, el('p', { class: 'scope-note' }, 'No readiness state recorded.')); return; }
    fill(box,
      el('p', { class: 'scope-note' }, 'Phase ' + notRecorded(r.phase)),
      el('ul', { class: 'record-list' }, (r.plans || []).map((p) => el('li', null, el('strong', null, 'Revision ' + p.revision), el('code', { title: p.digest }, 'digest ' + short(p.digest)), el('code', { title: p.parentDigest || '' }, p.parentDigest ? 'parent ' + short(p.parentDigest) : 'no parent')))),
      el('ul', { class: 'record-list events' }, (r.events || []).map((e) => el('li', null, el('span', { class: 'mono-text' }, notRecorded(e.at)), el('strong', null, notRecorded(e.kind)), e.detail ? el('span', null, e.detail) : null))));
  }

  function renderAttempts(d) {
    const attempts = d.attempts || [];
    fill($('#attempts'), attempts.length
      ? el('ul', { class: 'record-list' }, attempts.map((a) => {
        const u = a.usage, t = (u && u.total) || {};
        const tokens = u ? 'tokens in ' + notRecorded(t.input) + ' · out ' + notRecorded(t.output) : 'usage unavailable';
        return el('li', null, el('strong', null, a.kind + ' ' + a.id), el('span', null, notRecorded(a.status)), el('span', null, 'model ' + notRecorded(a.model || (u && (u.effectiveModels || []).join(', ')))), el('span', null, tokens), el('span', null, 'cost ' + costText(u)));
      }))
      : el('p', { class: 'scope-note' }, 'No harness attempts recorded.'));
  }

  $('#yaml').addEventListener('click', async () => {
    const id = state.selectedId;
    if (!id) return;
    try {
      const res = await api('/api/molecules/' + encodeURIComponent(id) + '?yaml=1');
      if (!res.ok) { show('Formula snapshot', 'The snapshot could not be read.'); return; }
      const d = await res.json();
      show('Formula snapshot · ' + notRecorded(d.formulaId) + ' · sha256 ' + short(d.formula.sha256), d.formula.yaml || 'No YAML recorded.');
    } catch (error) { failed(error); }
  });

  // ---- Overview ----------------------------------------------------------------
  function renderOverview() {
    const all = molecules().filter(live);
    const counts = (state.list && state.list.counts) || {};
    const sample = Boolean(state.list && state.list.sample);
    $('#aggregate .panel-head .mono').textContent = sample ? 'LAB OVERVIEW · SAMPLE DATA' : 'LAB OVERVIEW';
    $('#aggregate-count').textContent = 'All ' + all.length + ' Molecules';
    const waiting = all.filter((m) => m.lane === 'decision');
    fill($('#review-queue'), (waiting.length
      ? waiting.slice(0, 5).map((m) => el('button', { type: 'button', class: 'review-row', dataset: { review: m.id }, onclick: () => select(m.id) },
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

  // ---- App shell: views, needs-you badge, loop pill, operator, resident --------
  // The view lives in the URL (?view=…), set through history.pushState by real
  // <a href> links, so Back, reload and copied links restore it.
  const VIEWS = ['inbox', 'issues', 'work', 'insights', 'formula'];
  function viewFromURL() {
    const view = new URLSearchParams(location.search).get('view');
    return VIEWS.includes(view) ? view : 'inbox';
  }
  function showView(view) {
    state.view = view;
    document.querySelectorAll('[data-view-panel]').forEach((panel) => { panel.hidden = panel.dataset.viewPanel !== view; });
    document.querySelectorAll('a[data-view]').forEach((link) => {
      if (link.dataset.view === view && !link.closest('main')) link.setAttribute('aria-current', 'page');
      else link.removeAttribute('aria-current');
    });
    const more = $('#tab-more');
    more.classList.toggle('is-current', view === 'insights' || view === 'formula');
    more.open = false;
    hideTip();
  }
  function navigate(view, focus) {
    if (view !== viewFromURL()) history.pushState({ view: view }, '', '?view=' + view);
    showView(view);
    if (focus) $('#main').focus({ preventScroll: true });
  }
  document.addEventListener('click', (e) => {
    const link = e.target.closest('a[data-view]');
    if (!link || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(link.dataset.view, true);
  });
  window.addEventListener('popstate', () => { showView(viewFromURL()); restoreSelection(); });

  // A Molecule link (?view=work&m=…) is a real link: a plain click opens the
  // Molecule under Work through history.pushState, so Back returns here.
  const MOLECULE_ID = /^[A-Za-z0-9_-]{1,200}$/;
  function moleculeHref(id) { return '?view=work&m=' + encodeURIComponent(id); }
  function moleculeFromURL() {
    const id = new URLSearchParams(location.search).get('m');
    return id && MOLECULE_ID.test(id) ? id : null;
  }
  function restoreSelection() {
    const id = moleculeFromURL();
    if (state.view === 'work' && id && id !== state.selectedId) select(id);
  }
  function openMolecule(id) {
    if (location.search !== moleculeHref(id)) history.pushState({ view: 'work', m: id }, '', moleculeHref(id));
    showView('work');
    if (id !== state.selectedId || $('#selected').hidden) select(id);
    $('#main').focus({ preventScroll: true });
  }
  document.addEventListener('click', (e) => {
    const link = e.target.closest('a[data-molecule]');
    if (!link || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if (!MOLECULE_ID.test(link.dataset.molecule)) return;
    e.preventDefault();
    openMolecule(link.dataset.molecule);
  });

  // Counts read as words through Intl.PluralRules.
  const plural = new Intl.PluralRules('en-US');
  function needsYouText(n) { return n + (plural.select(n) === 'one' ? ' thing needs you' : ' things need you'); }
  function runningText(n) { return n + (plural.select(n) === 'one' ? ' Molecule running' : ' Molecules running'); }
  // setText changes a node's text only when it differs, so a poll never
  // re-announces or disturbs what is already shown.
  function setText(node, text) { if (node.textContent !== text) node.textContent = text; }

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
  $('#loop-stop-confirm').addEventListener('click', stopLoop);
  $('#loop-start-link').addEventListener('click', () => {
    // The form is in the side panel, beside every view; focus its chosen scope.
    const input = document.querySelector('#drive-loop [aria-pressed="true"]');
    if (input) setTimeout(() => input.focus({ preventScroll: true }), 0);
  });

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

  // ---- Inbox --------------------------------------------------------------------
  // The home view: what needs you first, then work in progress, then the last
  // drafts recorded. Every entry is the server's InboxView, in plain words, so
  // no phase code, id, digest or timestamp is turned into text here. Polling
  // patches entries in place, keyed by Molecule id: an entry already shown keeps
  // its node and has only its changed text and attributes updated.
  const STEPS = ['Plan', 'Your approval', 'Coding', 'Checks', 'Review', 'Draft PR', 'Merged'];
  const NEEDS_KINDS = ['scope', 'publish', 'blocked'];
  const CARD_KINDS = { scope: ['is-needs', '◇'], publish: ['is-ready', '✓'], blocked: ['is-blocked', '!'] };
  // Bounds: at most this many cards or rows each, and the last five recorded.
  const MAX_ENTRIES = 20, MAX_RECENT = 5;
  const relativeTime = new Intl.RelativeTimeFormat('en-US', { numeric: 'auto' });
  const fullDate = new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'short' });

  function inboxOf(m) {
    const v = m && m.inbox;
    return v && typeof v === 'object' && typeof v.kind === 'string' && v.kind && MOLECULE_ID.test(m.id) ? v : null;
  }
  function plainText(value, fallback) { return typeof value === 'string' && value ? value : fallback; }
  // count is a served count, or 0 when it was omitted as zero; callers show it
  // only when its recorded flag (planned, verified, captured) is set.
  function count(n) { return Number.isInteger(n) && n >= 0 ? n : 0; }
  function counted(n, one, many) { return n + ' ' + (plural.select(n) === 'one' ? one : many); }
  function timeOf(m) { const t = Date.parse(m.finishedAt || m.startedAt); return Number.isFinite(t) ? t : 0; }
  function newestFirst(a, b) { return timeOf(b) - timeOf(a) || (a.id < b.id ? -1 : 1); }
  // needsYou is the one definition of what needs you: the Molecules shown under
  // Needs you (scope gate, ready to publish, blocked), newest first. The headline,
  // the live region, the nav badge and the resident all count it.
  function needsYou() {
    return molecules().filter((m) => { const v = live(m) && inboxOf(m); return Boolean(v) && NEEDS_KINDS.includes(v.kind); }).sort(newestFirst);
  }
  function numberText(m) { return Number.isInteger(m.number) ? '#' + m.number : ''; }
  function titleText(m) { return (state.list && state.list.sample ? '[Sample] ' : '') + plainText(m.title, 'Untitled'); }

  // ago is a recorded time as relative words ("12 minutes ago"); dateTitle is
  // the same time as a full local date, for a tooltip.
  function ago(iso) {
    const t = Date.parse(iso || '');
    if (!Number.isFinite(t)) return '';
    const s = Math.round((t - Date.now()) / 1000), abs = Math.abs(s);
    if (abs < 60) return 'just now';
    if (abs < 3600) return relativeTime.format(Math.round(s / 60), 'minute');
    if (abs < 86400) return relativeTime.format(Math.round(s / 3600), 'hour');
    return relativeTime.format(Math.round(s / 86400), 'day');
  }
  function dateTitle(iso) { const t = Date.parse(iso || ''); return Number.isFinite(t) ? fullDate.format(t) : ''; }
  function minutesText(ms) {
    const min = Math.floor(ms / 60000);
    if (min < 1) return 'under a minute';
    if (min < 60) return min + ' min';
    return Math.floor(min / 60) + ' h' + (min % 60 ? ' ' + (min % 60) + ' min' : '');
  }
  function stepIndex(v) { return Number.isInteger(v.step) && v.step >= 0 && v.step < STEPS.length ? v.step : 0; }
  // stepText is the current step with its elapsed time: "Coding · 18 min".
  function stepText(v) {
    if (v.queued) return 'Queued for checks';
    const label = plainText(v.stepLabel, STEPS[stepIndex(v)]);
    const t = Date.parse(v.stepSince || '');
    return Number.isFinite(t) ? label + ' · ' + minutesText(Math.max(0, Date.now() - t)) : label;
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
  function setAttr(node, name, value) { if (node.getAttribute(name) !== value) node.setAttribute(name, value); }
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
    const [cls, icon] = CARD_KINDS[v.kind];
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
    // One primary action per card. Until the plan view (slice 3) exists, every
    // action opens the Molecule's detail under Work.
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
    const when = ago(m.finishedAt || m.startedAt);
    setText(slot(node, 'meta'), plainText(v.title, 'Draft published') + (when ? ' · ' + when : ''));
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
    const entries = molecules().filter((m) => live(m) && inboxOf(m));
    const kind = (k) => entries.filter((m) => inboxOf(m).kind === k);
    const needs = needsYou();
    const running = kind('progress').sort(newestFirst);
    const recent = kind('recorded').sort(newestFirst).slice(0, MAX_RECENT);
    patchList($('#inbox-needs'), needs.slice(0, MAX_ENTRIES), createCard, updateCard);
    patchList($('#inbox-progress'), running.slice(0, MAX_ENTRIES), createRow, updateRow);
    patchList($('#inbox-recent'), recent, createRecent, updateRecent);
    $('#inbox-needs-empty').hidden = needs.length > 0;
    $('#inbox-progress-empty').hidden = running.length > 0;
    $('#inbox-recent-empty').hidden = recent.length > 0;
    moreText($('#inbox-needs-more'), needs.length, 'Work');
    moreText($('#inbox-progress-more'), running.length, 'Work, Running');

    const name = firstName();
    setText($('#inbox-title'), 'Good ' + partOfDay(new Date().getHours()) + (name ? ', ' + name : ''));
    const waiting = needs.length, inFlight = counts.running || 0, blocked = counts.blocked || 0, drafts = counts.finished || 0;
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

  showView(viewFromURL());
  restoreSelection();

  // ---- Dialog, motion and visibility -------------------------------------------
  let dialogReturn = null;
  function show(title, body, note) {
    dialogReturn = document.activeElement;
    $('#dialog-title').textContent = title;
    const noteBox = $('#dialog-note');
    noteBox.textContent = note || '';
    noteBox.hidden = !note;
    $('#dialog-body').textContent = body;
    $('#detail').showModal();
  }
  $('#close-dialog').addEventListener('click', () => $('#detail').close());
  $('#detail').addEventListener('close', () => { if (dialogReturn && dialogReturn.focus) dialogReturn.focus({ preventScroll: true }); });

  $('#motion-toggle').addEventListener('change', (e) => document.body.classList.toggle('motion-paused', !e.target.checked));
  document.addEventListener('visibilitychange', () => {
    document.body.classList.toggle('page-hidden', document.hidden);
    if (!document.hidden) poll();
  });
  if ('IntersectionObserver' in window) {
    new IntersectionObserver(([entry]) => document.body.classList.toggle('shelf-offscreen', !entry.isIntersecting)).observe($('.shelf'));
    // The resident's working pose pauses while it is offscreen.
    new IntersectionObserver(([entry]) => document.body.classList.toggle('resident-offscreen', !entry.isIntersecting)).observe($('#resident'));
  }

  loadLab().then(poll).catch(failed);
  setInterval(poll, 5000);
  // An unchanged list answers 304, so the Inbox's elapsed and relative times are
  // patched on their own interval.
  setInterval(() => { if (!document.hidden && state.list) renderInbox(); }, 30000);
  // The Lab header also changes without a ledger change, as when a Formula is
  // activated, so it is refreshed on its own slower interval.
  setInterval(() => { if (!document.hidden && !state.sessionLost) loadLab().catch(failed); }, 30000);
})();
