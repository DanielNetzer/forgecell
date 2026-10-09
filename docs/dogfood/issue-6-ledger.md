# Molecule mol-6-20260927T215456Z-c1cf6b12

- Status: waiting
- Formula: forgecell-github-issue
- Formula approved: yes
- Started: 2026-09-27T21:54:56.240Z
- Finished: 2026-09-27T21:56:47.656Z
- Issue: #6 Dogfood: preserve Unicode in streamed harness output
- URL: https://github.com/DanielNetzer/forgecell/issues/6
- Repo: DanielNetzer/forgecell
- Intake source: github-issues — DanielNetzer/forgecell#6
- Formula SHA-256: 0402d3ed39f71e473d53017a70396a7810f622d5bc52d96bb1ba5895bab7fa91

## Atoms

### intake (intake) — done

#6 Dogfood: preserve Unicode in streamed harness output

- done: read · github-issues · DanielNetzer/forgecell#6 — https://github.com/DanielNetzer/forgecell/issues/6
### harness (harness) — done

Implemented Unicode-safe streaming for stdout and stderr, preserving existing invocation controls. Added a deterministic regression test: failed before the fix, passed afterward.

Validation from `lab/`:

- `npm test`: blocked by sandbox IPC `EPERM`.
- `env -u NO_COLOR FORCE_COLOR=3 node --import tsx --test src/*.test.ts src/*.test.tsx`: **47 passed, 0 failed**.
- `npm run typecheck`: exit 0.
- `npm run build`: exit 0.
- `git diff --check`: exit 0.

Intake is recorded; final harness outcome recording remains with the parent Molecule after return. No commits or external mutations performed.

- done: execute · codex · /opt/homebrew/Cellar/node/25.9.0_2/bin/node
### check (check) — recorded

Recent runs — Lab CLI: success; Lab CLI: success; Lab CLI: success. Read-only context, not proof that this Molecule passed CI.

- done: read · github-actions · Lab CLI — https://github.com/DanielNetzer/forgecell/actions/runs/36351314816
- done: read · github-actions · Lab CLI — https://github.com/DanielNetzer/forgecell/actions/runs/36351046423
- done: read · github-actions · Lab CLI — https://github.com/DanielNetzer/forgecell/actions/runs/36336676794
### gate (gate) — waiting

Human gate is open. No merge or deployment is authorized.

- waiting: approve · local · Human approval
### ship (ship) — skipped

Lab records the Molecule and does not merge or deploy.

- Planned (not run): write · github · Merge / deploy (not run)
### document (document) — skipped

No comment was posted to the GitHub Issue.

- Planned (not run): write · github-issues · Issue comment (not run) — https://github.com/DanielNetzer/forgecell/issues/6
### learn (learn) — skipped

Generate a human-reviewed Formula proposal with: forgecell learn mol-6-20260927T215456Z-c1cf6b12

## Notes

- Ran approved Formula forgecell-github-issue.
- Configured BYO harness executed. Its exit status is not proof of shipping. Lab did not merge, deploy, or comment.

