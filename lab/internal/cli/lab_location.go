package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// defaultLabDir gives each local checkout its own private Lab without writing
// runtime evidence into the repository. Explicit overrides remain authoritative.
func defaultLabDir() (string, error) {
	if override := os.Getenv("FORGECELL_LAB"); override != "" {
		return override, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root := checkoutRoot(cwd)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	home := os.Getenv("FORGECELL_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(user, ".forgecell")
	}
	if !filepath.IsAbs(home) {
		// A relative custom home is relative to the checkout, not the
		// invoking subdirectory, so every command finds the same Lab.
		home = filepath.Join(root, home)
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return "", err
	}
	name := labName(filepath.Base(root))
	digest := sha256.Sum256([]byte(root))
	return filepath.Join(home, "labs", name+"-"+hex.EncodeToString(digest[:8])), nil
}

func checkoutRoot(start string) string {
	for dir := start; ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
	}
}

func existingCheckoutLab() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	legacy := filepath.Join(checkoutRoot(cwd), ".forgecell")
	for _, name := range []string{"lab.json", "assays"} {
		if _, err := os.Stat(filepath.Join(legacy, name)); err == nil {
			return legacy
		}
	}
	return ""
}

func labName(base string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
		if b.Len() >= 32 {
			break
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "checkout"
	}
	return name
}
