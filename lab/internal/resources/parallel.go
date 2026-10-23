package resources

import (
	"context"
	"strconv"
	"strings"
)

// MemoryPerAttempt is the available memory one parallel attempt is budgeted.
const MemoryPerAttempt = 4 * GiB

// EffectiveParallel is min(requested, max(1, floor(availableMemory / 4 GiB))).
func EffectiveParallel(requested int, availableMemory uint64) int {
	return max(1, min(requested, int(availableMemory/MemoryPerAttempt)))
}

// GoParallelism is the go -p value for each of effective concurrent attempts:
// max(1, numCPU / effective).
func GoParallelism(numCPU, effective int) int {
	return max(1, numCPU/max(1, effective))
}

// GoFlagsWithParallelism returns the GOFLAGS value existing with -p=p: an existing -p
// setting is replaced, every other flag is kept in order.
func GoFlagsWithParallelism(existing string, p int) string {
	kept := []string{}
	for _, f := range strings.Fields(existing) {
		if strings.HasPrefix(f, "-p=") || strings.HasPrefix(f, "--p=") {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(append(kept, "-p="+strconv.Itoa(max(1, p))), " ")
}

type goParallelismKey struct{}

// WithGoParallelism makes every check environment built under ctx run go with -p=p.
func WithGoParallelism(ctx context.Context, p int) context.Context {
	return context.WithValue(ctx, goParallelismKey{}, max(1, p))
}

// GoParallelismFrom returns the -p value WithGoParallelism set on ctx.
func GoParallelismFrom(ctx context.Context) (int, bool) {
	p, ok := ctx.Value(goParallelismKey{}).(int)
	return p, ok
}

// ApplyGoParallelism sets GOFLAGS in env to carry ctx's -p value, replacing an existing
// GOFLAGS entry's -p and keeping its other flags. Without a value on ctx env is returned
// unchanged.
func ApplyGoParallelism(ctx context.Context, env []string) []string {
	p, ok := GoParallelismFrom(ctx)
	if !ok {
		return env
	}
	out := make([]string, 0, len(env)+1)
	existing := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GOFLAGS="); ok {
			existing = v
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GOFLAGS="+GoFlagsWithParallelism(existing, p))
}
