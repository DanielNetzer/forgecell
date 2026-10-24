package formula

import (
	"os"
	"path/filepath"
)

// Identity states, from the same checks the execution boundary (Load) applies.
const (
	IdentityApproved   = "approved"
	IdentityNone       = "none"
	IdentityLocked     = "activation-locked"
	IdentityUnapproved = "unapproved"
	IdentityUnreadable = "unreadable"
)

// Identity is the active Formula of a Lab as a read-only view reports it. The
// decision id is set only when the exact bytes carry a complete, current
// activation decision. It never carries local paths or error text, which can
// name them.
type Identity struct {
	State      string `json:"state"`
	FormulaID  string `json:"formulaId,omitempty"`
	Name       string `json:"name,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	DecisionID string `json:"decisionId,omitempty"`
	Repository string `json:"repository,omitempty"`
	Binding    string `json:"binding,omitempty"`
	Model      string `json:"model,omitempty"`
}

// ActiveIdentity reads lab.json, the active Formula and formula-decisions/ without
// writing, locking or resuming anything. A held activation lock or a stale or
// unfinished decision is reported as a state instead of an error, so a viewer can
// still show the rest of the Lab.
func ActiveIdentity(dir string) Identity {
	if _, err := os.Lstat(filepath.Join(dir, ".formula-write.lock")); err == nil {
		return describe(Identity{State: IdentityLocked}, dir)
	}
	loaded, err := Load(dir, "")
	if err != nil {
		return describe(Identity{State: IdentityUnapproved}, dir)
	}
	if loaded.Formula.ID == "" {
		return Identity{State: IdentityNone}
	}
	id := fill(Identity{State: IdentityApproved}, loaded.Formula)
	if loaded.Approved {
		id.DecisionID = loaded.Approval.DecisionID
	}
	return id
}

// describe adds the Formula's own identity, read without approval checks, to a
// state that is not approved.
func describe(id Identity, dir string) Identity {
	loaded, err := Inspect(dir, "")
	if err != nil {
		if id.State != IdentityLocked {
			id.State = IdentityUnreadable
		}
		return id
	}
	if loaded.Formula.ID == "" {
		if id.State != IdentityLocked {
			id.State = IdentityNone
		}
		return id
	}
	return fill(id, loaded.Formula)
}

func fill(id Identity, f Formula) Identity {
	id.FormulaID, id.Name, id.SHA256 = f.ID, f.Name, f.SHA256
	id.Repository, id.Binding, id.Model = f.Intake.Repo, f.Harness.Binding, f.Harness.Model
	return id
}
