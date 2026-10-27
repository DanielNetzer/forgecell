// ---- Issues: coming next ---------------------------------------------------------
// The Issues view is still its coming-next placeholder, whose markup stays in
// index.html; until it is built, intake starts from Start intake in the side
// panel. This module owns the view so the slice that builds it edits only here.

// The Issues view mounts on its data-view-panel; the placeholder needs nothing.
export function mount(root, ctx) {}

// update has nothing to patch while the view is a placeholder.
export function update(state) {}
