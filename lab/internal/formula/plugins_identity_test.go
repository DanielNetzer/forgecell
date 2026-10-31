package formula

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func withForgeIdentity(value string) string {
	return strings.Replace(pluginRecipe, "  project: acme/widgets\n", "  project: acme/widgets\n  identity: "+value+"\n", 1)
}

func TestForgeIdentityAbsentKeepsFormulaBytesAndEncoding(t *testing.T) {
	f, err := Parse([]byte(pluginRecipe))
	if err != nil {
		t.Fatal(err)
	}
	if f.YAML != pluginRecipe || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(pluginRecipe))) {
		t.Fatal("snapshot changed")
	}
	if f.Forge == nil || f.Forge.Identity != "" {
		t.Fatalf("absent identity decoded as present: %+v", f.Forge)
	}
	raw, err := json.Marshal(f.Forge)
	if err != nil || string(raw) != `{"plugin":"gitlab","project":"acme/widgets"}` {
		t.Fatalf("ForgeRef JSON changed: %s %v", raw, err)
	}
}

func TestForgeIdentityValidShapes(t *testing.T) {
	for _, value := range []string{
		"gitlab.com/welltech-com/backend",
		"gitlab.example.co.uk/group/sub-group/project",
		"git.internal.example/a",
		"gitlab.com/g/p.v2",
		"gitlab.com/g_1/p~x",
		"gitlab.com/g.git/p",
		"gitlab.com/g/p.gitx",
		"x.y/" + strings.Repeat("a", MaxForgeIdentity-4),
	} {
		t.Run(value[:min(len(value), 40)], func(t *testing.T) {
			f, err := Parse([]byte(withForgeIdentity(value)))
			if err != nil {
				t.Fatal(err)
			}
			if f.Forge.Identity != value {
				t.Fatalf("identity %q decoded as %q", value, f.Forge.Identity)
			}
			raw, _ := json.Marshal(f.Forge)
			if !strings.Contains(string(raw), `"identity":"`+value+`"`) {
				t.Fatalf("identity missing from JSON: %s", raw)
			}
		})
	}
}

func TestForgeIdentityRefusedShapes(t *testing.T) {
	for name, value := range map[string]string{
		"upper case":      "GitLab.com/group/project",
		"no dot in host":  "gitlab/group/project",
		"host only":       "gitlab.com",
		"trailing slash":  "gitlab.com/group/",
		"empty segment":   "gitlab.com//project",
		"dot dot":         "gitlab.com/../project",
		"dot dot inside":  "gitlab.com/a..b/project",
		"dot segment":     "gitlab.com/./project",
		"query":           `"gitlab.com/group/project?x=1"`,
		"fragment":        `"gitlab.com/group/project#x"`,
		"userinfo":        `"user@gitlab.com/group/project"`,
		"port":            `"gitlab.com:443/group/project"`,
		"scheme":          `"https://gitlab.com/group/project"`,
		"percent":         `"gitlab.com/group/%2e%2e"`,
		"space":           `"gitlab.com/group/my project"`,
		"empty":           `""`,
		"too long":        "x.y/" + strings.Repeat("a", MaxForgeIdentity-3),
		"not a string":    "[gitlab.com/a]",
		"leading hyphen":  "-gitlab.com/a",
		"empty host part": "gitlab..com/a",
		"trailing .git":   "gitlab.com/group/project.git",
		"only .git":       "gitlab.com/group/.git",
		"double .git":     "gitlab.com/group/project.git.git",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(withForgeIdentity(value)))
			if err == nil || !strings.Contains(err.Error(), "forge: identity") {
				t.Fatalf("got %v, want a forge: identity refusal", err)
			}
		})
	}
}

func TestForgeIdentityKeepsTheUnknownKeyMessage(t *testing.T) {
	yaml := withForgeIdentity("gitlab.com/acme/widgets")
	yaml = strings.Replace(yaml, "  identity: gitlab.com/acme/widgets\n", "  identity: gitlab.com/acme/widgets\n  branch: main\n", 1)
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), `forge: unknown key "branch"; use plugin and project`) {
		t.Fatalf("got %v", err)
	}
}
