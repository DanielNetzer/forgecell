# Atom types

An Atom is one workflow step. The table distinguishes the current executor from the
longer-term ticket-to-production flow.

| Type | Current behavior | Planned behavior / limitation |
|---|---|---|
| `intake` | Reads a GitHub Issue with `gh`; records provenance | No automatic clarification conversation |
| `harness` | Executes explicit `harness.command` with issue and recipe guidance | Requires approved Formula and exact ticket scope; exit zero is not proof of shipping |
| `check` | Runs approved local verification of the captured source tree in a fresh checkout | Required failure stops the run; exact-head CI is a separate gate |
| `gate` | Waits for exact scope approval before coding and human review after successful verification | Formula approval does not approve ticket execution or publication |
| `ship` | Skipped | Explicit draft-PR delivery is separate; merge and deploy are not implemented |
| `document` | Skipped | Issue write-back is not implemented |
| `learn` | Explicit `forgecell learn` proposes a Formula change for human review | Skipped during a normal run; evaluation of follow-up ledgers is manual |
| `observe` | No day-one binding | Future post-production observation; no Datadog integration |

The check Atom executes reviewed argv, working directories and timeouts after validating
command definition hashes. It uses a temporary HOME and filtered environment, with
unrestricted network access and no OS filesystem/network sandbox. Source mutations
or writes outside approved artifact allowances fail verification.

Required-check failure blocks the run and leaves subsequent Atoms skipped. Successful
verification leaves the review gate waiting; later publication Atoms remain skipped.
When test paths or cited regression definitions change, regression checks also run
against a protected view that restores those frozen baseline files, so candidate edits
cannot replace the preserved suite. Passing
candidate checks does not establish independent acceptance; that requires separately
supplied checks with independent provenance. See [ticket readiness](ticket-readiness.md).

Named GitHub checks for the exact PR head (`forgecell checks`) and explicitly approved
draft-PR delivery (`forgecell deliver`) are separate gates. Local check success does not
prove CI, merge, deployment or acceptance. These ticket-readiness and delivery commands
are development-build features until a new release is published; see [Install](install.md).

The coding harness implements the ticket. The learn Atom invokes the distinct meta harness,
which proposes improvements to the process. See [Learning](learning.md).
