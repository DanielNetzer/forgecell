// Package fixround is the fix-round service the CLI and the Lab UI share:
// collecting a findings round, proposing the next fix round from it, and
// previewing and pushing that round through the Molecule's own forge. Findings
// are untrusted evidence: nothing here approves a plan or pushes without the
// caller's exact approval.
package fixround

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// MaxForgeLedgerBytes bounds the ledger read that finds a Molecule's forge.
const MaxForgeLedgerBytes = 8_000_000

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Options are the seams and inputs a caller may set; every zero field keeps
// the default. LookupEnv reads the environment plugin processes may receive
// (os.LookupEnv). Formula is the Lab's active approved Formula when the caller
// already loaded it; Propose loads it otherwise. ReadRecord reads the Molecule
// ledger (molecule.ReadRecord) and Amend records the fix round
// (AmendThroughIssue). Now is the collection clock (time.Now).
type Options struct {
	LookupEnv  func(string) (string, bool)
	Formula    *formula.Formula
	ReadRecord func(lab, id string) (molecule.Record, error)
	Amend      func(ctx context.Context, lab string, r molecule.Record, req molecule.FixRoundRequest) (molecule.Record, error)
	Now        func() time.Time
}

func options(opts []Options) Options {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.ReadRecord == nil {
		o.ReadRecord = molecule.ReadRecord
	}
	if o.Amend == nil {
		lookupEnv := o.LookupEnv
		o.Amend = func(ctx context.Context, lab string, r molecule.Record, req molecule.FixRoundRequest) (molecule.Record, error) {
			return AmendThroughIssue(ctx, lab, r, req, lookupEnv)
		}
	}
	return o
}

// SnapshotFormula returns the Formula snapshot a Molecule ran with when it
// declares a plugin forge (true), or false for GitHub. Only an invalid id, a
// missing ledger, or a ledger with no Formula snapshot or no forge block is
// GitHub, whose path reports them as it always has. A ledger that is present
// but unreadable, not a regular file, oversized or malformed, or whose Formula
// snapshot does not parse, is an error: forge selection never fails open to
// GitHub.
func SnapshotFormula(lab, id string) (formula.Formula, bool, error) {
	if !safeID.MatchString(id) {
		return formula.Formula{}, false, nil
	}
	unreadable := fmt.Errorf("the Molecule ledger cannot be read, so its forge is unknown; nothing was published or collected")
	file := filepath.Join(lab, "ledgers", id+".json")
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return formula.Formula{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return formula.Formula{}, false, unreadable
	}
	if info.Size() > MaxForgeLedgerBytes {
		return formula.Formula{}, false, fmt.Errorf("the Molecule ledger exceeds its size bound, so its forge is unknown; nothing was published or collected")
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return formula.Formula{}, false, unreadable
	}
	defer f.Close()
	if stat, err := f.Stat(); err != nil || !os.SameFile(info, stat) {
		return formula.Formula{}, false, unreadable
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxForgeLedgerBytes+1))
	if err != nil {
		return formula.Formula{}, false, unreadable
	}
	if len(raw) > MaxForgeLedgerBytes {
		return formula.Formula{}, false, fmt.Errorf("the Molecule ledger exceeds its size bound, so its forge is unknown; nothing was published or collected")
	}
	var ledger struct {
		FormulaSnapshot struct {
			YAML string `json:"yaml"`
		} `json:"formulaSnapshot"`
	}
	if json.Unmarshal(raw, &ledger) != nil {
		return formula.Formula{}, false, fmt.Errorf("the Molecule ledger is malformed, so its forge is unknown; nothing was published or collected")
	}
	if ledger.FormulaSnapshot.YAML == "" {
		return formula.Formula{}, false, nil
	}
	parsed, err := formula.Parse([]byte(ledger.FormulaSnapshot.YAML))
	if err != nil {
		return formula.Formula{}, false, fmt.Errorf("the Molecule ledger's Formula snapshot does not parse, so its forge is unknown; nothing was published or collected")
	}
	if parsed.Forge == nil {
		return formula.Formula{}, false, nil
	}
	return parsed, true, nil
}

// PluginForge selects the Formula's forge plugin for the Lab, with a call log
// the delivery records keep; every observer also receives each call record.
func PluginForge(lab string, f formula.Formula, lookupEnv func(string) (string, bool), observers ...func(mcpadapter.CallRecord)) (forge.Changes, delivery.PluginForge, error) {
	abs, err := filepath.Abs(lab)
	if err != nil {
		return nil, delivery.PluginForge{}, fmt.Errorf("the Lab directory cannot be resolved")
	}
	// The adapter needs the Lab's real path.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, delivery.PluginForge{}, fmt.Errorf("the Lab directory cannot be resolved")
	}
	calls := &delivery.PluginCallLog{}
	observe := calls.Observe
	if len(observers) > 0 {
		observe = func(rec mcpadapter.CallRecord) {
			calls.Observe(rec)
			for _, o := range observers {
				if o != nil {
					o(rec)
				}
			}
		}
	}
	changes, ok, err := mcpadapter.ForgeFor(resolved, f, mcpadapter.Options{LookupEnv: lookupEnv, Observer: observe})
	if err != nil {
		return nil, delivery.PluginForge{}, err
	}
	if !ok {
		return nil, delivery.PluginForge{}, fmt.Errorf("the Formula declares no forge plugin")
	}
	p, project, _ := f.ForgePlugin()
	return changes, delivery.PluginForge{Name: p.Name, Project: project, Calls: calls}, nil
}

