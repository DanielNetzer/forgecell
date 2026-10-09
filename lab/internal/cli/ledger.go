package cli

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

const ledgerInspectionLimit = 5_000_000

func readLedgerForInspection(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("ledger must be a regular file: %s", path)
	}
	// Refuse a symlink substituted after Lstat, and avoid blocking if a FIFO
	// replaces the file before open. Validate the opened object before reading.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("ledger must remain the same regular file: %s", path)
	}
	if opened.Size() > ledgerInspectionLimit {
		return nil, fmt.Errorf("ledger exceeds %d bytes: %s", ledgerInspectionLimit, path)
	}
	// The extra byte detects growth beyond the inclusive limit after Stat.
	raw, err := io.ReadAll(io.LimitReader(f, ledgerInspectionLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > ledgerInspectionLimit {
		return nil, fmt.Errorf("ledger exceeds %d bytes: %s", ledgerInspectionLimit, path)
	}
	return raw, nil
}
