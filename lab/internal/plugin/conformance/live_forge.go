package conformance

import (
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// The fixed title and body of the one draft live-publish opens, and the fixed
// summary of a live-publish that opened a draft but did not pass.
const (
	livePublishOpened = "a draft change request was opened; close it by hand"
	livePublishTitle  = "Forgecell conformance check"
	livePublishBody   = "This draft was opened by `forgecell plugin test --live --allow-write` to check the plugin's forge_publish_draft. It was not marked ready, and nothing was merged or pushed."
)

// forge runs live-describe, live-policy, then live-change with a named change
// and live-publish with AllowWrite. The target is the project.
func (lr *liveRun) forge() {
	project := lr.opts.Target
	d, err := as[contract.ForgeDescription](lr.call("forge_describe", map[string]any{"project": project}))
	if !lr.record(CheckLiveDescribe, err) {
		return
	}
	_, err = as[contract.ForgePolicy](lr.call("forge_policy", map[string]any{"project": project, "branch": d.DefaultBranch}))
	if !lr.record(CheckLivePolicy, err) {
		return
	}
	if lr.opts.Change != "" && !lr.record(CheckLiveChange, lr.change(project)) {
		return
	}
	if lr.opts.AllowWrite {
		opened, err := lr.publish(project)
		lr.record(CheckLivePublish, err)
		// A draft left open by a failed or stopped check is named only by this
		// fixed text, never by its ref.
		if result := lr.results[CheckLivePublish]; opened && result.Status != Pass {
			result.Summary = livePublishOpened
			lr.results[CheckLivePublish] = result
		}
	}
}

// change requires forge_change, forge_checks and forge_comments on the named
// change to succeed and decode.
func (lr *liveRun) change(project string) error {
	ref := lr.opts.Change
	if _, err := as[contract.ForgeChange](lr.call("forge_change", map[string]any{"project": project, "ref": ref})); err != nil {
		return err
	}
	if _, err := as[contract.ForgeChecks](lr.call("forge_checks", map[string]any{"project": project, "ref": ref, "commit": lr.opts.Commit})); err != nil {
		return err
	}
	_, err := as[contract.ForgeComments](lr.call("forge_comments", map[string]any{"project": project, "ref": ref}))
	return err
}

// publish opens one draft from Head onto Base at Commit with the fixed title
// and body, then requires forge_change to report a draft at Commit and
// forge_find_change(Head) to return the same ref. It never merges, closes,
// marks ready or pushes. opened reports whether forge_publish_draft reached
// the plugin without a well-formed MCP error, so a draft may be open.
func (lr *liveRun) publish(project string) (opened bool, err error) {
	const tool = "forge_publish_draft"
	commit := lr.opts.Commit
	calls := lr.k.calls
	out, err := lr.call(tool, map[string]any{
		"project":    project,
		"headBranch": lr.opts.Head,
		"base":       lr.opts.Base,
		"title":      livePublishTitle,
		"body":       livePublishBody,
		"commit":     commit,
	})
	opened = lr.sentSince(calls, err)
	published, err := as[contract.ForgePublished](out, err)
	if err != nil {
		return opened, err
	}
	if !published.Draft || published.HeadCommit != commit {
		return opened, mcp.Fail(mcp.Failed, tool, "the published change is not a draft at the commit")
	}
	change, err := as[contract.ForgeChange](lr.call("forge_change", map[string]any{"project": project, "ref": published.Ref}))
	if err != nil {
		return opened, err
	}
	if !change.Draft {
		return opened, mcp.Fail(mcp.Failed, "forge_change", "the published change is not a draft")
	}
	if change.HeadCommit != commit {
		return opened, mcp.Fail(mcp.Failed, "forge_change", "the published change's head is not the commit")
	}
	found, err := as[contract.ForgeChanges](lr.call("forge_find_change", map[string]any{"project": project, "headBranch": lr.opts.Head}))
	if err != nil {
		return opened, err
	}
	for _, c := range found.Changes {
		if c.Ref == published.Ref {
			return opened, nil
		}
	}
	return opened, mcp.Fail(mcp.Failed, "forge_find_change", "the head branch does not find the published change")
}
