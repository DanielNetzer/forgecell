package resources

import (
	"context"
	"slices"
	"testing"
)

func TestEffectiveParallelCapsByAvailableMemory(t *testing.T) {
	for _, c := range []struct {
		requested int
		memory    uint64
		want      int
	}{
		{3, 16 * GiB, 3},
		{3, 12 * GiB, 3},
		{3, 11 * GiB, 2},
		{4, 8 * GiB, 2},
		{3, 7 * GiB, 1},
		{2, 3 * GiB, 1},
		{1, 0, 1},
		{4, 64 * GiB, 4},
	} {
		if got := EffectiveParallel(c.requested, c.memory); got != c.want {
			t.Errorf("EffectiveParallel(%d, %s) = %d, want %d", c.requested, Format(c.memory), got, c.want)
		}
	}
}

func TestGoParallelismSharesCPUs(t *testing.T) {
	for _, c := range []struct{ cpus, effective, want int }{{8, 1, 8}, {8, 3, 2}, {10, 4, 2}, {2, 4, 1}, {1, 1, 1}, {8, 0, 8}} {
		if got := GoParallelism(c.cpus, c.effective); got != c.want {
			t.Errorf("GoParallelism(%d, %d) = %d, want %d", c.cpus, c.effective, got, c.want)
		}
	}
}

func TestGoFlagsWithParallelismAppendsOrReplaces(t *testing.T) {
	for _, c := range []struct {
		existing string
		p        int
		want     string
	}{
		{"", 4, "-p=4"},
		{"-mod=mod", 2, "-mod=mod -p=2"},
		{"-p=8 -mod=mod", 2, "-mod=mod -p=2"},
		{"  -trimpath   -p=1 ", 3, "-trimpath -p=3"},
		{"-mod=mod", 0, "-mod=mod -p=1"},
	} {
		if got := GoFlagsWithParallelism(c.existing, c.p); got != c.want {
			t.Errorf("GoFlagsWithParallelism(%q, %d) = %q, want %q", c.existing, c.p, got, c.want)
		}
	}
}

func TestApplyGoParallelismOnlyWithContextValue(t *testing.T) {
	env := []string{"HOME=/h", "GOFLAGS=-mod=mod -p=16"}
	if got := ApplyGoParallelism(context.Background(), env); !slices.Equal(got, env) {
		t.Fatalf("without a value the environment must be unchanged, got %v", got)
	}
	got := ApplyGoParallelism(WithGoParallelism(context.Background(), 2), env)
	if !slices.Equal(got, []string{"HOME=/h", "GOFLAGS=-mod=mod -p=2"}) {
		t.Fatalf("got %v", got)
	}
	if got := ApplyGoParallelism(WithGoParallelism(context.Background(), 3), []string{"HOME=/h"}); !slices.Equal(got, []string{"HOME=/h", "GOFLAGS=-p=3"}) {
		t.Fatalf("got %v", got)
	}
}
