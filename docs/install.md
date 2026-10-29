# Install Forgecell CLI

Public CLI `v0.3.1` release; core source licensed under the [MIT License](../LICENSE). No repository access, Node or Go is needed to install.

```sh
sh -c 'set -eu; script=$(mktemp "${TMPDIR:-/tmp}/forgecell-bootstrap.XXXXXXXX") || { echo "Forgecell: cannot create bootstrap temporary file." >&2; exit 1; }; trap '"'"'rm -f "$script"'"'"' 0; trap '"'"'exit 1'"'"' HUP INT TERM; curl -fsSL --proto "=https" --proto-redir "=https" --connect-timeout 15 --max-time 30 https://github.com/DanielNetzer/forgecell/releases/download/v0.3.1/install.sh -o "$script" || { echo "Forgecell: bootstrap download failed." >&2; exit 1; }; [ -s "$script" ] || { echo "Forgecell: bootstrap download was empty." >&2; exit 1; }; sh "$script"'
```

The command downloads the bootstrap over HTTPS before executing it. A failed download
(including HTTP errors or the 30-second timeout) or an empty response prints a failure
and returns nonzero. Its temporary file is removed on exit.

Packages: macOS and Linux, arm64 and amd64. The bootstrap needs curl, standard shell utilities, and shasum or sha256sum. It verifies the executable checksum and version before activation. Checksums are delivered alongside binaries over GitHub HTTPS, not by an independent signing authority.

## First run

The four words you will see everywhere:

- **Lab**: the runtime on your machine and its records for one repository.
- **Formula**: the YAML recipe you approve. It names the steps and the coding agent.
- **Molecule**: one Issue's unit of work and its ledger.
- **Atom**: one step of a Molecule, such as the plan, coding, checks or review.

