package intake

import "testing"

func TestIssueReferences(t *testing.T) {
	cases := []struct {
		input string
		want  IssueRef
	}{
		{"12", IssueRef{Number: 12}}, {"#12", IssueRef{Number: 12}},
		{"acme/widgets#12", IssueRef{Number: 12, Owner: "acme", Repo: "widgets"}},
		{"https://github.com/acme/widgets/issues/12#issuecomment-9", IssueRef{Number: 12, Owner: "acme", Repo: "widgets"}},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, err := ParseIssueRef(c.input)
			if err != nil || got != c.want {
				t.Fatalf("got %#v, %v; want %#v", got, err, c.want)
			}
		})
	}
}
func TestInvalidIssueReferences(t *testing.T) {
	for _, input := range []string{"", "not-an-issue", "0", "#0", "acme/widgets#0", "https://github.com/acme/widgets/issues/0", "9007199254740992", "https://example.com/acme/widgets/issues/12", "acme/../widgets#12"} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseIssueRef(input); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
func TestBindRepository(t *testing.T) {
	ref, err := BindRepository(IssueRef{Number: 12}, "acme/widgets")
	if err != nil || ref.Owner != "acme" || ref.Repo != "widgets" {
		t.Fatalf("%+v %v", ref, err)
	}
	if _, err := BindRepository(IssueRef{Number: 12, Owner: "other", Repo: "repo"}, "acme/widgets"); err == nil {
		t.Fatal("repository mismatch must be explicit")
	}
	if _, err := BindRepository(IssueRef{Number: 12}, "acme/widgets/extra"); err == nil {
		t.Fatal("invalid binding accepted")
	}
}
