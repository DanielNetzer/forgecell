package pinned

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

func TestPinnedRefusesFilesAndAncestorsOwnedByOthers(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	path := writeFile(t, filepath.Join(root, "dir", "tool"), "x", 0o600)
	if err := CheckPlacement(path); err != nil {
		t.Fatal(err)
	}
	me := uint32(os.Geteuid())

	restore := OverrideTrustedUIDForTest(func(uint32) bool { return false })
	err := CheckPlacement(path)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "owned by uid "+strconv.Itoa(int(me))) {
		t.Fatalf("a file owned by an untrusted uid must be refused by path and uid: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open must refuse an untrusted owner")
	}
	if _, finding := Verify("tool", File{Path: path, SHA256: sum("x")}, "unavailable"); !strings.HasPrefix(finding, "unavailable:") {
		t.Fatalf("Verify must refuse an untrusted owner: %q", finding)
	}
	restore()
	if err := CheckPlacement(path); err != nil {
		t.Fatalf("the override must be undone: %v", err)
	}
}

func TestPinnedRefusesHardLinkedFiles(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	path := writeFile(t, filepath.Join(root, "dir", "tool"), "x", 0o600)
	other := filepath.Join(root, "other", "alias")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, other); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	wantErr(t, CheckPlacement(path), "2 hard links")
	wantErr(t, CheckPlacement(other), other)
	if _, err := Open(path); err == nil {
		t.Fatal("Open must refuse a hard-linked file")
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if err := CheckPlacement(path); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedPathInArgComparesCleanedComponents(t *testing.T) {
	for _, c := range []struct {
		arg, path string
		want      bool
	}{
		{"/opt/x/tool", "/opt/x/tool", true},
		{"/opt/x/./tool", "/opt/x/tool", true},
		{"/opt//x/tool", "/opt/x/tool", true},
		{"/opt/x/../x/tool", "/opt/x/tool", true},
		{"--flag=/opt/x/tool", "/opt/x/tool", true},
		{"--paths=/usr/bin:/opt/x/tool", "/opt/x/tool", true},
		{"--paths=/opt/x/tool,/usr/bin", "/opt/x/tool", true},
		{"/abc", "/a", false},
		{"/opt/x/tool2", "/opt/x/tool", false},
		{"/opt/x/tool.cfg", "/opt/x/tool", false},
		{"/opt/x/tool/../other", "/opt/x/tool", false},
		{"plain", "/opt/x/tool", false},
	} {
		if got := PathInArg(c.arg, c.path); got != c.want {
			t.Errorf("PathInArg(%q, %q) = %t, want %t", c.arg, c.path, got, c.want)
		}
	}
}

func TestPinnedPathInArgCatchesSymlinkAliases(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	real := writeFile(t, filepath.Join(root, "real", "tool"), "x", 0o600)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "real"), alias); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if !PathInArg(filepath.Join(alias, "tool"), real) {
		t.Error("an argument through a symlinked directory names the pinned file")
	}
	if !PathInArg("--cfg="+filepath.Join(alias, "tool"), real) {
		t.Error("a flag value through a symlinked directory names the pinned file")
	}
	if !PathInArg(real, filepath.Join(alias, "tool")) {
		t.Error("a pinned path through a symlink is resolved too")
	}
	if !PathInArg(filepath.Join(alias, "not-yet-created", "x"), filepath.Join(root, "real", "not-yet-created")) {
		t.Error("a non-existing tail keeps its resolved leading directory")
	}
	if PathInArg(filepath.Join(alias, "other"), real) {
		t.Error("a sibling is not the pinned file")
	}
	if runtime.GOOS == "darwin" {
		if resolved, err := filepath.EvalSymlinks("/tmp"); err == nil && resolved != "/tmp" {
			if !PathInArg("/tmp/forgecell-x/tool", resolved+"/forgecell-x/tool") {
				t.Error("/tmp names /private/tmp on macOS")
			}
		}
		if !PathInArg("/OPT/X/Tool", "/opt/x/tool") {
			t.Error("macOS paths compare case-insensitively")
		}
	} else if PathInArg("/OPT/X/Tool", "/opt/x/tool") {
		t.Error("paths compare case-sensitively off macOS")
	}
}

func TestPinnedPathInArgSplitsOnShellDelimiters(t *testing.T) {
	for _, arg := range []string{
		"cat</opt/x/tool", "cat>/opt/x/tool", "a|/opt/x/tool", "a&/opt/x/tool",
		"$(/opt/x/tool)", "(/opt/x/tool)", "{/opt/x/tool}", "[/opt/x/tool]", "a&&/opt/x/tool|b",
	} {
		if !PathInArg(arg, "/opt/x/tool") {
			t.Errorf("%q names the pinned path", arg)
		}
	}
}

func TestPinnedAllowsMultiLinkFilesOnlyUnderRoot(t *testing.T) {
	unixOnly(t)
	var system string
	for _, candidate := range []string{"/usr/bin/git", "/usr/bin/python3", "/bin/sh", "/usr/bin/env"} {
		info, err := os.Lstat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if n, _ := LinkCount(info); n > 1 {
			system = candidate
			break
		}
	}
	if system == "" {
		t.Skip("no root-owned multi-link system file available")
	}
	info, _ := os.Lstat(system)
	if !rootProtected(system, info) {
		t.Skipf("%s is not root-protected on this machine", system)
	}
	if err := CheckPlacement(system); err != nil {
		t.Fatalf("a root-owned multi-link file under a root-owned path must be accepted: %v", err)
	}
	if file, err := Open(system); err != nil {
		t.Fatalf("Open must accept it too: %v", err)
	} else {
		file.Close()
	}
}

func TestPinnedRefusesUserOwnedHardLinkWithAdvice(t *testing.T) {
	unixOnly(t)
	root := tempRoot(t)
	path := writeFile(t, filepath.Join(root, "dir", "tool"), "x", 0o600)
	if err := os.Link(path, filepath.Join(root, "alias")); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	info, _ := os.Lstat(path)
	if rootProtected(path, info) {
		t.Skip("running as root: the file is root-owned")
	}
	wantErr(t, CheckPlacement(path), "private location")
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