You need Git, the GitHub CLI signed in (`gh auth login`), and one coding agent signed in
with its own CLI: Claude Code, Codex or Cursor. Forgecell never asks for those
credentials. GitLab and Jira are not supported yet (post-v1, #259).

1. **Install.** Run the command above. Make sure `~/.local/bin` is on your PATH; the
   installer does not edit shell profiles.
2. **Open the UI.** `cd` into the repository you want Forgecell to work on and run:

   ```sh
   forgecell ui
   ```

   Your browser opens on the Lab UI. Everything below happens there.
3. **Set up your Lab.** When the checkout has no ready Lab, the UI opens on Set up your
   Lab and walks you through five steps, one at a time. If more than one Lab already
   matches the repository, it first shows **Choose a Lab**: pick one and press Open Lab;
   the others stay as they are.
   1. **Repository**: the GitHub repository, its default branch and your `gh` sign-in
      status. This step only reads; sign-in stays in `gh`.
   2. **Coding agent**: pick claude-code, codex or cursor. Each one shows whether it is
      ready and signed in. An agent that is not ready can't be picked: install it, sign
      in with its own CLI, then press Check Again.
   3. **Create the Lab**: press Create Lab. Forgecell saves the Lab and proposes its
      first Formula, as `forgecell init --harness` does. Nothing is approved yet.
   4. **Approve the Formula**: review the Atoms in order, the baseline checks taken from
      your pull-request CI and the gate policy. Open Details to see the proposal's
      digest (Copy Digest copies it). Then press Approve Formula. Approval binds that
      exact proposal and digest; if it changes before you click, nothing is approved.
      The review Atom is optional: tick the box to propose it after this approval (or
      press Propose Review Atom when setup is done). It adds a second agent that
      reviews each change before you do, and it is a separate Formula change that you
      approve separately.
   5. **Done**: your Lab is ready and the UI opens on it.
4. **Issues.** The UI's views are Inbox, Issues, Work, Insights and Formula. The Issues
   view lists the repository's open Issues with their Forgecell state; Search finds one
   by number or title, and Refresh reads GitHub again. Press Start intake on one Issue
   to start a Molecule for it. Or filter by label or milestone and press Run the loop on
   these: it fills the loop selector (for example `milestone:<name>`), and Start loop
   starts the loop. The loop starts intake for matching Issues and stops each Molecule
   at every human gate. The loop panel shows the running loop, and Stop loop stops it.
   Inbox shows what needs you, and Work lists every Molecule.
5. **Molecule view.** Open a Molecule to see its plan: what Forgecell will do, the files
   it may change, the checks it must pass and what counts as done. Press Approve plan &
   start coding to approve exactly the plan shown, or Request changes. Then watch coding
   and the checks, which run independently in a fresh checkout. At the review gate,
   press Preview delivery to see the diff and PR body, then Publish draft PR. Merging
   stays on GitHub: Forgecell never merges, deploys or closes Issues.
6. **When something needs you.**
   - **Unblock**: unblocking is not a separate view. Inside the Molecule view, a blocked
     Molecule shows the Why it's blocked · What you can do panel, with the next actions
     the engine offers, such as accepting failures that happen by design, approving a
     requested scope change, re-running checks or recovering. Each one records a
     revision or an interruption and never approves a gate for you. Under Not needed
     anymore? you can stop or retire a Molecule. Retire attempts for closed Issues lists
     the Molecules of closed Issues and retires them after you confirm.
   - **Formula**: the active Formula and its proposals. Rebind to This Build after an
     upgrade, Refresh Baseline from CI when your pull-request checks change, Enable
     In-Loop Review to add the review Atom, and Propose Model Pin to pin the model an
     agent role runs. Each button creates a proposal that you review and approve.
   - **Insights**: the Lab overview, with the live Molecules by lane and the ones that
     need your decision, then cost, tokens and time By model, for each Atom role, with a
     weekly chart. A value the agent did not report reads "not reported", never 0.

Learning is not driven from the UI yet: proposing and approving Formula suggestions from
finished ledgers still uses `forgecell learn` and `forgecell suggestion`. Outside
reviewers are not supported yet (post-v1, #260).

### Try the demo first

`forgecell demo` runs one complete Molecule against a built-in sample repository with a
deterministic stub harness, printing each stage as it runs. It needs only Git and Go on
PATH: no API key, no GitHub token, no `gh`, no network and no repository of your own, and
it publishes nothing. Add `--keep DIR` (an absent path or an empty directory you own) to
keep its Lab, then inspect the Molecule with `forgecell ledger MOLECULE_ID --lab DIR`.
`forgecell ui --demo` shows sample data in the UI without reading any Lab.

### Prefer the terminal?

Every step above has a CLI command. Run them inside the repository; each picks the
same default Lab as the UI, or the one you give with `--lab DIR` or, for a named Lab of
the repository, `--lab-name NAME`. `forgecell labs` lists every Lab, read-only.

```sh
forgecell doctor                          # check Git, gh and the coding agents
forgecell init --harness claude-code      # create the Lab and propose its Formula
forgecell init --approve PROPOSAL_ID      # approve the Formula you reviewed
forgecell run ISSUE_NUMBER                # start a Molecule; approve with --approve DIGEST
forgecell loop --select milestone:NAME    # or oldest, or label:NAME
forgecell amend MOLECULE_ID ...           # change scope or continue a blocked Molecule
forgecell deliver MOLECULE_ID --lab DIR --base BRANCH -- FILE...   # preview, then --approve DIGEST --title TITLE
```

`loop --max-concurrent N` sets how many Molecules it advances at once (default 1) and `--once` makes a
single pass; `loop --watch` repeats a pass every `--interval` (default 5m), and
`--learn` also runs the pending learn requests after each pass, like
`learn --pending`. `retire MOLECULE_ID --reason TEXT` retires a stale Molecule and
keeps every record; `recover MOLECULE_ID --plan DIGEST --confirm-stopped` recovers an
interrupted one; `checks` reads a pull request's required CI checks at an exact commit for
a Molecule's delivery and exits nonzero unless they passed.

`init --rebind ID`, `init --refresh-baseline`, `init --enable-review` and
`init --model ID` are the Formula view's buttons. `doctor` performs read-only capability
and authentication checks; it does not prove model access or repository policy
readiness. `forgecell --help` lists every command and flag.

The `v0.3.1` release uses a distinct directory under `~/.forgecell/labs/`
for each checkout by default; `forgecell doctor --json` reports the exact location.
Existing preview.1 checkout Labs in the ignored `.forgecell` directory are left
intact. Use the same `--lab .forgecell` path for every command to continue using one;
`init` will not silently replace its approved Formula with a new home Lab. Labs and
saved bindings are not automatically migrated or rebound. With preview.1, an explicit
path such as the example above keeps records outside the checkout.

## Versions, upgrade and rollback

The command above pins `v0.3.1`. v0.3.1 is the first release that drives the whole loop from `forgecell ui`. Repeating it safely reinstalls that version. For another GitHub release, use its matching versioned installer; `FORGECELL_VERSION` overrides require the generic `FORGECELL_RELEASE_BASE` layout. Review release notes before upgrading.

`forgecell rollback` restores the previous verified installation when one exists. Installation preserves Formulas and ledgers, stores versioned executables under `~/.forgecell/releases`, and refuses conflicting unmanaged launchers.

After upgrading, rebind your Lab to the new build: open the Formula view, press Rebind to
This Build, review the proposal and approve it. From the terminal, `forgecell init --rebind ID`
proposes the same binding-only change. `forgecell doctor` checks the Lab's saved
binding; a stale binding needs a reviewed rebind before runs use it.
Developer builds made with `forgecell dev install` record their checkout's commit time,
so a rebind can tell which of two dev builds of the same version is newer.

`FORGECELL_HOME` and `FORGECELL_BIN` customize paths. Set these variables before the opening `sh -c` so they reach the downloaded installer, not only curl.

## Resources and cleanup

Forgecell runs full test suites, parallel cohort attempts and worktrees on your machine, so it guards disk and memory before and during that work.

- **Floors.** Free disk is checked on the volumes holding `TMPDIR`, the Lab and `GOCACHE`: the soft floor is the larger of 15 GiB and 10% of the volume, capped at 30 GiB, and the hard floor the larger of 5 GiB and 3%, capped at 10 GiB, so a large disk never demands an unreasonable amount of free space (on a 2 TB disk the floors are 30 and 10 GiB). `forgecell doctor` shows the effective floors for each volume. Available memory (on macOS the kernel's memory-status level times physical memory, from `sysctl kern.memorystatus_level hw.memsize`, which is the figure `memory_pressure` reports, with free plus inactive pages from `vm_stat` only as a fallback; on Linux `MemAvailable` from `/proc/meminfo`) has a soft floor of 3 GiB and a hard floor of 1.5 GiB. Override them in `<lab>/resources.json`, for example `{"diskSoftFloorGiB": 20, "diskHardFloorGiB": 8, "memorySoftFloorGiB": 4, "memoryHardFloorGiB": 2}`. A configured disk floor replaces the percentage. No floor goes below 2 GiB of disk or 1 GiB of memory. For `evaluate --cohort` the file is read from `--out`.
- **Below a soft floor**, a Molecule's verification stage or a cohort attempt is not started. Forgecell re-checks every 30 s for up to 10 min, then stops cleanly with the reason `resources: low disk` or `resources: low memory`. Work already running keeps running. `loop --watch` skips the pass and prints the reason in its pass report.
- **Below a hard floor while work runs** (sampled every 10 s), the running check processes are killed through their process group and the reason `resources-exhausted` is recorded. A cohort attempt cancelled this way is discarded, not recorded, so resuming the cohort re-runs it.
- **Verification slots, stalls and fail-fast checks.** `verificationSlots` in `<lab>/resources.json` (1 to 8) sets how many Molecules may verify at the same time. When it is unset the limit is 1; a new Lab is written with `{"verificationSlots": 2}`. The resource guard grants the first slot as usual, refuses every extra slot while any reading is below its soft floor, and allows one slot per 4 GiB of available memory, so you may get fewer than you configured. A wall-clock watchdog ends a verification check that runs past its time budget as interrupted (stalled), even when the machine slept in between, so a stuck check never runs for hours. It never judges a check by silence or CPU use. Within a view, the checks run cheapest first (formatting and vet, then builds, then targeted tests, then the full suite), and setup commands always run first; once a required check fails, the remaining checks are recorded as not run, never as passed.
- **Parallelism.** `evaluate --cohort --parallel N` runs at most one attempt per 4 GiB of available memory and prints the reduced level. Each attempt's `go` commands get `GOFLAGS=-p=<CPUs / attempts>`.
- **Temporary directories.** Every Forgecell temp directory holds a `.forgecell-owner` file (pid and start time) and is removed on exit and on SIGINT or SIGTERM. It is removed even when it contains a read-only Go module cache. At every start, Forgecell removes your own `$TMPDIR/forgecell-*` directories whose owner process is gone, or that have no owner file and are over 24 h old. It never touches anything else.
- **Check temp files.** A Molecule's check views share one stable `TMPDIR` per Lab, outside the Lab and so outside your home: `<temp root>/forgecell-lab-<16 hex digits>`, where the digits start the SHA-256 of the Lab's resolved path. The temp root, with symbolic links resolved, is the first of: `$FORGECELL_LAB_TMP_ROOT`, then `$TMPDIR`, each only when set, an existing directory and not inside your home (`$HOME`); otherwise the per-user default, which on macOS is your per-user temp dir (`getconf DARWIN_USER_TEMP_DIR`, under `/var/folders`), on Linux `/tmp` and elsewhere the system temp dir. A disk-backed `$TMPDIR` is therefore kept, and a nested run inside a check view stays under that view's `TMPDIR`. A set root that is passed over, or a per-user temp dir macOS cannot report, is recorded as a note on the Molecule. Go's `GOTMPDIR` is its `gotmp` subdirectory. It is created at 0700 with a `.forgecell-check-tmp` marker and a `lab` file naming its Lab; reusing your own directory at that path makes it 0700 and writes either file again if it is missing. A symbolic link, a non-directory or another user's directory at that path is refused, and so is a per-user default that lies inside your home; the views then use a private `TMPDIR` and the Molecule records a note. If the directory vanishes during a verification (an OS temp cleaner), the remaining views use a private `TMPDIR` and the Molecule records a note. It is kept, never swept as leaked while its Lab exists; each verification first removes your entries in it older than 24 h. A marked `forgecell-lab-*` directory older than 24 h whose `lab` file names a path that no longer exists, or that has no `lab` file, is an orphan: the start sweep (in `$TMPDIR`) and each verification (in its temp root) remove it. Forgecell versions before this one used `<lab>/tmp`, which is no longer used or pruned automatically.

`forgecell doctor` shows a **Resources** section: free disk and memory against the floors, the Lab size, the staged release count and size, the `GOCACHE` size and the stale temp-directory count. Above 20 GiB it suggests `go clean -cache`, which Forgecell never runs itself.

`forgecell doctor --clean` lists what it would remove and asks before removing anything; `--yes` skips the question. It removes:
- staged releases beyond the active one, the previous one (kept for `rollback`), the 5 newest and any release referenced by an absolute path into `~/.forgecell/releases/<version>/`. The listing says why each kept release is kept. Exactly these places are scanned for references: every file in the `formulas` directory of each Lab under `~/.forgecell/labs`, of the Lab given with `--lab` (or the default Lab), and of the legacy checkout Lab at `<checkout>/.forgecell` when it exists; and the proposed Formula of every `suggestions/*.json` with status `pending` in those same Labs. Symlinked Lab and `formulas` directories are followed, and each referenced path is also matched after resolving symlinks. Releases are re-checked under the install lock just before removal;
- the verification trees (`verification/<id>-r<N>` and `verification/<id>-r<N>-protected`) and workspace trees of Molecules that finished more than 14 days ago. Trees of interrupted Molecules and of Molecules with a pending recovery proposal are kept, and a Molecule locked by a running loop or command is skipped. Under the Molecule's lock, just before removal, its ledger is read again: if its status changed, it is no longer prunable, or a tree's path changed or another ledger now uses its workspace, the trees are kept and the output says why. For a blocked or failed Molecule the listing warns that it can no longer be amended or recovered once its trees are removed.
- entries of the Lab's check `TMPDIR` (above) older than 24 h; the directory, its marker, its `lab` file and `gotmp` itself are kept, and old children of `gotmp` go.
- a legacy `<lab>/tmp` that holds the `.forgecell-check-tmp` marker, removed whole. A `<lab>/tmp` without the marker is never touched.
- orphaned check `TMPDIR`s (above) of Labs that no longer exist, looked for under both the per-user default root and the current temp root.

Both temp locations are considered only when the Lab has a `ledgers/` directory. When one cannot be inspected (a symbolic link or a file at `<lab>/tmp` or at the check `TMPDIR`, or a temp root inside your home), `doctor --clean` prints a warning, skips it and still cleans the rest.

Ledgers, history and evidence files are never removed. A removed workspace stays registered as a Git worktree of its source until you run `git worktree prune` there.

## Tips for running a Lab

- **Edit an Issue before you start it.** Intake reads the Issue body (not comments) once. Changing the body while its Molecule is planning or waiting for plan approval makes the plan stale ("readiness inputs changed"), and you must start intake again.
- **Keep the computer awake for long unattended runs.** A check whose wall clock runs far past its budget, for example during sleep, is stopped as stalled ("… the machine may have slept"). Rerun its checks only: Re-run checks in the UI, or `amend --continuation checks-only`.
- **Prefer `milestone:<name>` (or `label:<name>`) over `oldest`** when the repository has parked or future Issues. `oldest` picks the oldest open Issue in any milestone.
- **When a change deliberately updates an existing test,** the protected view reports it. Accept it with Accept by-design failures (the "Accept by-design test failures" action), which lists exactly the tests it declares.
- **Two verification slots** (`<lab>/resources.json` → `{"verificationSlots": 2}`) let two Molecules check at once. On a 16 GB machine, don't go higher: the full suites start hitting package timeouts.

## Local UI

```sh
forgecell ui [--lab DIR] [--lab-name NAME] [--port N] [--no-open] [--demo]
```

`forgecell ui` is where you drive the Lab from your browser: Set up your Lab, Inbox, Issues, Work and each Molecule view, Insights and Formula (see [First run](#first-run)). It picks the Lab the same way as the other Lab commands (`--lab DIR`, `--lab-name NAME`, `FORGECELL_LAB` or the default Lab for the checkout).

What it does and doesn't do:

- **Every write goes through the engine.** Each write is a POST that the engine performs with the same checks as the CLI: digests, locks and receipts. Approvals are bound to the exact plan, proposal or preview digest you saw; if it changed before you clicked, nothing is approved. The writes are: Set up your Lab, creating, approving or dismissing Formula proposals, starting intake, starting and stopping the loop, stopping a Molecule this UI runs, approving a plan, requesting changes, the next actions of a blocked Molecule, retiring Molecules and publishing a draft PR.
- **It never merges or deploys,** and it never changes permissions. A blocked Molecule's next actions record a revision or an interruption and never approve a gate; recovery runs only after you confirm it.
- **Until setup is done, it refuses every other API route** with "set up your Lab first; nothing was read or written".
- It listens on `127.0.0.1` only, on a free port unless `--port N` is given, prints a `http://127.0.0.1:<port>/?t=<token>` URL and, unless `--no-open` is given, opens it through a private (0600) launch page in a Forgecell temp directory, so the token never appears in a process's arguments; the page is deleted at the first exchange, or when the UI stops. The URL is always printed too, for a browser that cannot read the page (such as a snap-confined one). Press Ctrl-C (SIGINT or SIGTERM) to stop it.
- The token is new for every launch and single-use: the first visit exchanges it for an HttpOnly, SameSite=Strict cookie holding a separate random session secret, plus a second random value that the page keeps in that tab's `sessionStorage` and sends as an `X-Forgecell-Session` header on every API request (browsers send `127.0.0.1` cookies to every port, so the cookie alone is not enough). Any later visit to the URL gets 403; a new tab has no session and says so; restart `forgecell ui` for a new link. Requests without that cookie, with another `Host`, or from another origin get 403. Do not share the URL.
- Every write also needs the exact `Origin`. The UI never returns credentials, the environment, the Lab or workspace directory, or other absolute local paths: in free text the Lab, workspace, source and home directories become `<lab>`, `<workspace>`, `<source>` and `~`, any other path under a local root (`/Users`, `/home`, `/root`, `/private`, `/var`, `/tmp`, `/Volumes`, `/opt`, `/etc`, `/mnt`, `/srv`, `/System`, `/Library`, also as `file://`) becomes `[local path]`, and evidence paths are shown relative to the Lab.
- Lanes: **Blocked** (status blocked or failed, even with a receipt), then **Needs my decision** (phase `scope-waiting` or `review-waiting`), then **Running** (status running or allocating, or an active phase), then **Finished · Draft published** (a `published` delivery receipt whose lifecycle is not CLOSED, UNKNOWN, MISSING or AMBIGUOUS). Anything else counts as **Other**, so the lane counts always add up to All. A prepared or committed receipt is shown as "Delivery in progress" with its exact state. Draft published never means merged.
- Not wired in this version, and shown as Unavailable: cost per merged PR, Formula suggestions, evaluation and pause. Learning and evaluation stay in the CLI (`learn`, `suggestion`, `evaluate`); there is no pause.
- `--demo` serves the design prototype's synthetic Molecules, labeled Sample data, and reads no Lab; it does not resolve the default Lab. The real mode never shows synthetic values.

## Contributors

Core source may be used and distributed under MIT; retain its copyright and permission notice.
Keep local Lab records, credentials, environment files and raw harness traces private.
Native maintainers use [the distribution design](design/native-cli-distribution.md). Source access is not required for users of the public CLI packages.

## Published public-core release and legacy provenance

The pinned installer uses `DanielNetzer/forgecell` and the `v0.3.1` tag.
The earlier published [v0.2.0-preview.2 prerelease](https://github.com/DanielNetzer/forgecell/releases/tag/v0.2.0-preview.2)
was built from public-core source `801a9846fc9066dde985bb678348cac2bc89a249`.
Installation requires no GitHub token, Git, gh, Go or Node.

The legacy `DanielNetzer/forgecell-releases` preview `0.2.0-preview.1` used an
unprefixed version tag. Its original assets, checksums and provenance remain unchanged.
Checksums from the same GitHub HTTPS endpoint verify integrity, not independently
signed authenticity. Never relabel an old executable as a new release.

Future releases require local verification, independent exact-source review,
exact-head Linux/macOS CI, draft review and approved publication, followed by anonymous
install/upgrade/rollback validation before updating the identical README and install
commands. This documentation patch also requires independent exact-source review and
exact-head Linux/macOS CI before merge. Operator release evidence is separate from
source tests; publication does not authorize backlog merges.
