# Lab UI redesign: approved mockup (2026-10-26)

The operator approved these four screens on 2026-10-26. They are the visual and UX reference for the Lab UI redesign slices. Open each file directly in a browser. The asset paths point at `lab/internal/ui/web/assets/`.

| File | Screen | What it settles |
|---|---|---|
| `Main.html` | Inbox (the new home) | App shell: header with repo, loop status pill and operator avatar. Left nav: Inbox, Issues, Work, Insights, Formula. "Needs you" cards with one primary action each. "In progress" with a 7-step progress bar. Recently merged. A side panel with the resident and the **Run the loop** form (scope selector, stepper, primary button). |
| `Plan.html` | Review a plan | The stepper. The plan in plain words. **Files it may change**, each marked NEW or EDIT with its reason. **Checks it must pass.** A sticky decision card with one primary **Approve plan & start coding** and a secondary **Request changes…**. The digest is collapsed under "Plan details". Retire sits in a separate collapsed "Not needed anymore?" danger box. |
| `Issues.html` | Issues | Every open Issue, including ones never started. A segmented state filter with dots and counts, milestone and label pickers, search, and a table with one action per row (Start / Review plan / Open). "Run the loop on milestone v1". |
| `Mobile.html` | Inbox on a phone | Stacked cards, full-width primary buttons, a bottom tab bar. |

## Design rules taken from the mockup
- **Tokens:**
  - ground `#F3F2E8` with soft radial blooms;
  - paper `#FFFEF8`, ink `#183E32`, muted `#56695E`, subtle `#6B7F72`, edge `#D9E3D6`, strong edge `#B9CCBA`;
  - primary forest `#1E4D3B` (hover `#173E30`), teal `#239F89`;
  - **Needs you:** amber `#D99A2B`, text `#7A5410` on `#F6E7C8`;
  - **Running:** teal, text `#1F5F52` on `#DCEFE9`;
  - **Blocked:** clay `#B4492F`, text `#8A3322` on `#F4DDD5`;
  - **Merged:** forest `#2D5B46`;
  - focus ring `#14523C`.
- **Glass panel:** `background: #fffef8e6; backdrop-filter: blur(20px) saturate(160%); border: 1px solid #1e4d3b1f; box-shadow: inset 0 1px 0 #ffffffd9, 0 1px 2px #1a3a2a14, 0 12px 32px #1a3a2a12;` Radius 20 px for panels, 12–14 px for controls and inner cards.
- **Buttons:**
  - **Primary:** forest fill, 40 px (48 px for the main decision), radius 12–14 px, with an inner highlight and shadow.
  - **Secondary:** paper fill with a `#B9CCBA` border.
  - **Danger:** paper fill, text `#8A3322`, border `#E2B9AD`. Always kept in a separate block, never next to a primary.
- **Plain language:** "Plan ready for your approval", "Coding · 18 min", "Ready to publish", "7 of 7 checks passed". Never show raw phase codes, `assay-…` ids, full digests or ISO timestamps. Use `Intl` dates and relative time, and shortened digests only behind "details".
- **One primary action per card or row.** Destructive actions are behind a disclosure plus a confirmation.
- **Status:** a dot plus text, never colour alone. Counts use tabular numbers.
- **Decoration:** the fern and orchid sit behind panel edges and are `aria-hidden`. The resident's pose follows Lab state: waving when something needs you, studying a cube while work runs, reading when idle.
