package verification

import (
	"crypto/sha256"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type ArtifactInventory struct {
	Path   string `json:"path"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Inspect exclusions without following directory symlinks. Internal links (for
// example node_modules/.bin) are allowed only when their resolved target remains
// in the artifact root. These bytes never enter the deliverable source tree.
func inventoryArtifact(workspace string, a readiness.ArtifactRoot) (ArtifactInventory, error) {
	out := ArtifactInventory{Path: a.Path}
	maxFiles, maxBytes, maxDepth, err := a.Limits()
	if err != nil {
		return out, err
	}
	root := filepath.Join(workspace, filepath.FromSlash(a.Path))
	digest := sha256.New()
	err = filepath.WalkDir(root, func(file string, d fs.DirEntry, e error) error {
		if os.IsNotExist(e) && file == root {
			return nil
		}
		if e != nil {
			return e
		}
		if file == root {
			return nil
		}
		rel, e := filepath.Rel(root, file)
		if e != nil {
			return e
		}
		out.Files++
		if out.Files > maxFiles || len(strings.Split(rel, string(filepath.Separator))) > maxDepth {
			return fmt.Errorf("artifact inventory exceeds approved count/depth: %s", a.Path)
		}
		if strings.EqualFold(d.Name(), ".git") {
			return fmt.Errorf("nested repository in artifact: %s", a.Path)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		fmt.Fprintf(digest, "%s\x00%s\x00", filepath.ToSlash(rel), info.Mode())
		if d.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := filepath.EvalSymlinks(file)
			if e != nil {
				return fmt.Errorf("unresolved artifact link: %s", rel)
			}
			relative, e := filepath.Rel(root, target)
			if e != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("artifact link escapes root: %s", rel)
			}
			value, e := os.Readlink(file)
			if e != nil {
				return e
			}
			out.Bytes += int64(len(value))
			fmt.Fprintf(digest, "%s\x00", value)
		} else {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular artifact: %s", rel)
			}
			if info.Size() > maxBytes-out.Bytes {
				return fmt.Errorf("artifact inventory exceeds approved bytes: %s", a.Path)
			}
			f, e := os.Open(file)
			if e != nil {
				return e
			}
			h := sha256.New()
			n, e := io.Copy(h, io.LimitReader(f, maxBytes-out.Bytes+1))
			f.Close()
			if e != nil {
				return e
			}
			out.Bytes += n
			fmt.Fprintf(digest, "%d:%x\x00", n, h.Sum(nil))
		}
		if out.Bytes > maxBytes {
			return fmt.Errorf("artifact inventory exceeds approved bytes: %s", a.Path)
		}
		return nil
	})
	out.SHA256 = fmt.Sprintf("%x", digest.Sum(nil))
	return out, err
}
