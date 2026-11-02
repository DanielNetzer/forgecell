package reviewer

import (
	"encoding/json"
	"go/build"
	"strings"
	"testing"
)

func TestDescriptionHasMatchesExactly(t *testing.T) {
	d := Description{DisplayName: "Critic", Capabilities: []string{CapabilityComments}}
	if !d.Has(CapabilityComments) || d.Has(CapabilityChecks) || d.Has("Forge-Comments") || d.Has("forge-comments ") {
		t.Fatalf("Has is not an exact match: %+v", d)
	}
	if (Description{}).Has(CapabilityComments) {
		t.Fatal("an empty description lists a capability")
	}
}

func TestCheckMirrorsTheForgeChecksItemShape(t *testing.T) {
	raw, err := json.Marshal(Check{Name: "build", Status: "completed", Conclusion: "success"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"name":"build","status":"completed","conclusion":"success"}` {
		t.Fatalf("a check without a URL encodes as %s", raw)
	}
	raw, _ = json.Marshal(Check{Name: "build", Status: "queued", URL: "https://ci.example/1"})
	if string(raw) != `{"name":"build","status":"queued","conclusion":"","url":"https://ci.example/1"}` {
		t.Fatalf("a queued check encodes as %s", raw)
	}
}

func TestSeveritiesAreOrderedMostSevereFirst(t *testing.T) {
	if strings.Join(Severities, ",") != "high,medium,low,info" {
		t.Fatalf("Severities = %v", Severities)
	}
}

// The package imports only the standard library and forge.
func TestImportsOnlyTheStandardLibraryAndForge(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range pkg.Imports {
		if strings.Contains(path, ".") && path != "github.com/DanielNetzer/forgecell/lab/internal/forge" {
			t.Errorf("reviewer imports %s", path)
		}
	}
}
