// Package resources keeps Forgecell from destabilizing the user's machine: it reads
// free disk and available memory, refuses to start heavy work below soft floors,
// cancels running work below hard floors, caps parallelism, and owns the registry and
// removal helper for every Forgecell temp directory.
package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GiB is the unit every floor and reading is reported in.
const GiB = uint64(1) << 30

// Volume is the free and total space of the filesystem holding Path.
type Volume struct {
	Path  string `json:"path"`
	Free  uint64 `json:"freeBytes"`
	Total uint64 `json:"totalBytes"`
}

// Readings is one sample of the machine. MemoryKnown is false when available memory
// could not be read; memory floors then do not apply.
type Readings struct {
	Volumes     []Volume `json:"volumes"`
	Memory      uint64   `json:"availableMemoryBytes"`
	MemoryKnown bool     `json:"memoryKnown"`
}

// Reader takes one sample. Tests inject fixed or scripted readings.
type Reader interface {
	Read() Readings
}

// ReaderFunc adapts a function to Reader.
type ReaderFunc func() Readings

func (f ReaderFunc) Read() Readings { return f() }

// Floors are the thresholds a Guard enforces. A disk floor is the larger of its fixed
// bytes and its percentage of the volume's size, with the percentage part capped so a
// large disk does not demand an unreasonable amount of free space; a configured disk
// floor (from the Lab config) replaces both with that fixed size.
type Floors struct {
	DiskSoft, DiskHard               uint64
	DiskSoftPercent, DiskHardPercent float64
	DiskSoftCap, DiskHardCap         uint64
	MemorySoft, MemoryHard           uint64
}

// Minimums no configured floor may go below.
const (
	MinDiskFloor   = 2 * GiB
	MinMemoryFloor = 1 * GiB
)

// DefaultFloors: disk soft min(max(15 GiB, 10%), 30 GiB), hard min(max(5 GiB, 3%),
// 10 GiB); memory soft 3 GiB, hard 1.5 GiB.
func DefaultFloors() Floors {
	return Floors{DiskSoft: 15 * GiB, DiskHard: 5 * GiB, DiskSoftPercent: 10, DiskHardPercent: 3, DiskSoftCap: 30 * GiB, DiskHardCap: 10 * GiB, MemorySoft: 3 * GiB, MemoryHard: 3 * GiB / 2}
}

// Clamp raises every floor to its minimum and keeps each hard floor at or below its
// soft floor.
func (f Floors) Clamp() Floors {
	f.DiskSoft, f.DiskHard = max(f.DiskSoft, MinDiskFloor), max(f.DiskHard, MinDiskFloor)
	f.MemorySoft, f.MemoryHard = max(f.MemorySoft, MinMemoryFloor), max(f.MemoryHard, MinMemoryFloor)
	f.DiskHard, f.MemoryHard = min(f.DiskHard, f.DiskSoft), min(f.MemoryHard, f.MemorySoft)
	f.DiskSoftPercent, f.DiskHardPercent = max(f.DiskSoftPercent, 0), max(f.DiskHardPercent, 0)
	f.DiskHardPercent = min(f.DiskHardPercent, f.DiskSoftPercent)
	return f
}

// DiskFloors returns the soft and hard floors for a volume of total bytes. The
// percentage part never exceeds its cap (when set); the fixed part is never capped, so
// a configured floor is honoured as written.
func (f Floors) DiskFloors(total uint64) (soft, hard uint64) {
	percent := func(p float64, limit uint64) uint64 {
		v := uint64(float64(total) * p / 100)
		if limit > 0 {
			v = min(v, limit)
		}
		return v
	}
	soft = max(f.DiskSoft, percent(f.DiskSoftPercent, f.DiskSoftCap))
	hard = max(f.DiskHard, percent(f.DiskHardPercent, f.DiskHardCap))
	return soft, min(hard, soft)
}

// ConfigFile is the optional Lab config that overrides floors, at <lab>/resources.json:
// {"diskSoftFloorGiB": 20, "diskHardFloorGiB": 8, "memorySoftFloorGiB": 4, "memoryHardFloorGiB": 2}.
// Every field is optional; values below the minimums (2 GiB disk, 1 GiB memory) are raised.
const ConfigFile = "resources.json"

type config struct {
	DiskSoftFloorGiB   *float64 `json:"diskSoftFloorGiB,omitempty"`
	DiskHardFloorGiB   *float64 `json:"diskHardFloorGiB,omitempty"`
	MemorySoftFloorGiB *float64 `json:"memorySoftFloorGiB,omitempty"`
	MemoryHardFloorGiB *float64 `json:"memoryHardFloorGiB,omitempty"`
}

