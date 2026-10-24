// Forgecell Lab UI launch: keeps the exchange page's session value in this tab's
// sessionStorage, which is scoped to this origin and port, then opens the UI.
'use strict';
(() => {
  const meta = document.querySelector('meta[name="forgecell-session"]');
  try {
    if (meta && /^[0-9a-f]{64}$/.test(meta.content)) sessionStorage.setItem('forgecell-session', meta.content);
  } catch (error) {
    // Without storage the UI shows how to start a new session.
  }
  location.replace('/');
})();
