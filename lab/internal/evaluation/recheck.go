package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// Recheck independently grades existing output trees without rerunning a model or rewriting evidence.
func Recheck(ctx context.Context, source, evidence, output string, originalChecks []string) (report Report, err error) {
	source, err = filepath.Abs(source)
	if err != nil {
		return report, err
	}
	evidence, err = filepath.Abs(evidence)
	if err != nil {
		return report, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return report, err
	}
	raw, err := bounded(filepath.Join(evidence, "report.json"))
	if err != nil {
		return report, err
	}
	originalReportHash := Hash(raw)
	if err = json.Unmarshal(raw, &report); err != nil {
		return report, err
	}
	if report.Status != "complete" || len(report.Attempts) != 2 {
		return report, fmt.Errorf("recheck requires both completed recorded attempts")
	}
	inputDir := filepath.Join(evidence, "inputs")
	planRaw, err := bounded(filepath.Join(inputDir, "plan.json"))
	if err != nil {
		return report, err
	}
	if Hash(planRaw) != report.PlanSHA256 {
		return report, fmt.Errorf("original plan changed")
	}
	var p Plan
	if err = json.Unmarshal(planRaw, &p); err != nil {
		return report, err
	}
	if p.BaseCommit != report.BaseCommit || p.TaskSHA256 != report.TaskSHA256 || p.ApprovalSHA256 != report.ApprovalSHA256 {
		return report, fmt.Errorf("report input identities differ")
	}
	approvalRaw, err := frozen(inputDir, "approval.json", p.ApprovalSHA256)
	if err != nil {
		return report, err
	}
	pair, err := ValidateApproval(approvalRaw)
	if err != nil {
		return report, err
	}
	taskRaw, err := frozen(inputDir, "issue.json", p.TaskSHA256)
	if err != nil {
		return report, err
	}
	var issue molecule.Issue
	if err = json.Unmarshal(taskRaw, &issue); err != nil {
		return report, err
	}
	acceptanceRaw, err := frozen(inputDir, "acceptance"+filepath.Ext(p.Acceptance), p.AcceptanceSHA256)
	if err != nil {
		return report, err
	}
	scope := map[string]bool{}
	for _, f := range p.AllowedFiles {
		scope[f] = true
	}
	for _, f := range originalChecks {
		if !relative(f, false) || !scope[f] {
			return report, fmt.Errorf("original check file must be in the recorded scope")
		}
	}
	p.OriginalCheckFiles = append(p.OriginalCheckFiles, originalChecks...)
	if err = os.Mkdir(output, 0700); err != nil {
		return report, fmt.Errorf("recheck needs a new evidence directory: %w", err)
	}
	acceptance := filepath.Join(output, "acceptance"+filepath.Ext(p.Acceptance))
	if err = os.WriteFile(acceptance, acceptanceRaw, 0400); err != nil {
		return report, err
	}
	revisedPlan, _ := json.MarshalIndent(p, "", "  ")
	if err = os.WriteFile(filepath.Join(output, "verification-plan.json"), revisedPlan, 0400); err != nil {
		return report, err
	}
	report.Notes = append(report.Notes, "Reverified captured output trees in new checkouts with fresh dependencies. No model was rerun.", "Original report SHA-256: "+originalReportHash, "Verification plan SHA-256: "+Hash(revisedPlan), "Original check files retained from baseline: "+fmt.Sprint(p.OriginalCheckFiles))
	report.Status = "verifying"
	if err = saveReport(output, report); err != nil {
		return report, err
	}
	expected := []string{pair.Baseline.SHA256, pair.Candidate.SHA256}
	for i := range report.Attempts {
		a := &report.Attempts[i]
		if a.Variant != []string{"baseline", "candidate"}[i] || a.FormulaSHA256 != expected[i] || a.Molecule.FormulaSnapshot.SHA256 != expected[i] || a.Molecule.Workspace.BaseCommit != p.BaseCommit || Hash([]byte(a.Molecule.FormulaSnapshot.YAML)) != expected[i] || !reflect.DeepEqual(a.Molecule.Issue, issue) {
			return report, fmt.Errorf("attempt differs from frozen comparison")
		}
		a.Verification = verify(ctx, source, filepath.Join(output, a.Variant), acceptance, p, *a)
		a.Checks = a.Verification.Checks
		a.Correct = a.Verification.Correct
		if a.Verification.Error != "" {
			a.Error = a.Verification.Error
		}
		if err = saveReport(output, report); err != nil {
			return report, err
		}
	}
	report.Status = "complete"
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	err = saveReport(output, report)
	return report, err
}
