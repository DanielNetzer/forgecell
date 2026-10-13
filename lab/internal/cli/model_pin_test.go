package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

const modelPinFormula = "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: acme/widgets}\nharness:\n  binding: claude-code\n  command: [\"/forgecell\", \"__adapter\", \"claude-code\", \"/claude\"]\natoms:\n  - {id: intake, type: intake}\n  - {id: harness, type: harness}\n"

func TestModelPinEndToEnd(t *testing.T) {
	const pin = "claude-opus-4-8"
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gsWrite(t, filepath.Join(repo, "code.txt"), "base", 0600)
	gsEnv(t, "\"repo view\"*) printf '%s' '{\"nameWithOwner\":\"acme/widgets\",\"defaultBranchRef\":{\"name\":\"main\"}}'; exit 0;;\n")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}, {"add", "."}, {"commit", "-qm", "base"}, {"remote", "add", "origin", "https://github.com/acme/widgets.git"}} {
		gsGit(t, repo, args...)
	}
	t.Chdir(repo)
	lab := filepath.Join(root, "lab")
	gsApproveFormula(t, lab, modelPinFormula)
	pinned := strings.Replace(modelPinFormula, "\"/claude\"]\n", "\"/claude\"]\n  model: "+pin+"\n", 1)

	code, out, errOut := gsRun([]string{"init", "--lab", lab, "--model", pin})
	if code != 0 {
		t.Fatalf("init --model exit %d: %s %s", code, out, errOut)
	}
	for _, want := range []string{"only `model`", "atoms, checks, instructions, timeouts", "fresh intake", "Exact YAML before proposed change:\n" + modelPinFormula, "Exact Formula YAML:\n" + pinned} {
		if !strings.Contains(out, want) {
			t.Fatalf("init --model output omits %q:\n%s", want, out)
		}
	}
	id := regexp.MustCompile(`assay-[a-f0-9]{64}`).FindString(out)
	if id == "" {
		t.Fatalf("no proposal id:\n%s", out)
	}

	code, review, errOut := gsRun([]string{"init", "--lab", lab, "--review", id, "--json"})
	var reviewed struct {
		Proposal   struct{ YAML string } `json:"proposal"`
		BeforeYAML string                `json:"beforeYaml"`
	}
	if code != 0 || json.Unmarshal([]byte(review), &reviewed) != nil || reviewed.Proposal.YAML != pinned || reviewed.BeforeYAML != modelPinFormula {
		t.Fatalf("init --review exit %d: %s %s", code, review, errOut)
	}
	if code, review, errOut = gsRun([]string{"init", "--lab", lab, "--review", id}); code != 0 || !strings.Contains(review, "Exact YAML before proposed change:\n"+modelPinFormula) {
		t.Fatalf("init --review (human) exit %d: %s %s", code, review, errOut)
	}
	if loaded, err := formula.Load(lab, ""); err != nil || loaded.Formula.Harness.Model != "" {
		t.Fatalf("review activated the proposal: %v", err)
	}

	if code, out, errOut = gsRun([]string{"init", "--lab", lab, "--approve", id}); code != 0 || !strings.Contains(out, "approved") {
		t.Fatalf("init --approve exit %d: %s %s", code, out, errOut)
	}
	loaded, err := formula.Load(lab, "")
	if err != nil || !loaded.Approved || loaded.Formula.Harness.Model != pin || loaded.Formula.YAML != pinned {
		t.Fatalf("approval did not activate the pin: %v %+v", err, loaded)
	}

	code, doctor, errOut := gsRun([]string{"doctor", "--lab", lab})
	if code == 1 {
		t.Fatalf("doctor errored: %s", errOut)
	}
	roles := rolesByName(t, decodeDoctor(t, doctor))
	for _, role := range []string{"analysis", "coding"} {
		if roles[role]["model"] != pin {
			t.Fatalf("doctor %s: %v", role, roles[role])
		}
	}
	if code, doctor, errOut = gsRun([]string{"doctor", "--lab", lab, "--verbose"}); code == 1 || !strings.Contains(doctor, "(model: "+pin+")") {
		t.Fatalf("doctor --verbose exit %d: %s %s", code, doctor, errOut)
	}

	code, removal, errOut := gsRun([]string{"init", "--lab", lab, "--model", "", "--json"})
	var removed struct {
		Status     string                `json:"status"`
		BeforeYAML string                `json:"beforeYaml"`
		Note       string                `json:"note"`
		Proposal   struct{ YAML string } `json:"proposal"`
	}
	if code != 0 || json.Unmarshal([]byte(removal), &removed) != nil || removed.Status != "pending" || removed.BeforeYAML != pinned || removed.Proposal.YAML != modelPinFormula || !strings.Contains(removed.Note, "removing the model pin from harness") {
		t.Fatalf("init --model \"\" exit %d: %s %s", code, removal, errOut)
	}
}

func TestModelPinFlagConflictsWriteNothing(t *testing.T) {
	lab := t.TempDir()
	cases := map[string][]string{
		"harness":       {"--model", "m", "--harness", "claude-code"},
		"rebind":        {"--model", "m", "--rebind", "codex"},
		"meta harness":  {"--model", "m", "--meta-harness", "codex"},
		"meta command":  {"--model", "m", "--meta-command", `["/bin/x"]`},
		"timeout":       {"--model", "m", "--harness-timeout-ms", "1000"},
		"max timeout":   {"--model", "m", "--max-timeout-ms", "1000"},
		"review":        {"--model", "m", "--review", "assay-x"},
		"approve":       {"--model", "m", "--approve", "assay-x"},
		"dismiss":       {"--model", "m", "--dismiss", "assay-x"},
		"empty removal": {"--model", "", "--approve", "assay-x"},
		"role alone":    {"--role", "coding"},
		"unknown role":  {"--model", "m", "--role", "ship"},
		"invalid model": {"--model", "--bad"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := append([]string{"init", "--lab", lab}, extra...)
			if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev"); code != 1 || !strings.Contains(stderr.String(), "--") {
				t.Fatalf("exit %d stderr %q", code, stderr.String())
			}
			if entries, _ := os.ReadDir(lab); len(entries) != 0 {
				t.Fatalf("a refused --model wrote to the Lab: %v", entries)
			}
		})
	}
}

func TestModelPinIsDocumentedInHelp(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"--model ID", "--role analysis|coding|meta|review"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help omits %q: %s", want, out.String())
		}
	}
}

func TestModelPinDoctorShowsEachRolesModel(t *testing.T) {
	bin, lab := doctorFixture(t)
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codex := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	claude := writeTool(t, bin, "claude", strings.NewReplacer("LOGGED", "true").Replace(claudeTool))
	saveRoleFormula(t, lab, argv(t, launcher, "__adapter", "codex", codex), "harnesses:\n  analysis: {binding: claude-code, command: "+argv(t, launcher, "__adapter", "claude-code", claude)+", model: model-fixture}\n")
	_, out := runDoctor(t, lab)
	roles := rolesByName(t, decodeDoctor(t, out))
	if roles["analysis"]["model"] != "model-fixture" {
		t.Fatalf("analysis: %v", roles["analysis"])
	}
	if _, present := roles["coding"]["model"]; present {
		t.Fatalf("an unpinned role carries a model key: %v", roles["coding"])
	}
	_, human := runDoctor(t, lab, "--verbose")
	for _, want := range []string{"analysis: claude-code (override binding) — ", "(model: model-fixture)", "coding: codex (default binding) — ready", "(model: unpinned)"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human doctor output lacks %q:\n%s", want, human)
		}
	}
}
