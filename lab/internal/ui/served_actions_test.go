package ui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// Every gate's main action is a primary button, Retire sits in the separate
// Danger zone behind its own open step, a reason and Confirm retire, and the
// "What you can do" actions are primary buttons posting the shown digest.
func TestUIServedJSGateActionsArePrimaryButtons(t *testing.T) {
	js := servedJS(t)
	for _, want := range []string{
		"c.approve = submitButton({ class: 'btn btn-primary btn-decision'",
		"c.preview = el('button', { type: 'button', class: 'btn btn-primary btn-decision'",
		"c.publish = submitButton({ class: 'btn btn-primary btn-decision'",
		// Retire: open, then a reason and Confirm retire, in the Danger zone.
		"onclick: () => openConfirm('retire') }, 'Retire this Molecule…')",
		"c.retireSubmit = submitButton({ class: 'btn btn-danger', dataset: { gate: 'retire' } }, 'Confirm retire');",
		"field('mv-retire-reason', 'Reason', c.reasonInput)",
		"el('details', { class: 'danger-zone', id: 'mv-danger', hidden: true }",
		"fill($('#mv-danger-slot'), c.runnerNote, c.danger);",
		// What you can do.
		"'Why it’s blocked · What you can do'",
		"const button = submitButton({ class: 'btn btn-primary btn-decision', dataset: { action: a.kind } }",
		"if (a.kind === 'retire') return el('p', { class: 'drive-help' }, label + ': ' + copy);",
		"'/api/molecules/' + encodeURIComponent(d.id) + '/actions/' + encodeURIComponent(a.kind)",
		"let body = { digest: a.digest };",
		"if (a.kind === 'fresh-intake') { path = '/api/runs'; body = { issue: a.issue }; }",
		"'I confirm it is stopped: no Lab process works on this Molecule'",
		// Accept-failures binds the exact tests listed; scope-change posts the
		// ledger's paths; an action not offered is named with its reason.
		"if (a.kind === 'accept-failures') body.deltas = a.deltasDigest;",
		"body.paths = a.paths;",
		"' is not offered: ' + plainText(a.reason, '')",
		"res = await postJSON(path, JSON.stringify(body));",
		"'The response was lost or ambiguous. The Molecule is read back; nothing is resubmitted.'",
		"It waits for your approval: click Approve plan & start coding. Nothing was approved.",
		// Bulk retire: the exact count and list, then Confirm retire.
		"'Confirm retire ' + items.length",
		"JSON.stringify({ count: list.count, digest: list.digest })",
		"'Each is retired with the reason “Issue closed; superseded attempt”. Every record is kept.'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the served JS lacks %q", want)
		}
	}
	// The actions panel never posts an approval, and Retire is never a button
	// beside a primary action.
	item := moduleFunction(t, js, "actionItem")
	if strings.Contains(item, "btn-danger") || strings.Contains(item, "/scope") {
		t.Error("an action in What you can do is a danger button or approves")
	}
	submit := moduleFunction(t, js, "submitAction")
	if strings.Contains(submit, "approve") || strings.Contains(submit, "/scope") {
		t.Error("submitAction approves")
	}
}

// The served page and scripts no longer send the user to the CLI for a
// decision: the overview names the button, the panel and the Danger zone have
// their own blocks, and Next action names the button to click.
func TestUIDecisionCopyNamesTheButton(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	page := do(t, h, authed("/")).Body.String()
	js := servedJS(t)
	for _, text := range []string{page, js} {
		for _, gone := range []string{"Decide with the forgecell CLI", "or with the forgecell CLI", "decide with the forgecell CLI"} {
			if strings.Contains(text, gone) {
				t.Errorf("served copy still says %q", gone)
			}
		}
	}
	for _, want := range []string{`<section id="mv-actions" class="decision-card mv-actions" aria-labelledby="mv-actions-title" hidden></section>`, `<div id="mv-danger-slot"></div>`, "Open one and click its button"} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	if strings.Index(page, `id="mv-actions"`) > strings.Index(page, `id="mv-danger-slot"`) {
		t.Error("the Danger zone is not below What you can do")
	}

	lab, r, digest := gateFixture(t)
	hl := testHandler(t, Options{LabDir: lab})
	d := detailOf(t, hl, r.ID)
	if !strings.Contains(d.Next.Copy, "click Approve plan & start coding below") || strings.Contains(d.Next.Copy, "CLI") || d.View.Decision.Primary == nil || d.View.Decision.Primary.Label != "Approve plan & start coding" || d.View.Decision.Primary.Digest != digest {
		t.Fatalf("scope gate next %+v decision %+v", d.Next, d.View.Decision)
	}
	// Ready for a draft PR names Preview delivery and Publish draft PR.
	review := r
	review.Status, review.Readiness = "completed", &readiness.State{SchemaVersion: "v1", Phase: "review-waiting", Plans: r.Readiness.Plans}
	writeRecord(t, lab, review)
	var dv Detail
	decode(t, do(t, hl, authed("/api/molecules/"+r.ID)), &dv)
	if dv.Gate.State == GateDelivery && (!strings.Contains(dv.Next.Copy, "Click Preview delivery below") || strings.Contains(dv.Next.Copy, "CLI")) {
		t.Fatalf("delivery gate next %+v", dv.Next)
	}
	// A gate this page does not perform points at What you can do, not the CLI.
	for _, copy := range []string{decisionView(review, LaneDecision, "", GateView{}, nil).Copy} {
		if strings.Contains(copy, "CLI") || !strings.Contains(copy, "What you can do") {
			t.Errorf("decision copy %q", copy)
		}
	}
	if w := do(t, hl, authed("/api/molecules/"+r.ID+"/actions")); w.Code != http.StatusOK {
		t.Fatalf("actions view: %d", w.Code)
	}
}
