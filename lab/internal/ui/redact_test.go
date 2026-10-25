package ui

import (
	"strings"
	"testing"
)

// Secrets are assembled at run time so the source holds no token-shaped value.
var (
	fakeGitHub    = "ghp_" + strings.Repeat("A1b2", 9)
	fakeGitHubPAT = "github_pat_" + strings.Repeat("Z9_y", 8)
	fakeAnthropic = "sk-ant-" + "api03-" + strings.Repeat("Qw3-", 6)
	fakeOpenAI    = "sk-proj-" + strings.Repeat("Rt5u", 6)
	fakeSlack     = "xoxb-" + "1234567890-" + strings.Repeat("abCD", 4)
	fakeAWS       = "AKIA" + "IOSFODNN7EXAMPLE"
	fakeGoogle    = "AIza" + strings.Repeat("Sy0_", 8) + "abc"
	fakeBearer    = strings.Repeat("eyJhbGciOi", 3)
)

func TestUIRedactSecretPatterns(t *testing.T) {
	for _, tc := range []struct {
		name, in, secret, keep string
	}{
		{"GitHub token", "token " + fakeGitHub + " used", fakeGitHub, "token "},
		{"GitHub fine-grained token", "pat=" + fakeGitHubPAT, fakeGitHubPAT, "pat="},
		{"Anthropic key", "key " + fakeAnthropic, fakeAnthropic, "key "},
		{"OpenAI key", "export OPENAI " + fakeOpenAI, fakeOpenAI, "export OPENAI "},
		{"Slack token", "slack: " + fakeSlack, fakeSlack, "slack: "},
		{"AWS access key id", "id " + fakeAWS + " end", fakeAWS, " end"},
		{"Google API key", "g=" + fakeGoogle, fakeGoogle, "g="},
		{"Bearer token in prose", "sent Bearer " + fakeBearer + " to the API", fakeBearer, "sent Bearer "},
		{"env *_KEY", "ANTHROPIC_API_KEY=plainvalue123", "plainvalue123", "ANTHROPIC_API_KEY="},
		{"lowercase *_key", "my_key: hunter2hunter2", "hunter2hunter2", "my_key: "},
		{"JSON *_KEY", `{"STRIPE_SECRET_KEY": "notatokenshape"}`, "notatokenshape", `"STRIPE_SECRET_KEY": "`},
		{"escaped JSON *_KEY", `"{\"SERVICE_KEY\":\"inner-value\"}"`, "inner-value", `SERVICE_KEY`},
		{"*_TOKEN assignment", "GH_TOKEN=shortvalue", "shortvalue", "GH_TOKEN="},
		{"*_KEY_ID assignment", "AWS_ACCESS_KEY_ID=someid99", "someid99", "AWS_ACCESS_KEY_ID="},
		{"*_PASSWORD assignment", "DB_PASSWORD='s3cr3t!'", "s3cr3t!", "DB_PASSWORD='"},
		{"Authorization header", "Authorization: token opaque-value-1\nnext line", "opaque-value-1", "Authorization: "},
		{"Authorization basic", "authorization: Basic dXNlcjpwYXNz\n", "dXNlcjpwYXNz", "authorization: "},
		{"Proxy-Authorization header", "Proxy-Authorization: Digest abc123", "Digest abc123", "Proxy-Authorization: "},
		{"Authorization in JSON", `{"Authorization":"Bearer short"}`, "Bearer short", `{"Authorization":"`},
		{"x-api-key header", "x-api-key: value-of-key", "value-of-key", "x-api-key: "},
		{"URL credentials", "remote https://user:p4ss@github.com/o/r.git", "user:p4ss", "https://"},
		{"PEM private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----\nafter", "MIIEow", "after"},
		{"unterminated PEM private key", "x\n-----BEGIN PRIVATE KEY-----\nMIIEow and more", "MIIEow", "x\n"},
		{"Authorization header array", `{"Authorization":["Basic dXNlcjpwYXNz"]}`, "dXNlcjpwYXNz", `{"Authorization":`},
		{"escaped Authorization header array", `"{\"Authorization\":[\"Basic dXNlcjpwYXNz\"]}"`, "dXNlcjpwYXNz", `Authorization`},
		{"PGP private key block", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBF\n-----END PGP PRIVATE KEY BLOCK-----\nafter", "lQOYBF", "after"},
		{"quoted password key", `{"password": "hunter2"}`, "hunter2", `"password": "`},
		{"camelCase apiKey", `{"apiKey": "abc123def"}`, "abc123def", `"apiKey": "`},
		{"bare PASSWORD", "PASSWORD=opensesame", "opensesame", "PASSWORD="},
		{"token key", `token: "tok-value-9"`, "tok-value-9", "token: "},
		{"quoted value with spaces", `ANTHROPIC_API_KEY = "with space"`, "with space", "ANTHROPIC_API_KEY"},
		{"npm _authToken", "//registry.npmjs.org/:_authToken=npm_abcdefghijklmnopqrstuvwxyz0123456789", "npm_abcdefghijklmnopqrstuvwxyz0123456789", "_authToken="},
		{"Cookie header", "Cookie: session=abc123; theme=dark\nnext", "session=abc123", "Cookie: "},
		{"GitLab token", "glpat-abcdefghijklmnopqrst1234 end", "glpat-abcdefghijklmnopqrst1234", " end"},
		{"bare JWT", "jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N end", "dozjgNryP4J3jVmNHl0w5N", " end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.in)
			if strings.Contains(got, tc.secret) || !strings.Contains(got, secretMark) || !strings.Contains(got, tc.keep) {
				t.Fatalf("%q redacted to %q", tc.in, got)
			}
		})
	}
}

