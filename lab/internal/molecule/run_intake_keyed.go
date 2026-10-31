package molecule

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
)

// RunIntakeKeyed is RunIntake for a keyed Issue: intake first takes the key's
// start lock with owner run, so two keyed Molecules are never created for one
// key; a held lock refuses with ErrIssueBusy, naming its holder, as the numeric
// path does. An approval (Approve set) takes the Molecule lock instead and goes
// straight to RunKeyed.
func RunIntakeKeyed(ctx context.Context, o Options, key string) (Record, error) {
	if o.Approve != "" {
		return RunKeyed(ctx, o, key)
	}
	// The key names the lock file, so it is validated before anything is created.
	if err := intake.ValidateIssueKey(key); err != nil {
		return Record{}, err
	}
	lab, err := filepath.Abs(o.LabDir)
	if err != nil {
		return Record{}, err
	}
	loaded, err := formula.Load(lab, o.FormulaID)
	if err != nil {
		return Record{}, err
	}
	if !loaded.Approved {
		return Record{}, fmt.Errorf("approve a Formula with init before running")
	}
	repository, err := intake.BindKeyRepository(loaded.Formula.Intake.Repo)
	if err != nil {
		return Record{}, err
	}
	name, err := issueKeyLockName(repository, key)
	if err != nil {
		return Record{}, err
	}
	file, release, err := flockLedgerHandle(lab, name)
	if errors.Is(err, errLocked) {
		return Record{}, fmt.Errorf("%w: %s holds the start lock for Issue %s; wait for it to finish, then run this again. Nothing was started", ErrIssueBusy, issueLockHolderNamed(lab, name), key)
	}
	if err != nil {
		return Record{}, fmt.Errorf("the Issue start lock could not be taken: %w", pathlessErr(err))
	}
	defer release()
	if err = writeMarkerNote(file, issueOwnerRun); err != nil {
		return Record{}, fmt.Errorf("the Issue start lock could not be taken: %w", pathlessErr(err))
	}
	return RunKeyed(ctx, o, key)
}

// issueLockHolderNamed is issueLockHolder for the start lock file name.
func issueLockHolderNamed(lab, name string) string {
	switch readMarkerNote(filepath.Join(lab, "ledgers", name)) {
	case issueOwnerLoop:
		return "a running loop (forgecell loop or a Lab UI)"
	case issueOwnerRun:
		return "a forgecell run (from a terminal or a Lab UI)"
	}
	return "another process"
}
