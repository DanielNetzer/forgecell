package resources

import (
	"errors"
	"testing"
)

const vmStatSample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               12000.
Pages active:                            400000.
Pages inactive:                          100000.
Pages speculative:                         5000.
Pages throttled:                              0.
Pages wired down:                        150000.
Pages purgeable:                          8000.
"Translation faults":                 123456789.
Pages copy-on-write:                    9876543.
`

// vmStatReal is vm_stat output in the full format macOS prints on a 16 GB Apple
// silicon Mac, every line included.
const vmStatReal = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                3349.
Pages active:                            243206.
Pages inactive:                          239455.
Pages speculative:                         2012.
Pages throttled:                              0.
Pages wired down:                        167018.
Pages purgeable:                           7283.
"Translation faults":                3069434766.
Pages copy-on-write:                   92395577.
Pages zero filled:                   1442014958.
Pages reactivated:                    233062215.
Pages purged:                          33166385.
File-backed pages:                       129820.
Anonymous pages:                         354853.
Pages stored in compressor:              921587.
Pages occupied by compressor:            274116.
Decompressions:                       237167606.
Compressions:                         292208993.
Pageins:                              104925236.
Pageouts:                               1622620.
Swapins:                               13104738.
Swapouts:                              15307301.
`

func TestParseVMStatSumsFreeAndInactiveWithoutDoubleCountingPurgeable(t *testing.T) {
	got, err := ParseVMStat(vmStatSample)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(12000+100000) * 16384; got != want {
		t.Fatalf("got %d, want %d (purgeable pages are already inside active or inactive)", got, want)
	}
	got, err = ParseVMStat(vmStatReal)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(3349+239455) * 16384; got != want {
		t.Fatalf("real vm_stat output: got %d, want %d", got, want)
	}
	if _, err := ParseVMStat("Pages free: 10.\n"); err == nil {
		t.Fatal("output without a page size must fail")
	}
	if _, err := ParseVMStat("Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages active: 1.\n"); err == nil {
		t.Fatal("output without a free count must fail")
	}
}

func TestParseSysctlMemoryMultipliesLevelByMemsize(t *testing.T) {
	const memsize = uint64(17179869184) // 16 GiB
	for _, out := range []string{
		"kern.memorystatus_level: 62\nhw.memsize: 17179869184\n",
		"62\n17179869184\n",
	} {
		got, err := ParseSysctlMemory(out)
		if err != nil {
			t.Fatalf("%q: %v", out, err)
		}
		if want := memsize * 62 / 100; got != want {
			t.Fatalf("%q: got %d, want %d (62%% of 16 GiB)", out, got, want)
		}
	}
	for _, bad := range []string{"", "kern.memorystatus_level: 62\n", "kern.memorystatus_level: 140\nhw.memsize: 17179869184\n", "kern.memorystatus_level: x\nhw.memsize: 1\n"} {
		if _, err := ParseSysctlMemory(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func TestDarwinMemoryPrefersSysctlAndFallsBackToVMStat(t *testing.T) {
	sysctl := func() (string, error) { return "kern.memorystatus_level: 50\nhw.memsize: 17179869184\n", nil }
	vmStatCalled := false
	vmStat := func() (string, error) { vmStatCalled = true; return vmStatReal, nil }
	got, ok := DarwinMemory(sysctl, vmStat)
	if !ok || got != 8*GiB || vmStatCalled {
		t.Fatalf("got %d, %v (vm_stat called: %v); want 8 GiB from sysctl alone", got, ok, vmStatCalled)
	}
	failing := func() (string, error) { return "", errors.New("sysctl unavailable") }
	got, ok = DarwinMemory(failing, vmStat)
	if want := uint64(3349+239455) * 16384; !ok || got != want {
		t.Fatalf("fallback got %d, %v; want %d from vm_stat", got, ok, want)
	}
	if _, ok := DarwinMemory(failing, failing); ok {
		t.Fatal("memory must be unknown when neither source can be read")
	}
}

func TestParseMeminfo(t *testing.T) {
	got, err := ParseMeminfo("MemTotal:       16000000 kB\nMemFree:         1000000 kB\nMemAvailable:    6000000 kB\nInactive:        2000000 kB\n")
	if err != nil || got != 6000000*1024 {
		t.Fatalf("got %d, %v; want MemAvailable", got, err)
	}
	got, err = ParseMeminfo("MemTotal: 16000000 kB\nMemFree: 1000000 kB\nInactive: 2000000 kB\n")
	if err != nil || got != 3000000*1024 {
		t.Fatalf("got %d, %v; want MemFree plus Inactive on an old kernel", got, err)
	}
	if _, err := ParseMeminfo("garbage\n"); err == nil {
		t.Fatal("meminfo without MemFree must fail")
	}
}
