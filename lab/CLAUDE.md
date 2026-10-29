# Rules for changing the Lab

Follow these rules for every change under `lab/`. They prevent the failures that
the protected view and review catch most often.

## The protected view runs the base tests against your code

- Never change the signature of an existing Go function, method or type that a
  base `_test.go` file calls. Add a new one instead. A compile failure in the
  protected view can't be declared as an expected change.
- Never change a default so that a base test hangs or panics. When unconfigured,
  keep the old behaviour and make the new one opt-in (example:
  `verificationSlots` is 1 when unset; `init` writes 2).
- A by-design change to a base test's expectation is fine: declare it (UI:
  Accept by-design failures; CLI: `amend --accept-protected-failures`).

## Lab UI modules (`lab/internal/ui/web`)

- Write one module per view under `js/`, plus that view's own sub-modules (for
  example `molecule-plan.js`). A view imports only `core*.js` and its own
  sub-modules, never another view.
- Keep every module at or under 600 lines. Split a sub-module out before you
  reach the limit.
- Add a new served file in two places: the explicit `staticFiles` allowlist in
  `server.go` (no wildcards), and `servedModules` or `servedStylesheets` in
  `served_test.go`.
- The `ui` Go package is shared by every slice. Prefix new package-level names
  with their feature (for example `formulaProposalView`, not `proposalView`) so
  parallel slices don't collide.
- Render text only. Never use `innerHTML`, `insertAdjacentHTML` or `outerHTML`.
- Bind every label to its input with `for`/`id` from `uid()`.
- Use neutral placeholders that fit any repository, never Forgecell's own paths.

## Tests

- Keep tests deterministic: no sleeps or wall-clock budgets. Use readiness
  markers, injected clocks and seams.
- After an accepted async start (`202` from `/api/runs` or `/api/loop/start`),
  wait with `waitJob` before asserting engine calls. macOS CI is slower and
  exposes these races.
- Cover the failure path as well as the happy path.

## Docs

- Describe only shipped behaviour.
- The documented install one-liner is pinned in three places: `README.md`,
  `docs/install.md` and `scripts/test-native-installer.py` (`public_url`). Move
  all three together, and only after the release has been verified anonymously.
