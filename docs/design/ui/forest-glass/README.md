# Forest glass: Lab UI design reference

This folder holds the approved UX reference for the local Forgecell Lab UI, which `forgecell ui` will serve. It is a **design prototype, not product code**.

- `CLAUDE-HANDOFF.md` comes first. It covers scope, UX flows, what is mocked, the approval safety contract, accounting rules and the acceptance checks for integration.
- `DESIGN-PROMPT.md` records the original direction and iteration notes. When it disagrees with the handoff and the current HTML, CSS and JS, those win.
- `index.html`, `style.css` and `app.js` are a vanilla-JS interactive prototype. To preview it, serve this folder with any static server, for example `python3 -m http.server 3112`.
- `assets/` holds the Forgecell Blob sprite, botanical artwork from the marketing site (downscaled) and the fonts.
- `screenshots/` holds review snapshots. Not every one shows the latest state.

All data in the prototype is **synthetic and labeled as sample**. That covers Molecules, Formula versions, patches, checks, durations, costs and evaluation results. None of it is evidence or test fixtures. Decisions in the prototype run only in the browser. They never execute, publish, activate or change anything.
