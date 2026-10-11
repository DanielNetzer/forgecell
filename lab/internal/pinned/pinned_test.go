package pinned

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func unixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission and symlink checks are unsupported on Windows")
	}
}

// tempRoot resolves symlinks so pinned paths never pass through one; macOS
// temporary directories do.
func tempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func sum(content string) string {
	s := sha256.Sum256([]byte(content))
	return hex.EncodeToString(s[:])
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected an error containing %q, got %v", substr, err)
	}
}

func TestPinnedCheckPlacementAcceptsPrivateFile(t *testing.T) {
	unixOnly(t)
	path := writeFile(t, filepath.Join(tempRoot(t), "dir", "tool"), "x", 0o600)
	if err := CheckPlacement(path); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedCheckPlacementRefusesSymlinks(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	real := writeFile(t, filepath.Join(root, "real", "tool"), "x", 0o600)
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	wantErr(t, CheckPlacement(link), "is a symlink")
	dirLink := filepath.Join(root, "dirlink")
	if err := os.Symlink(filepath.Dir(real), dirLink); err != nil {
		t.Fatal(err)
	}
	wantErr(t, CheckPlacement(filepath.Join(dirLink, "tool")), "through a symlink")
}

func TestPinnedCheckPlacementRefusesWritableFileAndDirectory(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	for _, mode := range []os.FileMode{0o660, 0o606, 0o666, 0o777} {
		path := writeFile(t, filepath.Join(root, "files", mode.String()), "x", mode)
		wantErr(t, CheckPlacement(path), "group- or world-writable")
	}
	open := filepath.Join(root, "open")
	path := writeFile(t, filepath.Join(open, "tool"), "x", 0o600)
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	wantErr(t, CheckPlacement(path), "writable directory")
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := CheckPlacement(path); err != nil {
		t.Fatalf("a sticky directory is acceptable: %v", err)
	}
	wantErr(t, CheckPlacement(filepath.Join(root, "missing")), "is unreadable")
}

func TestPinnedOpenRequiresRegularFile(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	dir := filepath.Join(root, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wantErr(t, func() error {
		f, err := Open(dir)
		if f != nil {
			f.Close()
		}
		return err
	}(), "must be a regular file")
	file, err := Open(writeFile(t, filepath.Join(root, "tool"), "x", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
}

func TestPinnedOpenRefusesOversizeFile(t *testing.T) {
	unixOnly(t)
	path := writeFile(t, filepath.Join(tempRoot(t), "big"), "", 0o600)
	if err := os.Truncate(path, MaxFileBytes); err != nil {
		t.Fatal(err)
	}
	file, err := Open(path)
	if err != nil {
		t.Fatalf("a file of exactly MaxFileBytes is allowed: %v", err)
	}
	file.Close()
	if err := os.Truncate(path, MaxFileBytes+1); err != nil {
		t.Fatal(err)
	}
	file, err = Open(path)
	if file != nil {
		file.Close()
	}
	wantErr(t, err, "exceeds")
}

func TestPinnedVerifyReportsIdentityChangeAndUnavailable(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	path := writeFile(t, filepath.Join(root, "tool"), "content", 0o600)
	id, finding := Verify("executable", File{Path: path, SHA256: sum("content")}, "changed")
	if finding != "" || id != (Identity{Role: "executable", Path: path, SHA256: sum("content")}) {
		t.Fatalf("matching file: identity %+v, finding %q", id, finding)
	}
	other := sum("other")
	id, finding = Verify("executable", File{Path: path, SHA256: other}, "changed")
	if id != (Identity{}) || !strings.HasPrefix(finding, "changed: executable "+path+" changed: SHA-256 "+sum("content")+" differs from the pinned "+other) {
		t.Fatalf("mismatch: identity %+v, finding %q", id, finding)
	}
	_, finding = Verify("enforcement evidence r", File{Path: filepath.Join(root, "missing"), SHA256: other}, "missing-evidence")
	if !strings.HasPrefix(finding, "missing-evidence: enforcement evidence r ") || !strings.Contains(finding, "missing") {
		t.Fatalf("unavailable file must use the caller's class: %q", finding)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, finding = Verify("dependency", File{Path: link, SHA256: sum("content")}, "changed")
	if !strings.HasPrefix(finding, "changed: dependency "+link+" is a symlink") {
		t.Fatalf("symlink: %q", finding)
	}
}

func TestPinnedCopyRehashesAndSetsMode(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	for _, c := range []struct {
		name string
		mode os.FileMode
		want os.FileMode
	}{{"exec", 0o700, 0o500}, {"data", 0o600, 0o400}} {
		src := writeFile(t, filepath.Join(root, "src", c.name), "payload "+c.name, c.mode)
		dst := filepath.Join(root, "dst-"+c.name)
		if err := Copy(File{Path: src, SHA256: sum("payload " + c.name)}, dst); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dst)
		if err != nil || info.Mode().Perm() != c.want {
			t.Fatalf("%s: copy mode %v, err %v, want %v", c.name, info.Mode().Perm(), err, c.want)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != "payload "+c.name {
			t.Fatalf("%s: copied bytes %q, err %v", c.name, got, err)
		}
	}
}

func TestPinnedCopyRefusesChangedSourceAndExistingDestination(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	src := writeFile(t, filepath.Join(root, "src", "tool"), "payload", 0o600)
	wantErr(t, Copy(File{Path: src, SHA256: sum("different")}, filepath.Join(root, "dst1")), "changed after approval")
	if err := Copy(File{Path: src, SHA256: sum("payload")}, filepath.Join(root, "dst2")); err != nil {
		t.Fatal(err)
	}
	if err := Copy(File{Path: src, SHA256: sum("payload")}, filepath.Join(root, "dst2")); err == nil {
		t.Fatal("an existing destination must not be overwritten")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	wantErr(t, Copy(File{Path: link, SHA256: sum("payload")}, filepath.Join(root, "dst3")), "is a symlink")
}

func TestPinnedRejectDuplicateKeys(t *testing.T) {
	for _, ok := range []string{
		`{}`,
		`{"a":1,"b":{"a":2},"c":[{"a":1},{"a":2}]}`,
		`[1,"a",null]`,
	} {
		if err := RejectDuplicateKeys([]byte(ok)); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, c := range [][2]string{
		{`{"a":1,"a":2}`, "a"},
		{`{"x":{"k":1,"k":2}}`, "k"},
		{`{"x":[{"y":1,"y":2}]}`, "y"},
	} {
		var dup *DuplicateKeyError
		if err := RejectDuplicateKeys([]byte(c[0])); !errors.As(err, &dup) || dup.Key != c[1] {
			t.Fatalf("%s: expected a duplicate key %q, got %v", c[0], c[1], err)
		}
	}
	for _, bad := range []string{``, `{"a":`, `{"a" 1}`} {
		err := RejectDuplicateKeys([]byte(bad))
		var dup *DuplicateKeyError
		if err == nil || errors.As(err, &dup) {
			t.Fatalf("%q: expected a syntax error, got %v", bad, err)
		}
	}
}

func TestPinnedJSONShapeIsStable(t *testing.T) {
	raw, err := json.Marshal(Identity{Role: "r", Path: "/p", SHA256: "s"})
	if err != nil || string(raw) != `{"role":"r","path":"/p","sha256":"s"}` {
		t.Fatalf("identity JSON drifted: %s %v", raw, err)
	}
	raw, err = json.Marshal(File{Path: "/p", SHA256: "s"})
	if err != nil || string(raw) != `{"path":"/p","sha256":"s"}` {
		t.Fatalf("file JSON drifted: %s %v", raw, err)
	}
}
