// Command forgecell-forgecomments is the first-party review-bot comments
// reviewer plugin: an MCP server on stdio that serves the
// forgecell.reviewer/v1 contract. It turns the review comments the engine read
// from the forge, such as Cursor BugBot's or GitLab Duo's, into findings. It is
// offline: it reads no environment, makes no request and needs no credentials.
// Everything it reads arrives in the call.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/plugins/internal/mcpserver"
)

const (
	serverName    = "forgecell-forgecomments"
	serverVersion = "1"
)

func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "forgecell-forgecomments takes no arguments")
		os.Exit(2)
	}
	s := newServer(os.Stderr)
	if err := s.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}

// newServer registers every reviewer contract tool with its contract input
// schema and output bound. Incoming messages are bounded by the largest
// per-call message bound of the contract tools, so a call carrying the forge's
// comments is never refused for its size alone.
func newServer(stderr io.Writer) *mcpserver.Server {
	c, ok := contract.Lookup("reviewer")
	if !ok {
		panic("forgecomments: no reviewer contract")
	}
	s := mcpserver.New(serverName, serverVersion, c.ID)
	s.SetStderr(stderr)
	maxIn := 0
	for i := range c.Tools {
		t := &c.Tools[i]
		maxIn = max(maxIn, t.MessageBytes())
		s.Tool(t.Name, t.InputSchema(), bind(t), mcpserver.MaxOutputBytes(t.MaxOutputBytes))
	}
	s.SetMaxMessageBytes(maxIn)
	return s
}
