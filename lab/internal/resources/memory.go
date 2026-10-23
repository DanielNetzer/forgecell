package resources

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	vmStatPageSize = regexp.MustCompile(`page size of (\d+) bytes`)
	vmStatLine     = regexp.MustCompile(`^Pages (free|inactive):\s+(\d+)\.?\s*$`)
)

// ParseSysctlMemory returns available memory from `sysctl kern.memorystatus_level
// hw.memsize` output (with or without names): the memory-status level, the percentage
// of memory the kernel considers available, times physical memory. This is the figure
// memory_pressure reports as "System-wide memory free percentage".
func ParseSysctlMemory(out string) (uint64, error) {
	values := map[string]uint64{}
	var bare []uint64
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, named := strings.Cut(line, ":")
		if !named {
			value = name
		}
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("sysctl value %q is invalid", line)
		}
		if named {
			values[strings.TrimSpace(name)] = n
		} else {
			bare = append(bare, n)
		}
	}
	level, levelOK := values["kern.memorystatus_level"]
	size, sizeOK := values["hw.memsize"]
	if !levelOK && !sizeOK && len(bare) == 2 {
		level, size, levelOK, sizeOK = bare[0], bare[1], true, true
	}
	if !levelOK || !sizeOK {
		return 0, fmt.Errorf("sysctl output lacks kern.memorystatus_level or hw.memsize")
	}
	if level > 100 || size == 0 {
		return 0, fmt.Errorf("sysctl memory values are out of range (level %d, memsize %d)", level, size)
	}
	return size * level / 100, nil
}

// ParseVMStat returns free plus inactive memory from macOS vm_stat output. It is only a
// fallback for when sysctl cannot be read. Purgeable pages are deliberately not added:
// vm_stat already counts them inside active or inactive, so adding them would
// double-count. It fails when the page size or the free count is missing.
func ParseVMStat(out string) (uint64, error) {
	m := vmStatPageSize.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("vm_stat output has no page size")
	}
	page, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil || page == 0 {
		return 0, fmt.Errorf("vm_stat page size is invalid")
	}
	pages, free := uint64(0), false
	for _, line := range strings.Split(out, "\n") {
		f := vmStatLine.FindStringSubmatch(strings.TrimSpace(line))
		if f == nil {
			continue
		}
		n, err := strconv.ParseUint(f[2], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("vm_stat %s count is invalid", f[1])
		}
		pages += n
		free = free || f[1] == "free"
	}
	if !free {
		return 0, fmt.Errorf("vm_stat output has no free page count")
	}
	return pages * page, nil
}

// ParseMeminfo returns available memory from Linux /proc/meminfo: MemAvailable, the
// kernel's own estimate of free plus reclaimable memory, or MemFree plus Inactive on a
// kernel too old to report it.
func ParseMeminfo(out string) (uint64, error) {
	values := map[string]uint64{}
	for _, line := range strings.Split(out, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			n *= 1024
		}
		values[strings.TrimSpace(name)] = n
	}
	if v, ok := values["MemAvailable"]; ok {
		return v, nil
	}
	free, ok := values["MemFree"]
	if !ok {
		return 0, fmt.Errorf("/proc/meminfo has neither MemAvailable nor MemFree")
	}
	return free + values["Inactive"], nil
}

// command runs argv with a short timeout and returns its standard output.
func command(argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	return string(out), err
}

// The macOS memory sources. Tests replace them with sample output.
var (
	darwinSysctl = func() (string, error) {
		return command("/usr/sbin/sysctl", "kern.memorystatus_level", "hw.memsize")
	}
	darwinVMStat = func() (string, error) { return command("/usr/bin/vm_stat") }
)

// DarwinMemory returns available memory on macOS from sysctl (memory-status level
// times physical memory), falling back to vm_stat only when sysctl fails. Both sources
// are injected, so the computation is tested from sample output.
func DarwinMemory(sysctl, vmStat func() (string, error)) (uint64, bool) {
	if out, err := sysctl(); err == nil {
		if v, err := ParseSysctlMemory(out); err == nil {
			return v, true
		}
	}
	out, err := vmStat()
	if err != nil {
		return 0, false
	}
	v, err := ParseVMStat(out)
	return v, err == nil
}

// availableMemory reads sysctl (or vm_stat) on macOS or /proc/meminfo on Linux. It
// reports false when memory cannot be read, so memory floors are skipped rather than
// guessed.
func availableMemory() (uint64, bool) {
	switch runtime.GOOS {
	case "darwin":
		return DarwinMemory(darwinSysctl, darwinVMStat)
	case "linux":
		raw, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, false
		}
		v, err := ParseMeminfo(string(raw))
		return v, err == nil
	}
	return 0, false
}
