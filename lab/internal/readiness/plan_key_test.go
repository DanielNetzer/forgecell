package readiness

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func keyedFixture() Plan {
	p := fixture()
	p.MoleculeID = "mol-eng-1234-0123456789abcdef01234567"
	p.Inputs.Issue.Number = 0
	p.Inputs.Issue.Key = "ENG-1234"
	p.Inputs.Issue.URL = "https://tracker.example.test/browse/ENG-1234"
	return p
}

func TestKeyedPlanValidatesWithStableDigest(t *testing.T) {
	p := keyedFixture()
	if err := p.Validate(); err != nil {
		t.Fatalf("keyed plan: %v", err)
	}
	first, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	again := keyedFixture()
	again.Inputs.Issue.UpdatedAt = "later"
	if second, err := again.Digest(); err != nil || second != first {
		t.Fatalf("digest changed: %q, %q, %v", first, second, err)
	}
	// The key is bound by the digest.
	other := keyedFixture()
	other.Inputs.Issue.Key = "ENG-1235"
	if d, err := other.Digest(); err != nil || d == first {
		t.Fatalf("digest does not bind the key: %q, %v", d, err)
	}
	github, err := fixture().Digest()
	if err != nil || github == first {
		t.Fatalf("keyed and GitHub digests: %q, %q, %v", first, github, err)
	}
}

// legacyIssue is readiness.Issue as it was before Key was added.
type legacyIssue struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	URL        string `json:"url"`
	State      string `json:"state"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	UpdatedAt  string `json:"updatedAt"`
}

func TestGitHubPlanJSONUnchangedByKey(t *testing.T) {
	i := fixture().Inputs.Issue
	got, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(legacyIssue{i.Repository, i.Number, i.URL, i.State, i.Title, i.Body, i.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GitHub Issue JSON changed:\n%s\nwant\n%s", got, want)
	}
	raw, err := json.Marshal(fixture())
	if err != nil || bytes.Contains(raw, []byte(`"key"`)) {
		t.Fatalf("GitHub plan JSON carries a key: %v", err)
	}
	if err := fixture().Validate(); err != nil {
		t.Fatalf("GitHub plan: %v", err)
	}
}

func TestKeyedPlanRefusals(t *testing.T) {
	cases := map[string]func(*Plan){
		"key with number":     func(p *Plan) { p.Inputs.Issue.Number = 1234 },
		"key with github url": func(p *Plan) { p.Inputs.Issue.Number, p.Inputs.Issue.URL = 1, "https://github.com/owner/repo/issues/1" },
		"http url":            func(p *Plan) { p.Inputs.Issue.URL = "http://tracker.example.test/browse/ENG-1234" },
		"relative url":        func(p *Plan) { p.Inputs.Issue.URL = "/browse/ENG-1234" },
		"empty url":           func(p *Plan) { p.Inputs.Issue.URL = "" },
		"userinfo url":        func(p *Plan) { p.Inputs.Issue.URL = "https://u:p@tracker.example.test/ENG-1234" },
		"overlong url":        func(p *Plan) { p.Inputs.Issue.URL = "https://tracker.example.test/" + strings.Repeat("a", 4096) },
		"control key":         func(p *Plan) { p.Inputs.Issue.Key = "ENG\n1234" },
		"invalid utf-8 key":   func(p *Plan) { p.Inputs.Issue.Key = "ENG-\xff" },
		"overlong key":        func(p *Plan) { p.Inputs.Issue.Key = strings.Repeat("A", 600) },
		"closed":              func(p *Plan) { p.Inputs.Issue.State = "CLOSED" },
		"no key no number":    func(p *Plan) { p.Inputs.Issue.Key = "" },
		"other repository":    func(p *Plan) { p.Inputs.Issue.Repository = "owner/other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := keyedFixture()
			mutate(&p)
			if err := p.Validate(); err == nil || err.Error() != "issue must be open and match the selected repository and identity" {
				t.Fatalf("Validate = %v", err)
			}
			if _, err := p.Digest(); err == nil {
				t.Fatal("digest of an invalid keyed plan")
			}
		})
	}
}
