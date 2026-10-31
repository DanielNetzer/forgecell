// Command forgecell-jira is the first-party Jira tracker plugin: an MCP server
// on stdio that serves the forgecell.tracker/v1 contract over the Jira Cloud
// REST API v3. It reads JIRA_EMAIL and JIRA_API_TOKEN (a secret), sent as HTTP
// Basic auth, and JIRA_CA_FILE (extra trusted certificates). The site and
// project come from each call's reviewed config. It only reads.
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
	serverName    = "forgecell-jira"
	serverVersion = "1"
)

func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "forgecell-jira takes no arguments")
		os.Exit(2)
	}
	s := newServer(mcpserver.Env, os.Stderr)
	if err := s.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}

// newServer registers every tracker contract tool with its contract input
// schema and output bound. lookup reads the plugin's needs.env names. An
// invalid configuration is logged once and fails every call.
func newServer(lookup func(string) (string, bool), stderr io.Writer) *mcpserver.Server {
	c, ok := contract.Lookup("tracker")
	if !ok {
		panic("jira: no tracker contract")
	}
	s := mcpserver.New(serverName, serverVersion, c.ID)
	s.SecretEnv(tokenEnv)
	s.SetStderr(stderr)
	tr := newTracker(lookup)
	if tr.configErr != nil {
		s.Log("the configuration is invalid: " + tr.configErr.Error())
	}
	for i := range c.Tools {
		t := &c.Tools[i]
		s.Tool(t.Name, t.InputSchema(), tr.bind(t), mcpserver.MaxOutputBytes(t.MaxOutputBytes))
	}
	return s
}
