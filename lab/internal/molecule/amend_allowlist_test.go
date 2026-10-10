package molecule

import (
	"context"
	"reflect"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

// The coding allowlist is runtime-owned: an amendment payload can neither
// smuggle extra entries nor drop the derived list.
func TestAmendDerivesCodingAllowlist(t *testing.T) {
	for name, submitted := range map[string][]string{
		"smuggled": {"Read", "Bash(go run ./evil)"},
		"dropped":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			o, _ := fixture(t)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			next := copyPlan(t, r.Readiness.Plans[0].Plan)
			next.CodingAllowlist = submitted
			r, err = Amend(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, next, "")
			if err != nil {
				t.Fatal(err)
			}
			amended := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
			want, err := harness.DeriveCodingAllowlist(r.FormulaSnapshot.YAML, amended.Analysis.Checks)
			if err != nil {
				t.Fatal(err)
			}
			if len(amended.CodingAllowlist) == 0 || !reflect.DeepEqual(amended.CodingAllowlist, want) {
				t.Fatalf("amended allowlist %q, want derived %q", amended.CodingAllowlist, want)
			}
		})
	}
}