// LoadFloors reads the Lab's floors: the defaults, overridden by <lab>/resources.json
// when it exists, always clamped to the minimums. A malformed file is an error, so a
// typo never silently disables a floor the user meant to set.
func LoadFloors(labDir string) (Floors, error) {
	f := DefaultFloors()
	if labDir == "" {
		return f.Clamp(), nil
	}
	raw, err := os.ReadFile(filepath.Join(labDir, ConfigFile))
	if os.IsNotExist(err) {
		return f.Clamp(), nil
	}
	if err != nil {
		return f.Clamp(), err
	}
	var c config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return f.Clamp(), fmt.Errorf("invalid %s: %w", filepath.Join(labDir, ConfigFile), err)
	}
	gib := func(v *float64, dst *uint64) {
		if v != nil && *v > 0 {
			*dst = uint64(*v * float64(GiB))
		}
	}
	if c.DiskSoftFloorGiB != nil {
		gib(c.DiskSoftFloorGiB, &f.DiskSoft)
		f.DiskSoftPercent = 0
	}
	if c.DiskHardFloorGiB != nil {
		gib(c.DiskHardFloorGiB, &f.DiskHard)
		f.DiskHardPercent = 0
	}
	gib(c.MemorySoftFloorGiB, &f.MemorySoft)
	gib(c.MemoryHardFloorGiB, &f.MemoryHard)
	return f.Clamp(), nil
}

// Level is how far a reading is below its floors.
type Level int

const (
	OK Level = iota
	BelowSoft
	BelowHard
)

// Evaluate returns the worst level across every volume and memory, with a reason
// naming the resource ("resources: low disk (...)" or "resources: low memory (...)").
func (f Floors) Evaluate(r Readings) (Level, string) {
	level, reason := OK, ""
	note := func(l Level, text string) {
		if l > level {
			level, reason = l, text
		}
	}
	for _, v := range r.Volumes {
		soft, hard := f.DiskFloors(v.Total)
		switch {
		case v.Free < hard:
			note(BelowHard, fmt.Sprintf("resources: low disk (%s: %s free, hard floor %s)", v.Path, Format(v.Free), Format(hard)))
		case v.Free < soft:
			note(BelowSoft, fmt.Sprintf("resources: low disk (%s: %s free, soft floor %s)", v.Path, Format(v.Free), Format(soft)))
		}
	}
	if r.MemoryKnown {
		switch {
		case r.Memory < f.MemoryHard:
			note(BelowHard, fmt.Sprintf("resources: low memory (%s available, hard floor %s)", Format(r.Memory), Format(f.MemoryHard)))
		case r.Memory < f.MemorySoft:
			note(BelowSoft, fmt.Sprintf("resources: low memory (%s available, soft floor %s)", Format(r.Memory), Format(f.MemorySoft)))
		}
	}
	return level, reason
}

// Format renders bytes as GiB with one decimal.
func Format(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/float64(GiB)) }

// TreeSize sums the sizes of the regular files under path without following symbolic
// links. It stops once budget has elapsed (zero means no limit) and then reports the
// partial sum with complete false, so a huge cache never stalls doctor. A missing path
// is zero bytes.
func TreeSize(path string, budget time.Duration) (size uint64, complete bool) {
	deadline := time.Now().Add(budget)
	complete = true
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if budget > 0 && time.Now().After(deadline) {
			complete = false
			return filepath.SkipAll
		}
		if d.Type().IsRegular() {
			if info, e := d.Info(); e == nil {
				size += uint64(info.Size())
			}
		}
		return nil
	})
	return size, complete
}

// goCacheRetry is how long GoCacheDir remembers a failed go env resolution.
const goCacheRetry = time.Minute

var goCacheMiss struct {
	sync.Mutex
	at time.Time
}

// GoCacheDir is the build cache the disk guard watches. An explicit GOCACHE in the current
// environment wins, even when set after the process-wide resolution (GOCACHE=off means
// none); otherwise it is the shared cache every check uses (ResolveGoCache), and without
// a go toolchain Go's default build cache under the user cache directory. A failed
// resolution is not retried for goCacheRetry, so the guard does not run go env on every
// sample.
func GoCacheDir() string {
	if v := os.Getenv("GOCACHE"); v != "" {
		if v == "off" {
			return ""
		}
		return v
	}
	goCacheMiss.Lock()
	recent := !goCacheMiss.at.IsZero() && time.Since(goCacheMiss.at) < goCacheRetry
	goCacheMiss.Unlock()
	if !recent {
		if c, ok := ResolveGoCache(context.Background()); ok {
			return c.BuildCache
		}
		goCacheMiss.Lock()
		goCacheMiss.at = time.Now()
		goCacheMiss.Unlock()
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "go-build")
}

// SystemReader samples the volumes holding TMPDIR, labDir and GOCACHE, and available
// memory. A path that does not exist yet is measured at its nearest existing ancestor;
// a volume that cannot be read is left out rather than guessed.
func SystemReader(labDir string) Reader {
	paths := []string{os.TempDir()}
	if labDir != "" {
		paths = append(paths, labDir)
	}
	if cache := GoCacheDir(); cache != "" {
		paths = append(paths, cache)
	}
	return ReaderFunc(func() Readings {
		r := Readings{Volumes: []Volume{}}
		seen := map[string]bool{}
		for _, p := range paths {
			abs, err := filepath.Abs(p)
			if err != nil || seen[abs] {
				continue
			}
			seen[abs] = true
			existing := abs
			for {
				if _, err := os.Stat(existing); err == nil {
					break
				}
				parent := filepath.Dir(existing)
				if parent == existing {
					break
				}
				existing = parent
			}
			if free, total, err := diskSpace(existing); err == nil {
				r.Volumes = append(r.Volumes, Volume{Path: abs, Free: free, Total: total})
			}
		}
		r.Memory, r.MemoryKnown = availableMemory()
		return r
	})
}
