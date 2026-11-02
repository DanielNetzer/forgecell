package delivery

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// MaxFixRounds bounds the fix rounds recorded on one change request.
const MaxFixRounds = 3

// MaxFixRoundFindings bounds the finding fingerprints one fix round addresses.
const MaxFixRoundFindings = 500

// maxFixRoundTimeBytes bounds a fix round's pushedAt timestamp.
const maxFixRoundTimeBytes = 64

var (
	fixRoundSHA    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	fixRoundDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// FixRound records one commit a fix round pushed to an already-published
// change request. Receipt.Commit stays the first published commit; the last
// fix round names the current head.
type FixRound struct {
	Round  int    `json:"round"`
	Parent string `json:"parent"`
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
	// PlanDigest is the approved revision's digest.
	PlanDigest string `json:"planDigest"`
	// VerificationTree is the Molecule's verified source tree the push was bound to.
	VerificationTree string   `json:"verificationTree"`
	Findings         []string `json:"findings"`
	PushedAt         string   `json:"pushedAt"`
	// ObservedHead is the change request's head re-read after the push.
	ObservedHead string `json:"observedHead"`
}

// ReceiptHead returns the change request's current head: the last fix round's
// commit and tree, otherwise the first published commit and the reviewed tree.
func ReceiptHead(r Receipt) (commit, tree string) {
	if n := len(r.FixRounds); n > 0 {
		return r.FixRounds[n-1].Commit, r.FixRounds[n-1].Tree
	}
	return r.Commit, r.Preview.Tree
}

// verifiedTree is the Molecule source tree the current head is bound to: the
// last fix round's verification tree, otherwise the reviewed tree.
func verifiedTree(r Receipt) string {
	if n := len(r.FixRounds); n > 0 {
		return r.FixRounds[n-1].VerificationTree
	}
	return r.Preview.Tree
}

// receiptCommits lists every commit the receipt records, first published
// commit first, in push order.
func receiptCommits(r Receipt) []string {
	out := []string{r.Commit}
	for _, fr := range r.FixRounds {
		out = append(out, fr.Commit)
	}
	return out
}

// checkFixRound validates one fix round against the round number and parent it
// must carry and the commits recorded before it.
func checkFixRound(fr FixRound, round int, parent string, earlier []string) error {
	if round < 1 || round > MaxFixRounds {
		return fmt.Errorf("a change request has at most %d fix rounds", MaxFixRounds)
	}
	if fr.Round != round {
		return fmt.Errorf("fix round must be round %d", round)
	}
	if fr.Parent != parent {
		return fmt.Errorf("fix round parent is not the change request's current head")
	}
	if !fixRoundSHA.MatchString(fr.Commit) || !fixRoundSHA.MatchString(fr.Tree) {
		return fmt.Errorf("fix round commit and tree must be 40 lower-case hex characters")
	}
	if !fixRoundSHA.MatchString(fr.VerificationTree) {
		return fmt.Errorf("fix round verification tree must be 40 lower-case hex characters")
	}
	for _, c := range earlier {
		if fr.Commit == c {
			return fmt.Errorf("fix round commit was already recorded on this change request")
		}
	}
	if fr.ObservedHead != fr.Commit {
		return fmt.Errorf("the change request's observed head is not the pushed commit")
	}
	if !fixRoundDigest.MatchString(fr.PlanDigest) {
		return fmt.Errorf("fix round plan digest must be 64 lower-case hex characters")
	}
	if len(fr.PushedAt) > maxFixRoundTimeBytes {
		return fmt.Errorf("fix round push time is not an RFC 3339 time")
	}
	if _, err := time.Parse(time.RFC3339Nano, fr.PushedAt); err != nil {
		return fmt.Errorf("fix round push time is not an RFC 3339 time")
	}
	if len(fr.Findings) > MaxFixRoundFindings {
		return fmt.Errorf("a fix round addresses at most %d findings", MaxFixRoundFindings)
	}
	seen := make(map[string]bool, len(fr.Findings))
	for _, f := range fr.Findings {
		if !fingerprintHex.MatchString(f) {
			return fmt.Errorf("fix round findings must be finding fingerprints")
		}
		if seen[f] {
			return fmt.Errorf("fix round findings must be unique")
		}
		seen[f] = true
	}
	return nil
}

// checkFixRounds validates a receipt's whole fix-round history: a published
// receipt, at most MaxFixRounds rounds numbered from 1, each on the previous
// head. A receipt without fix rounds always passes.
func checkFixRounds(r Receipt) error {
	if len(r.FixRounds) == 0 {
		return nil
	}
	if r.State != "published" || !fixRoundSHA.MatchString(r.Commit) {
		return fmt.Errorf("fix rounds are recorded only on a published delivery with an exact commit")
	}
	if len(r.FixRounds) > MaxFixRounds {
		return fmt.Errorf("a change request has at most %d fix rounds", MaxFixRounds)
	}
	earlier := []string{r.Commit}
	for i, fr := range r.FixRounds {
		if err := checkFixRound(fr, i+1, earlier[len(earlier)-1], earlier); err != nil {
			return err
		}
		earlier = append(earlier, fr.Commit)
	}
	return nil
}

// AppendFixRound records one fix round on a published delivery receipt under
// the delivery lock. It refuses unless the receipt is published, the round is
// the next one and at most MaxFixRounds, the parent is the current head, the
// commit and tree are exact, the observed head is the pushed commit, the plan
// digest is exact and the findings are unique fingerprints. Nothing here pushes
// or contacts a forge.
func AppendFixRound(o Options, fr FixRound) (Receipt, error) {
	if !safeID.MatchString(o.MoleculeID) {
		return Receipt{}, fmt.Errorf("invalid Molecule id")
	}
	lock := filepath.Join(o.LabDir, ".delivery-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return Receipt{}, fmt.Errorf("delivery/fix round conflict: another delivery operation holds the lock")
	}
	defer os.Remove(lock)
	f, found, err := openReceipt(o.LabDir, o.MoleculeID)
	if f == nil {
		if err == nil && !found {
			err = fmt.Errorf("no delivery receipt exists for this Molecule")
		}
		return Receipt{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxReceiptBytes+1))
	f.Close()
	if err != nil || len(raw) > maxReceiptBytes {
		return Receipt{}, fmt.Errorf("delivery receipt is not a bounded regular file")
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return Receipt{}, fmt.Errorf("malformed delivery receipt")
	}
	if err := checkReceipt(o.MoleculeID, r.Preview.MoleculeID, r.State); err != nil {
		return Receipt{}, err
	}
	if r.State != "published" || !fixRoundSHA.MatchString(r.Commit) {
		return Receipt{}, fmt.Errorf("fix rounds are recorded only on a published delivery with an exact commit")
	}
	if err := checkFixRounds(r); err != nil {
		return Receipt{}, fmt.Errorf("recorded fix rounds are invalid: %w", err)
	}
	if len(r.FixRounds) >= MaxFixRounds {
		return Receipt{}, fmt.Errorf("a change request has at most %d fix rounds", MaxFixRounds)
	}
	head, _ := ReceiptHead(r)
	if err := checkFixRound(fr, len(r.FixRounds)+1, head, receiptCommits(r)); err != nil {
		return Receipt{}, err
	}
	fr.Findings = append([]string{}, fr.Findings...)
	r.FixRounds = append(r.FixRounds, fr)
	if err := writeReceipt(o, r); err != nil {
		return Receipt{}, err
	}
	return r, nil
}

// HeadCI keeps the CI observations recorded for head, in order, so that a
// classification never reports an earlier commit's result as current.
func HeadCI(history []CIObservation, head string) []CIObservation {
	out := []CIObservation{}
	for _, a := range history {
		if a.Identity.Commit == head {
			out = append(out, a)
		}
	}
	return out
}
