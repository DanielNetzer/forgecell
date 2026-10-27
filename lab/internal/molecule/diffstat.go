package molecule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// DiffStat is the size of a captured tree against its base commit, as git diff
// --numstat counts it. Binary files count as changed files with no lines.
// Truncated means a bound was hit: the counts cover only the files read before
// it, so they are a lower bound and never the whole change.
type DiffStat struct {
	Files     int   `json:"files"`
	Added     int64 `json:"added"`
	Removed   int64 `json:"removed"`
	Binary    int   `json:"binary"`
	Truncated bool  `json:"truncated"`
}

// Bounds of one numstat: the files counted, the bytes read and the time taken.
const (
	MaxDiffStatFiles = 10000
	MaxDiffStatBytes = 1 << 20
	diffStatTimeout  = 20 * time.Second
	// maxDiffStatLines bounds one file's line count, so no sum can overflow.
	maxDiffStatLines = 1 << 40
)

var (
	// ErrDiffStatInvalid refuses an object id, Lab or workspace the numstat never runs on.
	ErrDiffStatInvalid = errors.New("diff size refused: invalid object id or workspace")
	// ErrDiffStatFailed is a git failure, timeout or unreadable output; no size is known.
	ErrDiffStatFailed = errors.New("diff size unavailable: git diff --numstat failed")
)

// objectID is a full SHA-1 or SHA-256 Git object id, lower-case hex only.
var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// DiffNumstat measures tree, a captured tree object, against the base commit with
// git diff --numstat, run directly (no shell) in workspace, which must be a
// directory of the Lab's own workspaces/ owned by this user and not a symlink.
// Both object ids must be full hex ids, so neither can be read as an option or a
// revision expression. Output, file count and time are bounded; a bound hit is
// reported as Truncated, a failure as ErrDiffStatFailed. Error values never carry
// local paths or git output.
func DiffNumstat(ctx context.Context, lab, workspace, base, tree string) (DiffStat, error) {
	var out DiffStat
	if !objectID.MatchString(base) || !objectID.MatchString(tree) {
		return out, ErrDiffStatInvalid
	}
	dir, err := labWorkspace(lab, workspace)
	if err != nil {
		return out, err
	}
	argv := []string{"git", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "core.quotePath=false",
		"diff", "--numstat", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color", base, tree, "--"}
	r := process.Run(ctx, process.Options{Argv: argv, Dir: dir, Env: numstatEnv(), Stdin: []byte{}, Timeout: diffStatTimeout, MaxOutputBytes: MaxDiffStatBytes})
	switch {
	case r.Overflow && r.Reconciled && !r.TimedOut && !r.Interrupted && !r.ParentDeadline:
		// The output was cut at the byte bound: count the complete records read.
		out = parseNumstat(r.Stdout, false)
		out.Truncated = true
		return out, nil
	case !r.OK:
		return DiffStat{}, ErrDiffStatFailed
	}
	return parseNumstat(r.Stdout, true), nil
}

// labWorkspace is workspace when it is exactly <lab>/workspaces/<name> with both
// directories real (not symlinks) and owned by this user.
func labWorkspace(lab, workspace string) (string, error) {
	if lab == "" || workspace == "" || !filepath.IsAbs(workspace) {
		return "", ErrDiffStatInvalid
	}
	lab, err := filepath.Abs(lab)
	if err != nil {
		return "", ErrDiffStatInvalid
	}
	root := filepath.Join(lab, "workspaces")
	clean := filepath.Clean(workspace)
	name := filepath.Base(clean)
	if filepath.Dir(clean) != root || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return "", ErrDiffStatInvalid
	}
	for _, dir := range []string{root, clean} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", ErrDiffStatInvalid
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
			return "", ErrDiffStatInvalid
		}
	}
	return clean, nil
}

// numstatEnv is this process's environment without GIT_* variables, which could
// point git at another repository, index or config, and with optional locks off,
// so a read never writes the index.
func numstatEnv() []string {
	env := []string{}
	for _, s := range os.Environ() {
		name, _, _ := strings.Cut(s, "=")
		if !strings.HasPrefix(name, "GIT_") {
			env = append(env, s)
		}
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1")
}

// parseNumstat counts NUL-terminated "added\tremoved\tpath" records. A record
// that is not a complete, well-formed numstat line marks the result truncated
// instead of being guessed at; complete says the output ended on a record
// boundary, so a trailing fragment is only expected when it did not.
func parseNumstat(raw string, complete bool) DiffStat {
	var out DiffStat
	records := strings.Split(raw, "\x00")
	last := records[len(records)-1]
	records = records[:len(records)-1]
	if last != "" {
		// A trailing fragment is a record cut by the byte bound.
		out.Truncated = true
	}
	for _, rec := range records {
		if rec == "" {
			continue
		}
		if out.Files == MaxDiffStatFiles {
			out.Truncated = true
			break
		}
		added, rest, ok1 := strings.Cut(rec, "\t")
		removed, file, ok2 := strings.Cut(rest, "\t")
		if !ok1 || !ok2 || file == "" {
			out.Truncated = true
			continue
		}
		if added == "-" && removed == "-" {
			out.Files++
			out.Binary++
			continue
		}
		a, errA := strconv.ParseInt(added, 10, 64)
		d, errD := strconv.ParseInt(removed, 10, 64)
		if errA != nil || errD != nil || a < 0 || d < 0 || a > maxDiffStatLines || d > maxDiffStatLines {
			out.Truncated = true
			continue
		}
		out.Files++
		out.Added += a
		out.Removed += d
	}
	if !complete {
		out.Truncated = true
	}
	return out
}