// MoleculeForge builds the Molecule's forge from its ledger's Formula
// snapshot, never the Lab's active Formula: its forge plugin (true), or false
// with no forge for GitHub. An unusable ledger is an error, never GitHub.
func MoleculeForge(lab, id string, lookupEnv func(string) (string, bool), observers ...func(mcpadapter.CallRecord)) (forge.Changes, delivery.PluginForge, bool, error) {
	f, plugin, err := SnapshotFormula(lab, id)
	if err != nil || !plugin {
		return nil, delivery.PluginForge{}, false, err
	}
	changes, meta, err := PluginForge(lab, f, lookupEnv, observers...)
	if err != nil {
		return nil, delivery.PluginForge{}, false, err
	}
	return changes, meta, true, nil
}

// Collect starts the Formula's reviewers, and its forge plugin when it
// declares one, with one call log, and collects a findings round. It refuses a
// demo Lab before any plugin starts.
func Collect(ctx context.Context, lab, id string, f formula.Formula, opts ...Options) (delivery.FindingsRecord, error) {
	if err := delivery.RefuseDemoLab(lab); err != nil {
		return delivery.FindingsRecord{}, err
	}
	o := options(opts)
	refs := f.Reviewers()
	calls := &delivery.PluginCallLog{}
	mo := mcpadapter.Options{LookupEnv: o.LookupEnv, Observer: calls.Observe}
	list, err := mcpadapter.ReviewersFor(lab, f, mo)
	if err != nil {
		return delivery.FindingsRecord{}, err
	}
	if len(list) != len(refs) {
		return delivery.FindingsRecord{}, fmt.Errorf("the Formula's reviewers could not be started")
	}
	reviewers := make([]delivery.FindingsReviewer, len(list))
	for i, r := range list {
		reviewers[i] = delivery.FindingsReviewer{Plugin: refs[i].Plugin, Reviewer: r}
	}
	fo := delivery.FindingsOptions{LabDir: lab, MoleculeID: id, Repo: f.Intake.Repo, Reviewers: reviewers, Calls: calls, Now: o.Now}
	if c, ok, err := mcpadapter.ForgeFor(lab, f, mo); err != nil {
		return delivery.FindingsRecord{}, err
	} else if ok {
		p, project, declared := f.ForgePlugin()
		if !declared {
			return delivery.FindingsRecord{}, fmt.Errorf("the Formula's forge plugin is not declared")
		}
		fo.Changes, fo.ForgePlugin, fo.ForgeProject = c, p.Name, project
	}
	return delivery.Collect(ctx, fo)
}

// Preview is the fix round's push preview. It starts no plugin.
func Preview(ctx context.Context, lab, id, title string) (delivery.FixRoundPreview, error) {
	return delivery.PreviewFixRound(ctx, delivery.FixRoundPushOptions{LabDir: lab, MoleculeID: id, Title: title})
}

// Push pushes exactly the approved fix round through the Molecule's forge;
// without an approval it returns *delivery.FixRoundApprovalRequired with the
// preview, and starts no plugin.
func Push(ctx context.Context, lab, id, approve, title string, confirm bool, opts ...Options) (delivery.Receipt, error) {
	return PushWith(ctx, delivery.FixRoundPushOptions{LabDir: lab, MoleculeID: id, Approve: approve, Title: title, Confirm: confirm}, opts...)
}

// PushWith is Push for full push options: with an approval, the Molecule's
// forge from its ledger's Formula snapshot replaces Changes and Plugin, and
// GitHub keeps them unset.
func PushWith(ctx context.Context, po delivery.FixRoundPushOptions, opts ...Options) (delivery.Receipt, error) {
	if po.Approve != "" {
		o := options(opts)
		changes, meta, plugin, err := MoleculeForge(po.LabDir, po.MoleculeID, o.LookupEnv)
		if err != nil {
			return delivery.Receipt{}, err
		}
		if plugin {
			po.Changes, po.Plugin = changes, meta
		}
	}
	return delivery.PushFixRound(ctx, po)
}

// AmendThroughIssue records the fix round, re-reading the Issue through the
// Molecule's own reader: the tracker plugin of its approved Formula for a
// keyed Molecule, else GitHub.
func AmendThroughIssue(ctx context.Context, lab string, r molecule.Record, req molecule.FixRoundRequest, lookupEnv func(string) (string, bool)) (molecule.Record, error) {
	o := molecule.Options{LabDir: lab}
	if r.Issue.IsKeyed() {
		loaded, err := formula.Load(lab, r.FormulaID)
		if err != nil || !loaded.Approved {
			return molecule.Record{}, fmt.Errorf("a keyed fix round needs the Molecule's approved Formula to read its Issue")
		}
		if _, ok := loaded.Formula.TrackerPlugin(); !ok {
			return molecule.Record{}, fmt.Errorf("a keyed fix round needs a Formula that reads Issues from a tracker plugin")
		}
		calls := molecule.NewPluginCallLog(lab)
		keyed, _, err := mcpadapter.TrackerFor(lab, loaded.Formula, mcpadapter.Options{LookupEnv: lookupEnv, Observer: calls.Observe})
		if err != nil {
			return molecule.Record{}, err
		}
		o.ReadIssueKey, o.PluginCalls = molecule.KeyedReader(keyed, calls), calls
	}
	return molecule.AmendFixRound(ctx, o, req)
}
