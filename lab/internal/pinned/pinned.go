// Package pinned holds the rules for trusting a file by its content: where it may
// live, how it is opened, hashed and copied. The analysis wrapper and the
// extension manifest share them.
package pinned

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// MaxFileBytes bounds every pinned file.
const MaxFileBytes = 256 << 20

// File is one content-addressed identity.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Identity is a digest Forgecell computed from the file itself at verification
// time, never one copied from a manifest.
type Identity struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// DuplicateKeyError reports the first JSON object key that repeats.
type DuplicateKeyError struct{ Key string }

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("duplicate key %q", e.Key)
}

// RejectDuplicateKeys refuses a JSON document in which any object repeats a key.
// It returns a *DuplicateKeyError for a repeat and the decoder's own error for
// malformed JSON.
func RejectDuplicateKeys(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		if delim == '{' {
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				k, _ := key.(string)
				if seen[k] {
					return &DuplicateKeyError{Key: k}
				}
				seen[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
		} else {
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = d.Token()
		return err
	}
	return walk()
}

// CheckPlacement rejects a pinned path that another principal could swap after
// verification: a symlink (or a path resolved through one), a group- or
// world-writable file, or any ancestor directory that others can write to
// without the sticky bit.
func CheckPlacement(path string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("%s cannot be checked: permission and symlink checks are unsupported on Windows", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s is unreadable: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%s is unreadable: %v", path, err)
	}
	if resolved != path {
		return fmt.Errorf("%s resolves to %s through a symlink", path, resolved)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group- or world-writable", path)
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		di, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("%s has an unreadable parent: %v", path, err)
		}
		if di.Mode().Perm()&0o022 != 0 && di.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("%s lives in the writable directory %s", path, dir)
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

// Open opens a placement-checked regular file and proves the open handle is the
// file that was checked, so a swap between check and open is refused.
func Open(path string) (*os.File, error) {
	if err := CheckPlacement(path); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s is unreadable: %v", path, err)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || after.Size() > MaxFileBytes {
		file.Close()
		return nil, fmt.Errorf("%s was replaced while it was opened or exceeds %d bytes", path, int64(MaxFileBytes))
	}
	return file, nil
}

func sha256Path(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(file, MaxFileBytes+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify hashes the file as it is now. An unavailable file is reported with the
// given class; a digest mismatch is always "changed". The finding is empty when
// the file matches its pin.
func Verify(role string, f File, unavailable string) (Identity, string) {
	file, err := Open(f.Path)
	if err != nil {
		return Identity{}, unavailable + ": " + role + " " + err.Error()
	}
	defer file.Close()
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(file, MaxFileBytes)); err != nil {
		return Identity{}, unavailable + ": " + role + " " + f.Path + " is unreadable: " + err.Error()
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != f.SHA256 {
		return Identity{}, fmt.Sprintf("changed: %s %s changed: SHA-256 %s differs from the pinned %s", role, f.Path, got, f.SHA256)
	}
	return Identity{Role: role, Path: f.Path, SHA256: got}, ""
}

// Copy streams a verified file into the private directory, proves the bytes it
// read match the pin, then re-hashes the copy that will be executed.
func Copy(f File, dst string) error {
	src, err := Open(f.Path)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	mode := os.FileMode(0o400)
	if info.Mode().Perm()&0o100 != 0 {
		mode = 0o500
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), io.LimitReader(src, MaxFileBytes))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%s changed after approval: SHA-256 %s differs from the pinned %s", f.Path, got, f.SHA256)
	}
	if err = os.Chmod(dst, mode); err != nil {
		return err
	}
	if got, err := sha256Path(dst); err != nil || got != f.SHA256 {
		return fmt.Errorf("the private copy of %s does not match its pinned SHA-256", f.Path)
	}
	return nil
}
