package delivery

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

func validateReadiness(ctx context.Context, o Options, r molecule.Record, expectedHead string) error {
	if err := molecule.ValidateVerificationHistory(r); err != nil {
		return err
	}
	if r.Readiness == nil || r.Verification == nil {
		return fmt.Errorf("legacy Molecule has no readiness/verification evidence; historical inspection remains available, delivery requires a new reviewed run")
	}
	p, err := r.Readiness.ValidateForDelivery()
	if err != nil {
		return err
	}
	if p.MoleculeID != r.ID || p.Inputs.Repository != r.Issue.Repo || p.Inputs.Issue.Number != r.Issue.Number || p.Inputs.FormulaSHA256 != r.FormulaSnapshot.SHA256 || p.Inputs.BaseCommit != r.Workspace.BaseCommit || p.Inputs.TargetBranch != o.BaseBranch {
		return fmt.Errorf("delivery does not match the approved readiness identity")
	}
	v := r.Verification
	if !v.RequiredChecksPassed || v.SourceTree == "" || v.Error != "" {
		return fmt.Errorf("required verification has not passed")
	}
	// Recompute completeness from observations instead of trusting an aggregate flag.
	for _, check := range p.Analysis.Checks {
		if !check.Required {
			continue
		}
		found := false
		for _, obs := range v.Checks {
			if obs.ID == check.ID && reflect.DeepEqual(obs.Command, check) && obs.Category == check.Category && obs.SourceTree == v.SourceTree && obs.Result.OK && !obs.Result.TimedOut && !obs.Result.Interrupted && !obs.Result.Overflow {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("required check %s lacks successful exact-tree evidence", check.ID)
		}
	}
	// Rebuild the protected regression identity; a candidate-only success cannot
	// erase a failing preserved suite or substitute observations from another tree.
	protectedTree, err := verification.ProtectedTree(ctx, r.Workspace.Path, p, v.SourceTree)
	if err != nil {
		return err
	}
	hasRegression := false
	for _, check := range p.Analysis.Checks {
		if check.Category == "regression" {
			hasRegression = true
		}
	}
	if hasRegression && protectedTree != v.SourceTree {
		if v.Protected == nil || v.Protected.SourceTree != protectedTree || !v.Protected.RequiredChecksPassed || v.Protected.Error != "" {
			return fmt.Errorf("missing successful protected regression evidence")
		}
		for _, check := range p.Analysis.Checks {
			if check.Category != "regression" || !check.Required {
				continue
			}
			found := false
			for _, obs := range v.Protected.Checks {
				if obs.ID == check.ID && reflect.DeepEqual(obs.Command, check) && obs.Category == check.Category && obs.SourceTree == protectedTree && obs.Result.OK && !obs.Result.TimedOut && !obs.Result.Interrupted && !obs.Result.Overflow {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("missing protected regression check %s", check.ID)
			}
		}
	}
	allowed := []string{}
	for _, s := range p.Analysis.Scope {
		allowed = append(allowed, s.Path)
	}
	if expectedHead == "" {
		captured, err := verification.Capture(ctx, r.Workspace.Path, r.Workspace.BaseCommit, allowed, p.Artifacts)
		if err != nil {
			return err
		}
		if len(captured.Violations) > 0 || captured.Tree != v.SourceTree {
			return fmt.Errorf("source changed after verification or exceeds approved scope; new verification is required")
		}
	} else {
		// Recovery after publication must still bind its already-created commit tree
		// to the evidence; the existing receipt checks validate parent/scope/diff.
		tree, err := git(ctx, r.Workspace.Path, nil, "rev-parse", expectedHead+"^{tree}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(tree) != v.SourceTree {
			return fmt.Errorf("publication commit differs from verified source")
		}
	}
	return nil
}

// MatchesPublishedEvidence validates local provenance only. Publication does not
// prove merge, deployment, current remote PR state, or production health.
// Lifecycle observations do not alter this historical provenance predicate.
func MatchesPublishedEvidence(receipt Receipt, r molecule.Record) bool {
	p := receipt.Preview
	return receipt.State == "published" && validURL(p.Repo, receipt.URL) && len(receipt.Commit) == 40 && strings.Trim(receipt.Commit, "0123456789abcdef") == "" && digest(p) == p.Digest && p.MoleculeID == r.ID && p.Repo == r.Issue.Repo && p.BaseCommit == r.Workspace.BaseCommit && p.Branch == r.Workspace.Branch && r.Verification != nil && p.Tree == r.Verification.SourceTree
}
