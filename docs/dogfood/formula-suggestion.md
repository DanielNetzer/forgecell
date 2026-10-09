# First evidence-backed Formula suggestion

Status: **approved and applied** by the user on 2026-09-28 at 05:45:48 UTC.

Process change: when sandbox IPC blocks the ordinary test runner, use a verified fallback
and report both outcomes separately. The Unicode code fix is a separate ticket result.

Suggestion: `suggestion-326786a2-0b87-4bb2-8dd4-32670d0986b3`

Molecule: `mol-6-20260927T215456Z-c1cf6b12` (GitHub issue #6).

The Molecule records npm test blocked by sandbox IPC EPERM, while direct Node test execution passed all 47 tests. Add one narrowly scoped fallback instruction, preserving command bindings, timeouts, and the human gate. Require separate reporting of the blocked command and fallback result.

```diff
diff --git abefore.yaml bafter.yaml
index 4676797..fc588f4 100644
--- abefore.yaml
+++ bafter.yaml
@@ -17,6 +17,12 @@ harness:
     - /opt/homebrew/Cellar/node/25.9.0_2/bin/node
     - lab/examples/codex-adapter.mjs
   timeoutMs: 900000
+  instructions: >-
+    When running CLI tests from lab/, if npm test is blocked specifically by
+    sandbox IPC EPERM, try env -u NO_COLOR FORCE_COLOR=3 node --import tsx --test
+    src/*.test.ts src/*.test.tsx within the existing execution restrictions.
+    Report npm test as blocked and report the fallback's actual result separately;
+    do not treat the fallback as proof of CI or claim skipped checks succeeded.
 atoms:
   - id: intake
     type: intake
```

Only `harness.instructions` changes. Command bindings, timeouts, Atoms and the human gate remain identical. The CLI now sends this optional field as `recipeInstructions` to the coding harness, separate from execution restrictions. Two regression tests verified this contract (failed before implementation, passed afterward); the full CLI suite now has 51 passing tests.

The first learning attempt returned unchanged YAML. After exposing the supported recipe-guidance field, a later attempt timed out without producing a proposal; a subsequent completed invocation produced this diff. No mutation was applied automatically.

## Follow-up observation

Molecule `mol-6-20260928T054626Z-4fc54e6b` ran issue #6 with the approved Formula snapshot
`37b30b25aa85cb193dbb1c67a5109bd2a4b29c7365ea6837753ca7fb0239eaef`.
Its stored YAML hash was verified against this value.

The harness found the original Unicode fix already present and extended the regression to
both successful and nonzero command exits. It reported the original decoder failing both
cases, then the fixed decoder passing. `npm test` was again blocked by sandbox IPC EPERM;
the approved fallback passed 52 tests, with zero failures or skips. Typecheck and build passed.
The parent Molecule finished at the waiting human gate. Ship and document stayed skipped.

**Finding:** the approved guidance was used, and blocked versus completed validation was
reported separately. **Limitation:** this repeated the same ticket in the same sandbox; it
does not establish broader reliability, lower cost or faster delivery. A future comparable
ticket should confirm correct trigger detection, unchanged test coverage, and separate
reporting. Ordinary assertion failures must not be disguised as sandbox failures.

Approval and before/after YAML remain in the local suggestion JSON; exact execution evidence
remains in the two local Molecule ledgers. No GitHub issue comment was posted. The initial
proposal predates structured evaluation fields; this document records the subsequent human
interpretation without rewriting the original proposal or its approval.


## First paired evaluation

The approved instruction was compared with its exact baseline on a new frozen task. Both variants passed fresh independent verification and both recovered from IPC EPERM with the same fallback. This single pair provides no observed correctness or recovery advantage for the instruction. The original evaluation bundle is preserved in the private historical repository (access required) and omitted from the public snapshot. The report retains timings, token observations, unknown cost and methodological limits. No Formula was changed in response to this finding.
