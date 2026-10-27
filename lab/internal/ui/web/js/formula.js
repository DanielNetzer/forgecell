// ---- Formula: the active-Formula card -------------------------------------------
// The Formula new Molecules use, and its approval state. Existing Molecules keep
// their saved snapshot, shown in each Molecule's Timeline.
import { $, el, fill, notRecorded, short } from './core.js';

function renderFormula(lab) {
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
}

// The Formula view mounts on its data-view-panel; its markup stays in index.html.
export function mount(root, ctx) {}

// update renders the active-Formula card from the Lab.
export function update(state) { renderFormula(state.lab); }