func TestUIRedactLeavesOrdinaryTextUnchanged(t *testing.T) {
	for _, s := range []string{
		"",
		"go test ./internal/ui/... passed in 1.2s",
		"The bearer tokens are rotated; see the key file and the authorization docs.",
		"monkey=banana keyboard=qwerty KEY=value",
		"https://github.com/o/r/pull/5 and sk-short",
		`{"path":"verification-history/m/x.json","sha256":"` + strings.Repeat("ab", 32) + `"}`,
	} {
		if got := redactSecrets(s); got != s {
			t.Errorf("%q changed to %q", s, got)
		}
	}
}

// A secret that starts before the cut is replaced whole, so no prefix of it is
// served; one that starts after the cut is not served at all.
func TestUIRedactSecretAtTheChunkBoundary(t *testing.T) {
	for _, offset := range []int{1, 4, 10, len(fakeGitHub) - 1} {
		s := strings.Repeat("x", 100-offset) + " " + fakeGitHub + " tail"
		got := cutRedacted(s, 101)
		if strings.Contains(got, fakeGitHub[:4]) || !strings.HasSuffix(got, secretMark) || len(got) > 101+len(secretMark) {
			t.Fatalf("offset %d: %q", offset, got)
		}
	}
	header := "line\nAuthorization: Bearer " + fakeBearer + "\n"
	for limit := len("line\nAuthorization: ") + 1; limit < len(header); limit++ {
		if got := cutRedacted(header, limit); strings.Contains(got, "Bearer") || strings.Contains(got, fakeBearer[:3]) {
			t.Fatalf("limit %d served %q", limit, got)
		}
	}
	if got := cutRedacted("ab€cd", 3); got != "ab" {
		t.Fatalf("cut inside a rune: %q", got)
	}
}

func TestUIRedactHostileInput(t *testing.T) {
	// Many overlapping and adjacent secrets, invalid UTF-8 and a huge line stay
	// bounded and leave no secret.
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("Authorization: Bearer " + fakeBearer + " " + fakeGitHub + "\xff\xfe API_KEY=" + fakeAnthropic + "\n")
	}
	b.WriteString(strings.Repeat("A", 1<<20))
	got := redactSecrets(b.String())
	for _, secret := range []string{fakeBearer, fakeGitHub, fakeAnthropic} {
		if strings.Contains(got, secret) {
			t.Fatalf("hostile input kept %q", secret)
		}
	}
	if len(got) > len(b.String()) {
		t.Fatalf("redaction grew the text: %d > %d", len(got), len(b.String()))
	}
}
