# Forgecell product bible

Compose once. Improve forever.

Install it. Point it at a ticket. It ships — and remembers how.

Forgecell is a bottom-up dark software factory installed by one individual engineer. Dark describes the operating model. Distribution is the product: one install, no platform team required.

## Locked ontology

- Atom — one workflow step.
- Molecule — one ticket unit of work + its ledger.
- Formula — durable YAML recipe; learning patches this.
- Lab — the runtime you install.

The Lab runs Molecules; Molecules are Atoms arranged by a Formula. No fifth primitive.

## Pillars and sequence

Compose, Execute, Record, Improve, Automate the automations. The BYO coding harness executes work. The meta layer proposes better Formulas from ledgers; it is not the coding harness. Human approval precedes Formula mutation.

GitHub Issues intake; BYO harness and LLM; GitHub + Actions ship; document back on the Issue. No Datadog yet. Linear is only a possible later intake. Lab is a terminal-first Go CLI.

Priorities: wire the harness, then a learn Atom that proposes Formula diffs, then dogfood on Forgecell Issues. Do not imply merge/deploy/comment has shipped when it has not. These actions require explicit approval and do not run automatically.

## Design

Use Forgecell text branding and the four primitive names. Keep the product focused on
one individual engineer; no chat clone, team seats or collaboration mesh.

Companion UI (design direction only; implementation deferred): one screen, Runs & suggestions. Left: Molecules. Right: selected Formula, Atom ledger behavior, sources/actions, and suggestions with Approve / Dismiss. Intake source is shown for each Molecule. Reads, planned actions and actual actions must be distinguished.

## Research discipline

The Lab learns iteratively from its own work. Observations, reasoning, proposed process
changes, human decisions and follow-up results should be documented and traceable.
Formula learning improves the software-making process; it does not implement ticket fixes.
Expected impact is a hypothesis, not a finding. A next run may support, contradict, or leave
it unresolved. Seek better outcomes with explicit tradeoffs rather than claiming an optimum.
See [Learning](learning.md) for the current contract and implementation limits.
