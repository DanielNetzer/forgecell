package molecule

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockForCleanupReportsAHeldLock(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	held, err := lockMolecule(lab, "mol-1")
	if err != nil {
		t.Fatal(err)
	}
	unlock, locked, err := TryLockForCleanup(lab, "mol-1")
	if err != nil || !locked || unlock != nil {
		t.Fatalf("locked=%v err=%v; want the held lock reported as locked", locked, err)
	}
	held()
	unlock, locked, err = TryLockForCleanup(lab, "mol-1")
	if err != nil || locked || unlock == nil {
		t.Fatalf("locked=%v err=%v; want the free lock taken", locked, err)
	}
	if _, err := lockMolecule(lab, "mol-1"); err == nil {
		t.Fatal("the cleanup lock must exclude a loop while held")
	}
	unlock()
	if _, _, err := TryLockForCleanup(lab, "../escape"); err == nil {
		t.Fatal("an invalid id must be refused")
	}
}

func TestHasRecoveryProposal(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	if has, err := HasRecoveryProposal(lab, "mol-1"); err != nil || has {
		t.Fatalf("has=%v err=%v; want no proposal", has, err)
	}
	if err := os.MkdirAll(filepath.Join(lab, recoveryProposalDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recoveryProposalPath(lab, "mol-1"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if has, err := HasRecoveryProposal(lab, "mol-1"); err != nil || !has {
		t.Fatalf("has=%v err=%v; want the saved proposal found", has, err)
	}
	if _, err := HasRecoveryProposal(lab, "../x"); err == nil {
		t.Fatal("an invalid id must be refused")
	}
}
