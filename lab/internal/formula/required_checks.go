package formula

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// MaxRequiredChecks bounds the reviewed baseline so a plan keeps room for its own checks.
const MaxRequiredChecks = 16

// RequiredCheck is one reviewed baseline command, derived from a CI workflow, that
// every plan for this Formula must include. Workflow names the frozen definition.
type RequiredCheck struct {
	ID        string   `yaml:"id" json:"id"`
	Argv      []string `yaml:"argv" json:"argv"`
	Dir       string   `yaml:"dir" json:"dir"`
	TimeoutMS int      `yaml:"timeoutMs" json:"timeoutMs"`
	Workflow  string   `yaml:"workflow" json:"workflow"`
	Reason    string   `yaml:"reason" json:"reason"`
	// Select narrows a direct `go test ... ./...` baseline check to the affected
	// packages (readiness.SelectAffectedPackages). Absent keeps the Formula bytes, SHA
	// and the full suite unchanged; omitempty keeps bootstrapped Formulas from
	// writing an empty select.
	Select string `yaml:"select,omitempty" json:"select,omitempty"`
}

var (
	requiredCheckID      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	requiredCheckSegment = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9._-]*$`)
)

// Validate applies the rules Parse enforces on a requiredChecks entry.
func (c RequiredCheck) Validate() error {
	if !requiredCheckID.MatchString(c.ID) {
		return fmt.Errorf("id must be an identifier of at most 128 characters")
	}
	if len(c.Argv) == 0 || len(c.Argv) > 64 {
		return fmt.Errorf("argv must be a non-empty array of at most 64 strings")
	}
	for _, arg := range c.Argv {
		if strings.TrimSpace(arg) == "" || len(arg) > 2000 || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("argv must contain non-empty bounded strings")
		}
	}
	if strings.ContainsAny(c.Argv[0], " \t\r\n") {
		return fmt.Errorf("argv[0] must be a command, not a shell string")
	}
	if c.Dir != "." {
		if path.Clean(c.Dir) != c.Dir || len(c.Dir) > 512 {
			return fmt.Errorf("dir must be a clean repository-relative path")
		}
		for _, segment := range strings.Split(c.Dir, "/") {
			if segment == "." || segment == ".." || strings.EqualFold(segment, ".git") || !requiredCheckSegment.MatchString(segment) {
				return fmt.Errorf("dir must be a clean repository-relative path")
			}
		}
	}
	if c.TimeoutMS < 1 || c.TimeoutMS > maxFormulaTimeoutMS {
		return fmt.Errorf("timeoutMs must be an integer from 1 to %d", maxFormulaTimeoutMS)
	}
	if len(c.Workflow) > 512 || path.Clean(c.Workflow) != c.Workflow || strings.ContainsAny(c.Workflow, "\\\x00") || !strings.HasPrefix(c.Workflow, ".github/workflows/") || !(strings.HasSuffix(c.Workflow, ".yml") || strings.HasSuffix(c.Workflow, ".yaml")) {
		return fmt.Errorf("workflow must be a clean path under .github/workflows/")
	}
	if strings.TrimSpace(c.Reason) == "" || len(c.Reason) > 2000 || strings.ContainsRune(c.Reason, 0) {
		return fmt.Errorf("reason must be non-empty bounded text")
	}
	// A baseline check is a regression check, which is what the plan rule checks.
	if err := readiness.ValidateCheckSelect(readiness.Check{ID: c.ID, Category: "regression", Argv: c.Argv, Select: c.Select}); err != nil {
		return err
	}
	return nil
}

var requiredCheckKeys = map[string]bool{"id": true, "argv": true, "dir": true, "timeoutMs": true, "workflow": true, "reason": true, "select": true}

func validateRequiredChecks(value any) error {
	list, ok := value.([]any)
	if !ok {
		return fmt.Errorf("requiredChecks must be a list of baseline checks")
	}
	if len(list) > MaxRequiredChecks {
		return fmt.Errorf("requiredChecks allows at most %d baseline checks", MaxRequiredChecks)
	}
	seen := map[string]bool{}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("requiredChecks[%d] must be a mapping", i)
		}
		for key := range m {
			if !requiredCheckKeys[key] {
				return fmt.Errorf("requiredChecks[%d]: unknown key %q", i, key)
			}
		}
		var c RequiredCheck
		c.ID, _ = m["id"].(string)
		c.Dir, _ = m["dir"].(string)
		c.Workflow, _ = m["workflow"].(string)
		c.Reason, _ = m["reason"].(string)
		c.TimeoutMS, _ = m["timeoutMs"].(int)
		// An explicit empty or null select means the same as an absent one: the full
		// suite. Any other value is checked by Validate.
		if value, ok := m["select"]; ok && value != nil {
			if c.Select, ok = value.(string); !ok {
				return fmt.Errorf("requiredChecks[%d]: select must be %q", i, readiness.SelectAffectedPackages)
			}
		}
		args, _ := m["argv"].([]any)
		for _, arg := range args {
			s, ok := arg.(string)
			if !ok {
				return fmt.Errorf("requiredChecks[%d]: argv must contain strings", i)
			}
			c.Argv = append(c.Argv, s)
		}
		if err := c.Validate(); err != nil {
			return fmt.Errorf("requiredChecks[%d]: %w", i, err)
		}
		if seen[c.ID] {
			return fmt.Errorf("requiredChecks[%d]: duplicate id %q", i, c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}
