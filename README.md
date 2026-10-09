# Forgecell

**Compose once. Improve forever.**

Forgecell is a local Lab an individual engineer installs to run GitHub tickets with their
own coding harness and model. Every Molecule leaves a ledger; the meta harness uses that
evidence to propose human-reviewed improvements to the Formula that guides future work.

The ambition: *Install it. Point it at a ticket. It ships — and remembers how.*
The development build analyzes the ticket, waits for exact scope approval, then runs the
bound harness and verifies its output in a fresh checkout. It does **not** merge, deploy,
or comment on issues. Unsupported analysis bindings stop before coding.
The Lab is terminal-first; local UI implementation is deferred.

Distribution is the product: no platform team required to start. The Lab is Go. The core source is MIT licensed, and a compiled CLI preview is publicly downloadable. Ticket readiness exists in the development CLI but is not in the published preview. See [native distribution](docs/design/native-cli-distribution.md).

## Primitives

| Primitive | Meaning |
|---|---|
| **Atom** | One workflow step |
| **Molecule** | One ticket unit of work + its ledger |
| **Formula** | Durable YAML recipe; learning patches this |
| **Lab** | The runtime you install |

Spine: the **Lab** runs **Molecules**; each Molecule is **Atoms** arranged by a **Formula**.

## How it works

1. **Compose:** approve a Formula that arranges the Atoms and binds your harness.
2. **Execute:** point the Lab at a GitHub Issue, review its proposed scope and checks, then approve the exact plan before the coding harness starts.
3. **Record:** keep the exact Formula snapshot, Atom outcomes and harness report in the Molecule ledger.
4. **Improve:** ask the learn Atom to reason from ledgers and propose a process change with evidence and an evaluation plan.
5. **Automate the automations:** review the proposed Formula diff, then evaluate the revised recipe on another run.

The learning loop improves **how software is created**. A ticket's code fix belongs to the
coding harness; a reusable workflow lesson belongs in the Formula. Approval permits an
experiment—it does not prove that the change helped.

## What works today

| Capability | Current behavior |
|---|---|
| Intake | GitHub Issues via authenticated `gh` |
| Execution | Read-only intake, human scope gate, then your bound harness and model in isolation |
| Evidence | Go ledgers record the exact Formula, issue, isolated base/workspace and Atom outcomes |
| Checks | Approved local checks run in fresh worktrees; `checks` separately verifies named GitHub jobs against the exact PR head |
| Learning | Go `learn` proposes human-reviewed Formula diffs from finished ledgers |
| Review | Approve or dismiss; stale proposals cannot overwrite a changed Formula |
| Evaluation | First paired trial completed: both variants passed; no observed recovery/correctness advantage. The original evaluation bundle is preserved in the private historical repository (access required) and omitted from the public snapshot. |
| Shipping | Go delivery previews exact file scope and opens a draft PR after explicit approval; no automatic merge, deployment or issue closure |

The target is ticket → production using GitHub and Actions. No Datadog binding or local
companion UI is implemented.

## Where Codex fits

The native Codex adapter launches `codex exec` using your existing CLI configuration.
Ticket analysis receives frozen evidence with action tools disabled; coding uses the
provider’s workspace-write sandbox. Learning is a separate read-only request for a
structured Formula proposal. The adapter instructs coding runs not
to push, merge, deploy or comment. Configure harness permissions deliberately: Forgecell
does not enforce a sandbox around arbitrary adapters.

Codex is replaceable. Forgecell owns the Formula, execution record and human review;
your chosen harness provides coding and reasoning. Durable learning lives in Formulas
and ledgers, not in this conversation or a persistent model session.

## Lab CLI

The Go CLI in [`lab/`](lab/) exposes `doctor`, `init`, `run`, `ledger`, `learn`, `suggestion`, `deliver`, `checks`, `evaluate` and installer rollback. There is no local GUI.

Install the public CLI preview without source access, Node or Go:

