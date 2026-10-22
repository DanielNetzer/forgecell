package molecule

import (
	"errors"
	"fmt"
	"os"
)

// TryLockForCleanup takes the Molecule's exclusive advisory lock without blocking, so
// doctor --clean can remove the Molecule's trees while no loop or command uses it.
// locked is true, with a nil error, when another process holds the lock; the caller
// then skips the Molecule. unlock releases the lock once the trees are removed.
func TryLockForCleanup(lab, id string) (unlock func(), locked bool, err error) {
	unlock, err = lockMolecule(lab, id)
	if errors.Is(err, errLocked) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return unlock, false, nil
}

// HasRecoveryProposal reports whether the Lab holds a saved recovery proposal for the
// Molecule. Its trees are then still needed by the recovery the proposal names.
func HasRecoveryProposal(lab, id string) (bool, error) {
	if !safeLoopID(id) {
		return false, fmt.Errorf("invalid Molecule id")
	}
	info, err := os.Lstat(recoveryProposalPath(lab, id))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}
