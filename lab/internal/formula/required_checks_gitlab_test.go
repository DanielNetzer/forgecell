package formula

import "testing"

func TestRequiredChecksValidateAcceptsOnlyTheRootGitLabPipeline(t *testing.T) {
	good := RequiredCheck{ID: "baseline-go-test", Argv: []string{"go", "test", "./..."}, Dir: ".", TimeoutMS: 1000, Workflow: ".gitlab-ci.yml", Reason: "GitLab CI"}
	if err := good.Validate(); err != nil {
		t.Fatalf("root .gitlab-ci.yml refused: %v", err)
	}
	for _, workflow := range []string{"lab/.gitlab-ci.yml", "x.gitlab-ci.yml", "../.gitlab-ci.yml", "/.gitlab-ci.yml", "./.gitlab-ci.yml", ".gitlab-ci.yaml", ".gitlab-ci.yml/", ".GITLAB-CI.YML", ".gitlab-ci.yml\x00", ".github/workflows/.gitlab-ci.yml/../x", ""} {
		c := good
		c.Workflow = workflow
		if err := c.Validate(); err == nil {
			t.Fatalf("workflow %q accepted", workflow)
		}
	}
	for _, workflow := range []string{".github/workflows/ci.yml", ".github/workflows/ci.yaml"} {
		c := good
		c.Workflow = workflow
		if err := c.Validate(); err != nil {
			t.Fatalf("workflow %q refused: %v", workflow, err)
		}
	}
}

func TestRequiredChecksParseAcceptsGitLabWorkflow(t *testing.T) {
	list := []any{map[string]any{"id": "baseline-go-vet", "argv": []any{"go", "vet", "./..."}, "dir": ".", "timeoutMs": 1000, "workflow": ".gitlab-ci.yml", "reason": "GitLab CI"}}
	if err := validateRequiredChecks(list); err != nil {
		t.Fatal(err)
	}
	list[0].(map[string]any)["workflow"] = "lab/.gitlab-ci.yml"
	if err := validateRequiredChecks(list); err == nil {
		t.Fatal("nested .gitlab-ci.yml accepted")
	}
}