```sh
sh -c 'set -eu; script=$(mktemp "${TMPDIR:-/tmp}/forgecell-bootstrap.XXXXXXXX") || { echo "Forgecell: cannot create bootstrap temporary file." >&2; exit 1; }; trap '"'"'rm -f "$script"'"'"' 0; trap '"'"'exit 1'"'"' HUP INT TERM; curl -fsSL --proto "=https" --proto-redir "=https" --connect-timeout 15 --max-time 30 https://github.com/DanielNetzer/forgecell-releases/releases/download/0.2.0-preview.1/install.sh -o "$script" || { echo "Forgecell: bootstrap download failed." >&2; exit 1; }; [ -s "$script" ] || { echo "Forgecell: bootstrap download was empty." >&2; exit 1; }; sh "$script"'
```

The [download repository](https://github.com/DanielNetzer/forgecell-releases) distributes compiled packages independently of the MIT licensed core source. See [Install](docs/install.md) for prerequisites, PATH, upgrades and rollback.

For contributors with Go installed, build the preview from a checkout:

```bash
cd lab
go build -o bin/forgecell ./cmd/forgecell
cd ..
lab/bin/forgecell doctor
lab/bin/forgecell init --json
# Review the saved Formula, then use the returned proposal ID:
lab/bin/forgecell init --approve <proposal-id>
lab/bin/forgecell run 123 --target main
# Review the plan and its commands before approving its exact digest:
lab/bin/forgecell run 123 --approve <plan-digest>
```

See [ticket readiness](docs/ticket-readiness.md) for analysis support, amendments, recovery, and current limitations. The public preview predates this flow.

Go onboarding detects available Codex, Claude Code or Cursor bindings, records repository evidence and explicit unknowns, and saves a proposal before activation. Existing approved Formulas remain unchanged unless a rebind is requested. Runs require approval and use isolated worktrees. `deliver` previews a specific file list; an exact digest authorizes a draft PR, not a merge. `checks` requires an exact commit and explicit check names.

The native installer verifies checksums and preserves Lab data.

Lab checks: `cd lab && go test ./... && go vet ./...`. CI runs these and native installer lifecycle tests on macOS and Linux. See [live Go dogfood evidence](docs/dogfood/issue-9-ledger.md).

## Learning is research

Observe a run → reason from its ledger → propose one process change → human review →
run again → evaluate. A Formula changes how software is made; the coding harness changes
the codebase. Expected benefits remain hypotheses until evaluated. See the
[learning contract](docs/learning.md) and [dogfood findings](docs/dogfood/README.md).

Forgecell is a local Lab, not an IDE chat, productivity coach, team SaaS workspace,
or multi-agent collaboration mesh.

## Docs

- [Product bible](docs/product-bible.md)
- [Learning and evidence](docs/learning.md)
- [Ontology](docs/ontology.md)
- [Atoms](docs/atoms.md)
- [Pillars](docs/pillars.md)
- [Install](docs/install.md)
- [Agent-driven onboarding](docs/agent-onboarding.md)
- [Lab CLI](lab/README.md)
- [Visual brief](docs/visual-brief.md)

## License

The core source is licensed under the [MIT License](LICENSE). Copyright (c) 2026 Forge Labs.

Local Lab records remain private: do not publish ledgers, credentials, environment files
or raw harness traces. Licensing does not establish repository visibility; publication
and historical website exposure require separate review.

## Core release preparation and migration

Historical issue numbers, commit hashes and original URLs refer to private repository
ID1390966758, planned to become `DanielNetzer/forgecell-private-history` (access required).
Retained frozen ledgers, issue snapshots, approvals, patches and JSON preserve original
bytes and embedded URLs as historical identifiers, not current navigation or execution
targets. The current source/root identity is separate; this notice does not record a
completed rename or publication.

The install command remains pinned to the legacy `forgecell-releases` release
`0.2.0-preview.1`. Its historical endpoint uses the unprefixed version tag;
existing assets and checksums remain unchanged. A new core release is being
prepared at `DanielNetzer/forgecell` with a version-pinned installer and `v` tag.
This preparation does not establish public availability.

Publication requires local verification, independent review, import of the reviewed
clean core snapshot, public-visibility approval, exact-head Actions passes, draft
review, approved publication, then anonymous install/upgrade/rollback validation.
Only after those gates should README and install docs switch together to one
identical pinned command. Build a fresh release from the final reviewed public
source commit; never relabel an old executable. Any relocated artifact must retain
its original version, checksums and provenance. Installation requires no GitHub
token, Go or Node; ticket execution requires Git, authenticated gh and a configured,
authenticated coding harness. Backlog publication remains paused.
