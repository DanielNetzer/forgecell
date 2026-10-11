# Pinned capability-provider extensions: implementation plan

> **For agentic workers:** each task below is delivered as one GitHub issue and run as one
> Forgecell Lab Molecule (operator-chosen execution). Steps use checkbox (`- [ ]`) syntax.
> Tasks 0 and 7 are operator tasks and are never run by a coding harness.

**Goal:** let a Formula attach pinned, out-of-process MCP providers (first: a Graphify
repository index) to the Claude analysis and coding roles. A Lab-owned prepare/cache step
builds each index once per base commit, outside the checkout.

**Architecture:**

- A new `pinned` package holds the file-pinning rules from the analysis wrapper (#2).
- A new `extension` package owns the `mcp-provider/v1` manifest, its verification and the
  prepare/cache lifecycle.
- The Formula gains a top-level `extensions` map and a per-binding `extensions` list.
- The molecule layer prepares and materialises the extensions for each role. It passes
  verified server definitions to `harness.BuildInvocation`, which emits Claude's
  `--mcp-config` and `--allowedTools`. The adapter protocol becomes `2`.

**Tech stack:** Go 1.27 (module `github.com/DanielNetzer/forgecell/lab`), `go.yaml.in/yaml/v4`,
the existing `process.Run`, and `git archive` for snapshots. No new third-party Go
dependencies.

**Spec:** `docs/design/2026-10-11-extensions-design.md` (PR #102). Read it with this plan.

## Global constraints

- Extensions are hints. They never approve, decline, deliver, merge or change gates. They
  never write to the coding checkout or the ledger.
- v1 supports Claude Code only. Codex and Cursor reject role bindings with extensions at
  proposal time.
- Manifest kind is `mcp-provider` with `schemaVersion` `v1`. The manifest is at most 64 KiB
  and strictly decoded (no unknown fields, no duplicate keys, no trailing data).
- Extension name pattern: `^[a-z][a-z0-9-]{0,31}$`. Tool name pattern:
  `^[A-Za-z0-9_-]{1,64}$`. At most 32 tools.
- Placeholders: only `{{CACHE}}` and `{{SOURCE}}`, as a whole argument or a path prefix.
  The literal path of any pinned file must not appear in `args`.
- Prepare timeout is at most 1 800 000 ms. Each cache entry is at most 1 GiB, and an
  oversize output means unavailable.
- Cache key: `(name, manifest sha256, base commit)`. Directory:
  `<Lab>/extensions/<name>/<manifest-sha>/<base-commit>/`, mode 0700 while building, then
  read-only (directories 0500, files 0400).
- `required` defaults to `false`. When false, a failure makes the extension unavailable
  and the role runs without it. When true, intake stops.
- Analysis receives only tools with `"access": "read-only"`.
- `AdapterProtocolVersion` becomes `"2"`, and the protocol-2 contract digest is pinned in
  `protocol_contract_test.go`.
- `extensions` fields are `omitempty` everywhere, so Formulas, plans and ledgers without
  extensions are byte- and digest-identical to today.
- No MCP request or response content is recorded in v1.
- Every Molecule PR must pass the Formula baseline: `go test ./...`, `go vet ./...`,
  gofmt and both hygiene scripts.

## Review focus

The five conditions most likely to bite a user, which no other task tests. Each is
pinned by a test in the owning task:

1. **An MCP server that exits or hangs at startup.** The role still completes under the
   harness timeout, and the ledger shows the extension was available (Task 5, test
   `TestRoleCompletesWhenExtensionServerDies`).
2. **A huge index.** Prepare output over 1 GiB makes the extension unavailable and leaves
   no partial cache (Task 3, `TestPrepareOversizeOutputIsUnavailable`).
3. **A base commit missing from the source repository** (shallow clone or rewritten
   history). The snapshot fails, the extension is unavailable with a clear reason, and the
   role continues (Task 3, `TestPrepareMissingBaseCommitIsUnavailable`).
4. **A manifest path that escapes the Lab or points into the coding workspace.** It is
   rejected at Formula parse or load (Task 2, `TestExtensionManifestPathMustStayInLab`).
5. **Pruning while another Molecule prepares or uses an entry.** An entry referenced by a
   waiting or running Molecule is never deleted, and a concurrent build of the same key
   waits or reuses it (Task 3, `TestPruneKeepsReferencedEntries` and
   `TestConcurrentPrepareSameKey`).

---

### Task 0 (operator): Verify Claude analysis flags with an MCP server

Must finish before Task 4. No product code.

- [ ] **Step 1:** Write a 30-line stdio MCP test server (Python, standard library only) that
  exposes one tool, `ping`, returning `"pong"`. Pin it with `shasum -a 256`.
- [ ] **Step 2:** Run in a throwaway git repo:
  `printf 'Call the ping tool once and reply with its output.' | claude --print --output-format json --no-session-persistence --permission-mode dontAsk --model haiku --tools "" --safe-mode --strict-mcp-config --mcp-config '{"mcpServers":{"probe":{"command":"<abs>/server.py","args":[]}}}' --allowedTools mcp__probe__ping --no-chrome --disable-slash-commands`
- [ ] **Step 3:** Record whether `pong` came back.
  - If not, repeat with `--safe-mode` removed and `--setting-sources ""` added. If that
    is rejected, try `--setting-sources user`.
  - Record exactly which flag set lets the MCP tool run while built-in tools stay disabled
    (check `permission_denials` and that no Read/Bash tool was used).
- [ ] **Step 4:** Post the result as a comment on the Task 4 issue: the exact analysis flag
  set to use when extensions are present.

### Task 1: The `pinned` package and the `mcp-provider/v1` manifest

**Files:**
- Create: `lab/internal/pinned/pinned.go`, `lab/internal/pinned/pinned_test.go`
- Create: `lab/internal/extension/manifest.go`, `lab/internal/extension/manifest_test.go`
- Modify: `lab/internal/harness/analysis_wrapper.go`. Replace the private `checkPlacement`,
  `openPinned`, `sha256Path`, `verifyPinned`, `copyPinned` and `rejectDuplicateKeys`
  with calls into `pinned`. Behaviour must not change, and existing wrapper tests must
  pass unchanged.

**Interfaces:**
- Produces:
  ```go
  package pinned
  type File struct { Path string `json:"path"`; SHA256 string `json:"sha256"` }
  type Identity struct { Role, Path, SHA256 string }
  const MaxFileBytes = 256 << 20
  func CheckPlacement(path string) error
  func Open(path string) (*os.File, error)
  func Verify(role string, f File, unavailable string) (Identity, string) // "" means ok
  func Copy(f File, dst string) error
  func RejectDuplicateKeys(raw []byte) error

  package extension
  type Tool struct { Name string `json:"name"`; Access string `json:"access"` } // read-only|read-write
  type Command struct { Executable pinned.File `json:"executable"`; Interpreter *pinned.File `json:"interpreter,omitempty"`; Args []string `json:"args"` }
  type Prepare struct { Command; TimeoutMS int `json:"timeoutMs"`; Network string `json:"network"` } // none|declared
  type Dependency struct { pinned.File; Role string `json:"role"`; State string `json:"state"` } // state must be "pinned"
  type Manifest struct {
      SchemaVersion string `json:"schemaVersion"`; Kind string `json:"kind"`; Name string `json:"name"`
      Server Command `json:"server"`; Prepare *Prepare `json:"prepare,omitempty"`
      Tools []Tool `json:"tools"`; Roles []string `json:"roles"`
      Dependencies []Dependency `json:"dependencies"`; Limitations string `json:"limitations"`
  }
  func Decode(raw []byte) (Manifest, error)                       // strict, ≤64 KiB
  func (m Manifest) Verify() ([]pinned.Identity, error)            // pins every file now
  func Load(labDir, rel, sha256 string) (Manifest, []pinned.Identity, error)
  func (m Manifest) ToolsFor(role string) []Tool                   // analysis → read-only only
  ```

- [ ] **Step 1: Write failing manifest tests** in `extension/manifest_test.go`:
  ```go
  func TestDecodeRejectsInvalidManifests(t *testing.T) {
      good := validManifestJSON(t) // helper writes pinned fake files and returns JSON
      for name, mutate := range map[string]func(map[string]any){
          "unknown kind":        func(m map[string]any) { m["kind"] = "memory" },
          "unknown field":       func(m map[string]any) { m["extra"] = true },
          "bad tool name":       func(m map[string]any) { m["tools"] = []any{map[string]any{"name": "a b", "access": "read-only"}} },
          "bad access":          func(m map[string]any) { m["tools"] = []any{map[string]any{"name": "q", "access": "admin"}} },
          "too many tools":      func(m map[string]any) { m["tools"] = manyTools(33) },
          "unknown placeholder": func(m map[string]any) { m["server"].(map[string]any)["args"] = []any{"{{HOME}}"} },
          "pinned path in args": func(m map[string]any) { setArg(m, pinnedExecutablePath(m)) },
          "unknown role":        func(m map[string]any) { m["roles"] = []any{"deploy"} },
          "prepare timeout":     func(m map[string]any) { m["prepare"].(map[string]any)["timeoutMs"] = 1800001 },
          "unpinned dependency": func(m map[string]any) { m["dependencies"] = []any{map[string]any{"path": "/x", "role": "lib", "state": "unknown"}} },
      } {
          t.Run(name, func(t *testing.T) {
              raw := mutateJSON(t, good, mutate)
              if _, err := Decode(raw); err == nil { t.Fatalf("accepted %s", name) }
          })
      }
      if _, err := Decode(append(good, []byte(`{}`)...)); err == nil { t.Fatal("trailing data accepted") }
      if _, err := Decode(bytes.Repeat([]byte(" "), 65<<10)); err == nil { t.Fatal("oversize accepted") }
      if _, err := Decode([]byte(`{"name":"a","name":"b"}`)); err == nil { t.Fatal("duplicate key accepted") }
  }
  func TestVerifyRefusesUnsafeExecutables(t *testing.T) { /* symlink, 0o777 file, writable parent dir, digest mismatch: each returns an error naming the path */ }
  func TestToolsForAnalysisAreReadOnly(t *testing.T) {
      m := Manifest{Tools: []Tool{{"q", "read-only"}, {"w", "read-write"}}, Roles: []string{"analysis", "coding"}}
      if got := m.ToolsFor("analysis"); len(got) != 1 || got[0].Name != "q" { t.Fatalf("%v", got) }
      if got := m.ToolsFor("coding"); len(got) != 2 { t.Fatalf("%v", got) }
      if got := m.ToolsFor("meta"); len(got) != 0 { t.Fatalf("meta must get nothing: %v", got) }
  }
  ```
- [ ] **Step 2:** Run `cd lab && go test ./internal/extension/ -count=1`. Expected: FAIL, the
  package doesn't exist yet.
- [ ] **Step 3:** Create the `pinned` package by moving the helper bodies verbatim from
  `harness/analysis_wrapper.go`, exporting them as listed above. Change the wrapper to call
  `pinned.CheckPlacement`, `pinned.Open`, `pinned.Verify`, `pinned.Copy` and
  `pinned.RejectDuplicateKeys`. Move the helper unit tests to `pinned/pinned_test.go`.
- [ ] **Step 4:** Implement `extension.Decode`:
  - `len(raw) <= 64<<10`, then `pinned.RejectDuplicateKeys`, then a
    `json.Decoder` with `DisallowUnknownFields`;
  - require EOF after one value;
  - validate `schemaVersion == "v1"`, `kind == "mcp-provider"`, the name pattern, tools
    (pattern, access, at most 32, unique), roles drawn from
    `{"analysis","coding"}` (unique, non-empty), prepare timeout `1..1800000`, network
    `none|declared` and dependency `state == "pinned"`;
  - placeholder scan: every `{{` token must be exactly `{{CACHE}}` or `{{SOURCE}}`;
  - no arg may contain the path of any pinned file.
- [ ] **Step 5:** Implement `Verify`, which calls `pinned.Verify` for server, prepare,
  interpreters and dependencies. `Load` uses `filepath.Join(labDir, rel)`: `rel` must be
  clean, relative and not escape `labDir`, the file must pass `pinned.CheckPlacement`, and
  its bytes must hash to `sha256`; then `Decode` and `Verify`.
- [ ] **Step 6:** Run `cd lab && go test ./internal/pinned/ ./internal/extension/ ./internal/harness/ -count=1`.
  Expected: PASS, including the unchanged analysis-wrapper tests.
- [ ] **Step 7:** Commit `feat(extension): pinned package and mcp-provider/v1 manifest`.

### Task 2: Formula grammar for extensions

**Files:**
- Create: `lab/internal/formula/extensions.go`, `lab/internal/formula/extensions_test.go`
- Modify:
  - `lab/internal/formula/formula.go`: add a `Formula.Extensions` field and a
    `Binding.Extensions` field, and call the validation in `Parse`. Allow the
    `extensions` key in `validateBindingMapping` and `validateHarnessRoles`.
  - `lab/internal/harness/protocol.go`: add an `Extensions` field to `Capabilities`.
  - `lab/internal/onboarding/binding.go`: in `ValidateRoleBindings`, reject extensions on
    adapters whose capability is `unsupported`.
  - `lab/internal/learning/create.go`: reject a suggestion that adds, removes or changes the
    `manifest` or `sha256` of any `extensions` entry.

**Interfaces:**
- Consumes: none from Task 1. Parse is purely syntactic; manifests are loaded at run time
  in Task 5.
- Produces:
  ```go
  type ExtensionRef struct {
      Manifest string `yaml:"manifest" json:"manifest"`
      SHA256   string `yaml:"sha256" json:"sha256"`
      Required bool   `yaml:"required" json:"required,omitempty"`
  }
  // Formula: Extensions map[string]ExtensionRef `yaml:"extensions" json:"extensions,omitempty"`
  // Binding: Extensions []string `yaml:"extensions" json:"extensions,omitempty"`
  func (f Formula) RoleExtensions(role HarnessRole) []string // resolved binding's list, declared order
  // harness.Capabilities gains: Extensions Capability `json:"extensions"`
  //   claude-code: supported; codex, cursor: unsupported ("extensions are not wired for this adapter in v1")
  ```

- [ ] **Step 1: Write failing tests:**
  ```go
  func TestFormulaWithoutExtensionsIsUnchanged(t *testing.T) {
      f, err := Parse(baseRecipe()) // existing helper recipe with no extensions
      if err != nil || len(f.Extensions) != 0 || f.Harness.Extensions != nil { t.Fatalf("%+v %v", f, err) }
      raw, _ := json.Marshal(f.Harness)
      if bytes.Contains(raw, []byte("extensions")) { t.Fatal("binding JSON changed for an extension-free Formula") }
  }
  func TestExtensionDeclarationsValidate(t *testing.T) {
      for name, recipe := range map[string]string{
          "bad name":            withExtensions("Bad_Name: {manifest: e/x.json, sha256: " + hex64 + "}"),
          "bad digest":          withExtensions("g: {manifest: e/x.json, sha256: abc}"),
          "unknown reference":   withBindingExtensions("[missing]"),
          "duplicate reference": withExtensions("g: {manifest: e/x.json, sha256: " + hex64 + "}", "[g, g]"),
      } { t.Run(name, func(t *testing.T) { if _, err := Parse([]byte(recipe)); err == nil { t.Fatal("accepted") } }) }
  }
  func TestExtensionManifestPathMustStayInLab(t *testing.T) {
      for _, p := range []string{"../x.json", "/abs/x.json", "e/../../x.json", "workspaces/issue-1/x.json"} {
          if _, err := Parse([]byte(withExtensions("g: {manifest: " + p + ", sha256: " + hex64 + "}"))); err == nil {
              t.Fatalf("manifest path %q accepted", p)
          }
      }
  }
  func TestRoleExtensionsResolve(t *testing.T) { /* default harness list applies to coding; harnesses.analysis list applies to analysis; meta gets none */ }
  ```
  Also add to `onboarding/binding_test.go` a test that a Codex or Cursor role with
  extensions is rejected with the adapter reason. Add to `learning/create_test.go` a test
  that a suggestion changing `extensions.g.sha256` is refused.
- [ ] **Step 2:** Run `cd lab && go test ./internal/formula/ ./internal/onboarding/ ./internal/learning/ -count=1`.
  Expected: FAIL.
- [ ] **Step 3:** Implement the validation in `extensions.go`:
  - name pattern;
  - `manifest` is clean, relative, has no `..` segments and is not under `workspaces/`,
    `verification/` or `ledgers/`;
  - `sha256` is 64 lowercase hex characters;
  - each binding's list holds only declared, unique names.

  Wire it into `Parse`, then add `RoleExtensions`, the `Capabilities.Extensions` values,
  the onboarding rejection and the learning guard.
- [ ] **Step 4:** Run the same tests. Expected: PASS. Then run `go test ./... -count=1` and
  `go vet ./...`; the existing protocol and contract tests must still pass (BuildInvocation
  is unchanged so far).
- [ ] **Step 5:** Commit `feat(formula): declare extensions and bind them to roles`.

### Task 3: Prepare and cache

**Files:**
- Create: `lab/internal/extension/prepare.go`, `lab/internal/extension/prepare_test.go`

**Interfaces:**
- Consumes: Task 1 (`Manifest`, `pinned.Copy`).
- Produces:
  ```go
  type CacheOutcome string // "built" | "reused" | "unavailable"
  type CacheResult struct {
      Key, Dir string; Outcome CacheOutcome; TreeSHA256, Reason string
      Code int; ElapsedMS int64; TimedOut bool
  }
  const MaxCacheBytes = 1 << 30
  func CacheKey(name, manifestSHA, baseCommit string) string
  func Prepare(ctx context.Context, labDir, name, manifestSHA string, m Manifest, sourceRepo, baseCommit string) CacheResult
  func Prune(labDir, name string, keep int, referenced map[string]bool) ([]string, error) // returns removed keys
  ```

- [ ] **Step 1: Write failing tests**, using a fake prepare script. The script writes
  `{{CACHE}}/graph.json` and records its argv and env to a file outside the cache.
  ```go
  func TestPrepareBuildsOnceThenReuses(t *testing.T)
  func TestPrepareSnapshotIsReadOnlyWithoutGit(t *testing.T)        // script tries to write {{SOURCE}}/x and stat {{SOURCE}}/.git; both must fail
  func TestPrepareEnvironmentIsMinimal(t *testing.T)                // ambient GH_TOKEN/PYTHONPATH absent; HOME and TMPDIR are private
  func TestPrepareTimeoutLeavesNoPartialEntry(t *testing.T)
  func TestPrepareOversizeOutputIsUnavailable(t *testing.T)          // script writes 1 GiB + 1 byte (sparse file); outcome unavailable, no entry
  func TestPrepareMissingBaseCommitIsUnavailable(t *testing.T)       // baseCommit not in repo; outcome unavailable; reason names the commit
  func TestConcurrentPrepareSameKey(t *testing.T)                     // two goroutines: exactly one build (script counts runs), both see the same TreeSHA256
  func TestCompletedEntryIsReadOnly(t *testing.T)                     // dirs 0500, files 0400
  func TestTamperedEntryIsRebuilt(t *testing.T)                       // change a byte after completion: next Prepare reports built, not reused
  func TestPruneKeepsReferencedEntries(t *testing.T)                  // keep=1, three entries, one referenced: removes only the unreferenced older one
  func TestPrepareWithoutPrepareCommandUsesEmptyCache(t *testing.T)   // manifest with no prepare: outcome built, empty dir
  ```
- [ ] **Step 2:** Run `cd lab && go test ./internal/extension/ -run Prepare\|Prune\|Concurrent\|Completed\|Tampered -count=1`.
  Expected: FAIL.
- [ ] **Step 3:** Implement `Prepare`:
  1. Compute the key and dir. If `<dir>/.complete` exists and its recorded tree digest
     matches the actual tree, return `reused`.
  2. Take the lock `mkdir <dir>.lock` (owner PID file, the same convention as
     `ledgers/<id>.lock`). If it is held, poll every 250 ms until the lock is released
     or the context ends. If the entry is then complete, return `reused`.
  3. Snapshot:
     `git -C sourceRepo -c core.fsmonitor=false -c core.hooksPath=/dev/null archive --format=tar <base>`,
     piped into an extraction in `MkdirTemp`. Then chmod the result read-only. A git
     failure returns `unavailable` with the reason.
  4. Copy the prepare executable and interpreter with `pinned.Copy` into a private 0700
     dir.
  5. Substitute `{{CACHE}}` (the `.partial-<nonce>` dir) and `{{SOURCE}}`.
  6. Run with `process.Run`, using `Env` = `PATH=/usr/bin:/bin`, `HOME=<tmp>`,
     `TMPDIR=<tmp>` and `LANG=C.UTF-8`, cwd = a private temp dir and the manifest
     timeout.
  7. Walk the output. If it exceeds `MaxCacheBytes`, return `unavailable`.
  8. Compute the tree digest (sorted relative paths plus file SHA-256s), write
     `.complete` with the digest, chmod read-only and rename `.partial` to `dir`. Remove
     every temp dir on all paths.
- [ ] **Step 4:** Implement `Prune`. List the key directories, sort them by `.complete`
  mtime (newest first) and remove entries beyond `keep` that aren't in `referenced`. Make
  each removed entry writable before deleting it.
- [ ] **Step 5:** Run the tests. Expected: PASS. Then run `go vet ./...` and gofmt.
- [ ] **Step 6:** Commit `feat(extension): prepare and cache indexes outside the checkout`.

### Task 4: Claude wiring and adapter protocol 2

Depends on the Task 0 result, which names the analysis flag set.

**Files:**
- Modify: `lab/internal/harness/protocol.go`, `lab/internal/harness/protocol_contract_test.go`
- Test: `lab/internal/harness/extensions_invocation_test.go` (create)

**Interfaces:**
- Consumes: none at the type level. Requests carry plain data.
- Produces:
  ```go
  // request["mcpServers"] = []MCPServer (sorted by Name by the caller; BuildInvocation re-sorts)
  type MCPServer struct { Name, Command string; Args []string; Tools []string }
  const AdapterProtocolVersion = "2"
  ```
  BuildInvocation rules for `claude-code`:
  - No `mcpServers`: output is byte-identical to today, except the protocol digest.
  - **Coding:** `--mcp-config` holds `{"mcpServers":{<name>:{"command":..,"args":[..],"env":{}}}}`,
    marshalled with sorted keys. `--allowedTools` gets `mcp__<name>__<tool>` for each tool,
    appended after the derived Bash allowlist.
  - **Analysis:** the same config, with only the tools the caller passed (already filtered
    to read-only). The flag set comes from Task 0. `--tools ""`, `--strict-mcp-config`,
    `--no-chrome` and `--disable-slash-commands` always stay.
  - `codex` or `cursor` with a non-empty `mcpServers`: an error,
    `extensions are not supported for <id> in v1`.

- [ ] **Step 1: Write failing tests:**
  ```go
  func TestCodingInvocationAddsPinnedMCPServers(t *testing.T) {
      req := map[string]any{"kind": "molecule", "codingAllowlist": baseAllowlist(),
          "mcpServers": []MCPServer{{Name: "graphify", Command: "/priv/graphify", Args: []string{"mcp", "--graph", "/cache/graph.json"}, Tools: []string{"query_graph"}}}}
      call, err := BuildInvocation("claude-code", req, "/fixture/result")
      if err != nil { t.Fatal(err) }
      if got := argAfter(call.Args, "--mcp-config"); got != `{"mcpServers":{"graphify":{"args":["mcp","--graph","/cache/graph.json"],"command":"/priv/graphify","env":{}}}}` { t.Fatalf("%s", got) }
      if !strings.HasSuffix(argAfter(call.Args, "--allowedTools"), ",mcp__graphify__query_graph") { t.Fatal("tool not allowlisted") }
      if !contains(call.Args, "--strict-mcp-config") { t.Fatal("strict MCP dropped") }
  }
  func TestAnalysisInvocationKeepsToolsDisabled(t *testing.T) { /* --tools "" present; only passed tools in --allowedTools; Task 0 flag set asserted */ }
  func TestInvocationWithoutExtensionsIsUnchanged(t *testing.T) { /* compare against testdata/contracts.json entries */ }
  func TestOtherAdaptersRejectExtensions(t *testing.T) { /* codex and cursor return the v1 error */ }
  ```
- [ ] **Step 2:** Run `cd lab && go test ./internal/harness/ -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement it in `protocol.go`, keeping the server and tool order
  deterministic. Validate again inside `BuildInvocation` with the Task 1 patterns: name
  pattern, tool pattern, no empty command. Set `AdapterProtocolVersion = "2"`.
- [ ] **Step 4:** In `protocol_contract_test.go`:
  - add a coding request and an analysis request that each carry one fixed `MCPServer`
    to `adapterContractDigest`;
  - replace the map with
    `{"2": "<digest printed by the failing test>"}`, removing `"1"`;
  - run once to capture the digest, pin it, and run again.
- [ ] **Step 5:** Run `go test ./... -count=1`. Expected: PASS. Tests that assert
  `AdapterProtocolVersion` values in manifests and identity (#37/#55) use the constant,
  so update any literal `"1"` in fixtures to the constant.
- [ ] **Step 6:** Commit `feat(harness): wire pinned MCP providers into Claude invocations (protocol 2)`.

### Task 5: Molecule integration, ledger, gate and doctor

**Files:**
- Create: `lab/internal/molecule/extensions.go`, `lab/internal/molecule/extensions_test.go`
- Modify:
  - `lab/internal/molecule/run.go`: add the `Record.Extensions` field.
  - `lab/internal/molecule/readiness_run.go`: prepare before analysis
    (around the `invokeHarness(ctx, b.Command, w.SourceRoot, …)` call), and reuse before
    coding (around the `invokeHarness(ctx, codingBinding.Command, …)` call).
  - `lab/internal/molecule/gate_policy.go`: add active extension names to the decision
    evidence.
  - `lab/internal/molecule/store.go`: render extensions in the ledger.
  - `lab/internal/cli/cli.go`: show extensions in `presentRun` and the doctor section.

**Interfaces:**
- Consumes: Task 1 `extension.Load` and `Manifest.ToolsFor`; Task 2 `Formula.Extensions`
  and `RoleExtensions`; Task 3 `extension.Prepare`; Task 4 `harness.MCPServer`.
- Produces:
  ```go
  type ExtensionRecord struct {
      Name, Kind, ManifestSHA256 string; Roles []string; Tools []string
      Cache extension.CacheResult `json:"cache"`; Required bool `json:"required,omitempty"`
  }
  // Record: Extensions []ExtensionRecord `json:"extensions,omitempty"`
  func useExtensions(ctx context.Context, r *Record, f formula.Formula, role formula.HarnessRole, privateDir string) ([]harness.MCPServer, error)
  // error only when a required extension is unavailable; otherwise unavailable extensions are omitted from servers and recorded
  ```

- [ ] **Step 1: Write failing tests** with a fake `claude` that writes its argv, plus a fake
  prepare and server script:
  ```go
  func TestAnalysisAndCodingReceiveDeclaredExtensions(t *testing.T)   // argv contains generated --mcp-config for both roles; one build, coding reuses
  func TestUnavailableOptionalExtensionDoesNotBlock(t *testing.T)      // prepare exits 1: intake proceeds, record shows unavailable+reason, argv has empty mcpServers
  func TestUnavailableRequiredExtensionStopsIntake(t *testing.T)       // required:true: intake stops with an actionable message, no harness started
  func TestRoleCompletesWhenExtensionServerDies(t *testing.T)          // server script exits immediately; fake claude still returns a valid outcome; record shows available
  func TestExtensionsLeaveTheWorkspaceClean(t *testing.T)              // capture after coding shows only the fake harness's own edit
  func TestHistoricalLedgerWithoutExtensionsVerifies(t *testing.T)     // existing fixture ledger decodes and validates unchanged
  func TestScopeGateShowsExtensions(t *testing.T)                       // presentRun output names extension, tools, cache outcome, network claim
  func TestDoctorReportsExtensionProblems(t *testing.T)                 // digest mismatch → one fix command; unsupported adapter binding reported
  ```
- [ ] **Step 2:** Run `cd lab && go test ./internal/molecule/ ./internal/cli/ -run Extension\|ScopeGateShowsExtensions\|DoctorReportsExtension -count=1`.
  Expected: FAIL.
- [ ] **Step 3:** Implement `useExtensions`. For each name in `f.RoleExtensions(role)`:
  1. Run `extension.Load` with the Lab dir and the `ExtensionRef`. On failure, the cache
     result is `unavailable` with the reason.
  2. Run `extension.Prepare` at `p.Inputs.BaseCommit` against the source repository (it
     reuses a cached entry).
  3. Use `pinned.Copy` to place the server executable into `privateDir`, and substitute
     `{{CACHE}}` with the entry dir.
  4. Build an `MCPServer` with `Tools` taken from `m.ToolsFor(string(role))`.
  5. Upsert an `ExtensionRecord` into `r.Extensions`, keyed by name, and append the role.
  6. If an extension is `Required` and unavailable, return an error.

  Call it before analysis and before coding. Put the servers into the request as
  `"mcpServers"`. Remove `privateDir` after the harness returns.
- [ ] **Step 4:** Render the extensions:
  - in `presentRun`, after the allowlist block:
    `Extension <name> (<roles>): <outcome>; tools: a, b; network: <claim>`;
  - in the ledger Markdown, a short section;
  - in doctor, an `extensions` section listing load/verify status and adapter support per
    binding (doctor never runs prepare or the server);
  - in `gate_policy.go` evidence: `{Pointer: "/extensions", Observed: "<n> extensions: names"}`.
- [ ] **Step 5:** Run `go test ./... -count=1`, `go vet ./...`, gofmt and both hygiene
  scripts. Expected: PASS.
- [ ] **Step 6:** Commit `feat(molecule): prepare, attach and record extensions per role`.

### Task 6: Documentation and an example Graphify manifest

**Files:**
- Create: `docs/examples/extensions/graphify.json` (example placeholders for paths and
  digests, plus a comment-free JSON copy in `docs/examples/extensions/README.md`)
- Modify: `lab/README.md` (an "Extensions" section) and `docs/atoms.md` (extensions are
  hints, never gate inputs)

- [ ] **Step 1:** Write the docs:
  - the declaration (Formula plus manifest);
  - the trust model (pinned, copied before exec, read-only analysis, declared network);
  - the cache location and pruning;
  - the v1 limits (Claude only, no per-call logging);
  - how to compute digests (`shasum -a 256`).
- [ ] **Step 2:** Add a test in `lab/internal/extension/manifest_test.go`:
  `TestExampleManifestDecodes`. It loads `docs/examples/extensions/graphify.json` with its
  placeholder paths swapped for temp pinned files, and asserts `Decode` passes.
- [ ] **Step 3:** Run the baseline checks and commit
  `docs: extensions guide and example Graphify manifest`.

### Task 7 (operator): Dogfood in the v3 Lab

- [ ] **Step 1:** Rebuild with `forgecell dev install --from ~/Code/forgecell-core`, then run
  `init --rebind claude-code` on v3 (protocol 2), and approve it.
- [ ] **Step 2:** Install Graphify (asking the operator before downloading it). Run
  `graphify --help` to confirm the real subcommand names for build and MCP, and adjust the
  manifest's args to match. Pin the digests and place the manifest at
  `~/.forgecell/labs/forgecell-claude-v3/extensions/graphify.json`.
- [ ] **Step 3:** Propose the Formula change that attaches `graphify` to analysis and coding,
  and approve it. This is an operator gate.
- [ ] **Step 4:** Run the next roughly 5 Molecules. From the ledgers, compare against the
  v3 baseline: evidence requests per intake, evidence-cap rejections, `scope-change`
  outcomes caused by missing files, coding elapsed time and timeouts. Post the result on
  the tracking issue.
