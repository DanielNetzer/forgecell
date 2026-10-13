package readiness

import (
	"errors"
	"strings"
	"testing"
)

func TestBaselineIDPolicyIsARetryableViolationOfItsOwnClass(t *testing.T) {
	p := fixture()
	a := p.Analysis
	a.Checks[0].ID = "baseline-go-test"
	err := ValidateBaselineIDs(a, []string{"baseline-go-test", "baseline-go-vet"})
	var violation *PolicyViolation
	if !errors.As(err, &violation) || !IsPolicyViolation(err) {
		t.Fatalf("a baseline id collision must be a policy violation: %v", err)
	}
	if !strings.HasPrefix(err.Error(), BaselineIDMessage+": baseline-go-test") {
		t.Fatalf("message %q", err.Error())
	}
	if class := PolicyClass(violation); class != PolicyClassBaselineID {
		t.Fatalf("class %q, want %q", class, PolicyClassBaselineID)
	}
	for _, other := range []string{PolicyClassLabelling, PolicyClassInScopeDefinition, PolicyClassEvidenceRequestCap, PolicyClassDefinitionRef, PolicyClassOther} {
		if other == PolicyClassBaselineID {
			t.Fatalf("the baseline class must be distinct from %q", other)
		}
	}
}

func TestBaselineIDPolicyAcceptsDistinctIDs(t *testing.T) {
	a := fixture().Analysis
	if err := ValidateBaselineIDs(a, []string{"baseline-go-test"}); err != nil {
		t.Fatalf("distinct ids must pass: %v", err)
	}
	if err := ValidateBaselineIDs(a, nil); err != nil {
		t.Fatalf("no baseline must pass: %v", err)
	}
}

func TestBaselineIDPolicyNamesEachCollisionOnce(t *testing.T) {
	a := fixture().Analysis
	second := a.Checks[0]
	a.Checks = append(a.Checks, second)
	err := ValidateBaselineIDs(a, []string{"regression"})
	if err == nil || strings.Count(err.Error(), "regression") != 1+strings.Count(BaselineIDMessage, "regression") {
		t.Fatalf("the colliding id must be named once: %v", err)
	}
}
