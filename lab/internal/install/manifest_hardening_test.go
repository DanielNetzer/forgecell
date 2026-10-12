package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

const builtProtocolFixture = "fixture-protocol-5"

func releaseDir(o Options) string { return filepath.Join(o.Home, "releases", o.Version) }

func rawManifest(t *testing.T, o Options) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(releaseDir(o), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeRawManifest(t *testing.T, o Options, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(releaseDir(o), "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// protocolFixture is a release executable that answers --protocol with protocol, or
// fails it when protocol is empty, as a build that predates the flag does.
func protocolFixture(t *testing.T, version, protocol string) string {
	t.Helper()
	answer := "exit 2"
	if protocol != "" {
		answer = "printf '%s\\n' '" + protocol + "'"
	}
	file := filepath.Join(t.TempDir(), "forgecell")
	script := "#!/bin/sh\ncase \"$1\" in\n--protocol) " + answer + ";;\n*) printf '%s\\n' '" + version + "';;\nesac\n"
	if err := os.WriteFile(file, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestStagedManifestCarriesKindAndRoundTripsStrictReader(t *testing.T) {
	o := Options{Home: t.TempDir(), Bin: t.TempDir(), Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	raw := rawManifest(t, o)
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"kind", "protocolVersion", "sha256", "version"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("manifest keys = %v, want %v", keys, want)
	}
	if fields["kind"] != harness.ReleaseKind {
		t.Fatalf("kind = %v", fields["kind"])
	}
	m := harness.ReadManifest(releaseDir(o))
	if !m.Found || m.Version != "0.2.0" || m.Protocol != harness.AdapterProtocolVersion || len(m.Digest) != 64 || m.SHA256 != fields["sha256"] {
		t.Fatalf("the strict reader rejects the installer's manifest: %+v", m)
	}
	id := harness.ResolveLauncher(filepath.Join(releaseDir(o), "forgecell"))
	if id.Class != harness.LauncherCompatible || !id.ExecutableMatchesManifest() {
		t.Fatalf("an installed release is not established: %+v", id)
	}
}

func TestSameVersionReinstallBackfillsProtocolAndKind(t *testing.T) {
	if builtProtocolFixture == harness.AdapterProtocolVersion {
		t.Fatal("the built protocol must differ from the running installer's")
	}
	for name, tc := range map[string]struct {
		old          map[string]any
		built        string
		explicit     string
		wantKind     string
		wantProtocol string
	}{
		"protocol is the built binary's, not the installer's": {map[string]any{}, builtProtocolFixture, "", harness.ReleaseKind, builtProtocolFixture},
		"kind without protocolVersion":                        {map[string]any{"kind": harness.ReleaseKind}, builtProtocolFixture, "", harness.ReleaseKind, builtProtocolFixture},
		"protocolVersion without kind keeps its protocol":     {map[string]any{"protocolVersion": "1"}, builtProtocolFixture, "", harness.ReleaseKind, "1"},
		"protocol cannot be established":                      {map[string]any{}, "", "", harness.ReleaseKind, ""},
		"kind present and protocol cannot be established":     {map[string]any{"kind": harness.ReleaseKind}, "", "", harness.ReleaseKind, ""},
		"explicit protocol of a dev build":                    {map[string]any{}, builtProtocolFixture, "explicit-3", harness.ReleaseKind, "explicit-3"},
	} {
		t.Run(name, func(t *testing.T) {
			o := Options{Home: t.TempDir(), Bin: t.TempDir(), Source: protocolFixture(t, "0.2.0", tc.built), Version: "0.2.0"}
			if _, err := Install(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			installed := readManifest(t, o.Home, "0.2.0")
			fields := map[string]any{"version": installed.Version, "sha256": installed.SHA256}
			for k, v := range tc.old {
				fields[k] = v
			}
			writeRawManifest(t, o, fields)
			if m := harness.ReadManifest(releaseDir(o)); !m.Found || (tc.old["protocolVersion"] == nil) != (m.Protocol == "unknown") {
				t.Fatalf("the fixture must read as an incomplete release manifest: %+v", m)
			}

			o.Protocol = tc.explicit
			if _, err := Install(context.Background(), o); err != nil {
				t.Fatalf("a same-version reinstall of identical content must succeed: %v", err)
			}
			got := readManifest(t, o.Home, "0.2.0")
			if got.Kind != tc.wantKind || got.ProtocolVersion != tc.wantProtocol || got.Version != installed.Version || got.SHA256 != installed.SHA256 {
				t.Fatalf("manifest was not backfilled as expected: %+v", got)
			}
			wantRead := tc.wantProtocol
			if wantRead == "" {
				wantRead = "unknown"
			}
			if m := harness.ReadManifest(releaseDir(o)); !m.Found || m.Protocol != wantRead {
				t.Fatalf("the backfilled manifest reads as %+v, want protocol %s", m, wantRead)
			}
			entries, err := os.ReadDir(releaseDir(o))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".") {
					t.Fatalf("backfill left %s behind", e.Name())
				}
			}
			if st, err := os.Stat(filepath.Join(releaseDir(o), "manifest.json")); err != nil || st.Mode().Perm() != 0600 {
				t.Fatalf("manifest mode: %v %v", st, err)
			}
			before := rawManifest(t, o)
			if _, err = Install(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if string(rawManifest(t, o)) != string(before) {
				t.Fatal("a manifest with nothing left to backfill was rewritten")
			}
		})
	}
}

func TestSameVersionReinstallWithDifferentContentStillRefused(t *testing.T) {
	o := Options{Home: t.TempDir(), Bin: t.TempDir(), Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	installed := readManifest(t, o.Home, "0.2.0")
	writeRawManifest(t, o, map[string]any{"version": installed.Version, "sha256": installed.SHA256})
	old := rawManifest(t, o)

	different := o
	different.Source = filepath.Join(t.TempDir(), "forgecell")
	if err := os.WriteFile(different.Source, []byte("#!/bin/sh\n# other build\nprintf '%s\\n' '0.2.0'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), different); err == nil || !strings.Contains(err.Error(), "already exists with different content") {
		t.Fatalf("different content under the same version was accepted: %v", err)
	}
	if string(rawManifest(t, o)) != string(old) {
		t.Fatal("a refused reinstall rewrote the manifest")
	}

	writeRawManifest(t, o, map[string]any{"version": installed.Version, "sha256": installed.SHA256, "protocolVersion": "1"})
	recorded := rawManifest(t, o)
	explicit := o
	explicit.Protocol = "9"
	if _, err := Install(context.Background(), explicit); err == nil || !strings.Contains(err.Error(), "already exists with different content") {
		t.Fatalf("a release recorded under protocol 1 was accepted under protocol 9: %v", err)
	}
	if string(rawManifest(t, o)) != string(recorded) {
		t.Fatal("a refused reinstall rewrote the manifest")
	}
}

func TestVerifyRejectsMalformedProtocolVersion(t *testing.T) {
	o := Options{Home: t.TempDir(), Bin: t.TempDir(), Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	installed := readManifest(t, o.Home, "0.2.0")
	previous := o
	o.Source, o.Version = fixture(t, "0.2.1"), "0.2.1"
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		fields map[string]any
		valid  bool
	}{
		"valid protocol":   {map[string]any{"protocolVersion": harness.AdapterProtocolVersion}, true},
		"absent protocol":  {map[string]any{}, true},
		"release kind":     {map[string]any{"kind": harness.ReleaseKind, "protocolVersion": harness.AdapterProtocolVersion}, true},
		"foreign kind":     {map[string]any{"kind": "other-tool", "protocolVersion": harness.AdapterProtocolVersion}, false},
		"shell characters": {map[string]any{"protocolVersion": "1; rm"}, false},
		"leading dot":      {map[string]any{"protocolVersion": ".1"}, false},
		"too long":         {map[string]any{"protocolVersion": strings.Repeat("1", 33)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			fields := map[string]any{"version": installed.Version, "sha256": installed.SHA256}
			for k, v := range tc.fields {
				fields[k] = v
			}
			writeRawManifest(t, previous, fields)
			_, err := verify(releaseDir(previous))
			if tc.valid != (err == nil) {
				t.Fatalf("verify = %v, want valid=%t", err, tc.valid)
			}
			_, rollbackErr := Rollback(context.Background(), o)
			target, _ := os.Readlink(filepath.Join(o.Home, "current"))
			if tc.valid {
				if rollbackErr != nil {
					t.Fatalf("rollback to a verifiable release: %v", rollbackErr)
				}
				if _, err = Install(context.Background(), o); err != nil {
					t.Fatal(err)
				}
				return
			}
			if rollbackErr == nil || filepath.Base(target) != "0.2.1" {
				t.Fatalf("rollback to a malformed manifest: err=%v current=%s", rollbackErr, target)
			}
		})
	}
}
