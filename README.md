<p align="center">
  <img src="docs/assets/forgecell-mark.svg" width="48" height="48" alt="Forgecell four-dot logo">
</p>

<h1 align="center">Forgecell</h1>

<p align="center">
  <strong>Compose once. Improve forever.</strong><br>
  A local software Lab. Your harness, your model, your workflow.
</p>

<p align="center">
  <a href="#get-started">Get started</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="lab/README.md">CLI guide</a> ·
  <a href="LICENSE">MIT license</a>
</p>

<p align="center">
  <img src="docs/assets/forgecell-lab.png" width="1200" alt="The Forgecell Lab: a glass pavilion in a botanical garden at golden hour, with a white creature wearing brown goggles.">
</p>

Forgecell helps an individual engineer turn GitHub Issues into reviewable work using
their own coding harness and model. Every run leaves a ledger. Those records can inform
proposed improvements to the durable recipe that guides the next run.

**Start on your machine. Keep the evidence. Improve the process.**

> **Early preview.** The current Go source supports ticket analysis, approved execution,
> verification and separately approved draft PRs. The downloadable `0.2.0-preview.1`
> is older and does not include ticket readiness. Merge, deployment and issue closure
> remain manual. The Lab is terminal-first; there is no local GUI yet.

## How it works

**Approve a recipe → choose an issue → review the plan → run and verify → review the result.**

- **Compose your workflow.** A Formula defines the steps and binds your existing harness.
- **Execute with clear boundaries.** The development CLI proposes scope and checks before
  coding, works in an isolated checkout, and verifies the result in a fresh checkout.
- **Record what happened.** A Molecule retains the issue, Formula snapshot, Atom outcomes
  and verification evidence. A draft PR requires its own explicit approval.
- **Improve the recipe.** A separately bound learn Atom can propose a Formula diff from
  finished ledgers. You review the change and evaluate whether it actually helps.

The coding harness changes the software. The meta harness proposes changes to **how the
software is made**. Approval permits an experiment; it does not prove an improvement.

### Four primitives. One Lab.

| Primitive | Meaning |
| :--- | :--- |
| **Atom** | One workflow step |
| **Molecule** | One ticket unit of work + its ledger |
| **Formula** | Durable YAML recipe; learning patches this |
| **Lab** | The runtime you install |

The **Lab runs Molecules**; each Molecule is **Atoms arranged by a Formula**.

## Get started

Install the compiled preview for **macOS or Linux**, on **arm64 or amd64**.
No Node, Go or source checkout is required to install it.

**[Installation guide →](docs/install.md)** — prerequisites, PATH, upgrades and rollback.

<details>
<summary>One-line installer · pinned to 0.2.0-preview.1</summary>

```sh
sh -c 'set -eu; script=$(mktemp "${TMPDIR:-/tmp}/forgecell-bootstrap.XXXXXXXX") || { echo "Forgecell: cannot create bootstrap temporary file." >&2; exit 1; }; trap '"'"'rm -f "$script"'"'"' 0; trap '"'"'exit 1'"'"' HUP INT TERM; curl -fsSL --proto "=https" --proto-redir "=https" --connect-timeout 15 --max-time 30 https://github.com/DanielNetzer/forgecell-releases/releases/download/0.2.0-preview.1/install.sh -o "$script" || { echo "Forgecell: bootstrap download failed." >&2; exit 1; }; [ -s "$script" ] || { echo "Forgecell: bootstrap download was empty." >&2; exit 1; }; sh "$script"'
```

The installer verifies the executable checksum and version before activation and
preserves local Lab data. This command uses the existing release endpoint; a release
from the current core source has not been published yet.

</details>

After installing, open the repository you want Forgecell to work on. Ticket execution
requires Git, authenticated GitHub CLI (`gh`), and an authenticated coding harness.
Choose a distinct Lab directory and use it throughout:

```sh
lab_dir="$HOME/.forgecell/labs/YOUR_REPOSITORY"
forgecell doctor --lab "$lab_dir"
forgecell init --lab "$lab_dir"

# Review the saved Formula, then replace PROPOSAL_ID with its actual ID.
forgecell init --lab "$lab_dir" --approve PROPOSAL_ID
forgecell run ISSUE_NUMBER --lab "$lab_dir"
```

The published preview needs this explicit `--lab` path to keep its records outside the
checkout. The development CLI defaults to a private Lab under `~/.forgecell/labs/`.
See the **[CLI guide](lab/README.md)** to build the current Go source and use ticket readiness.

### Bring your harness

Onboarding detects Codex, Claude Code and Cursor, checks their capabilities and
authentication, and proposes a Formula for review. It does not silently activate it.

Binding support and read-only ticket-analysis support are different: Codex analysis is
verified; other bindings have [documented limits](docs/ticket-readiness.md#provider-and-policy-support).
Your harness supplies coding and reasoning. Forgecell keeps the workflow and its evidence.
A learn binding is separate and is not silently configured during onboarding.

## Learn from evidence

**Observe → propose one process change → review → run again → compare.**

Keep actual outcomes, failed attempts and uncertainty visible. Missing cost is unknown,
not zero. A faster run is not better if it skips verification or weakens permissions.
The first historical paired trial found no observed correctness or recovery advantage;
its original research bundle stays private.

Read the [learning contract](docs/learning.md) and [dogfood findings](docs/dogfood/README.md)
for the evidence and its limits. Historical records are labeled separately from current
repository activity.

## Build with us

Forgecell's local runtime is written in **Go**. With the version specified in
[`lab/go.mod`](lab/go.mod) installed:

```sh
cd lab
go test ./...
go vet ./...
go build -o bin/forgecell ./cmd/forgecell
```

The target is **issue → production**. Current delivery stops at explicitly approved draft
PRs; merge, deployment and production verification are not automated. Local UI work is
still design exploration. Contributions should preserve clear ledgers, explicit gates
and provider-neutral orchestration.

| Explore | Read |
| :--- | :--- |
| Product and terminology | [Product bible](docs/product-bible.md) · [Ontology](docs/ontology.md) · [Atoms](docs/atoms.md) |
| Onboarding and execution | [Agent onboarding](docs/agent-onboarding.md) · [Ticket readiness](docs/ticket-readiness.md) |
| Learning and direction | [Learning](docs/learning.md) · [Pillars](docs/pillars.md) |

---

Made with love by **Forge Labs**. Licensed under [MIT](LICENSE).

Keep credentials, local Lab records and raw harness traces off GitHub.
