// Command forgecell-gitlab is the first-party GitLab forge plugin: an MCP
// server on stdio that serves the forgecell.forge/v1 contract over the GitLab
// REST API v4. It reads GITLAB_TOKEN (a secret, sent as PRIVATE-TOKEN),
// GITLAB_URL (default https://gitlab.com) and GITLAB_CA_FILE (extra trusted
// certificates) and never pushes.
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
	serverName    = "forgecell-gitlab"
	serverVersion = "1"
)

func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "forgecell-gitlab takes no arguments")
		os.Exit(2)
	}
	s := newServer(mcpserver.Env, os.Stderr)
	if err := s.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}

// newServer registers every forge contract tool with its contract input schema
// and output bound. lookup reads the plugin's needs.env names. An invalid
// configuration is logged once and fails every call.
func newServer(lookup func(string) (string, bool), stderr io.Writer) *mcpserver.Server {
	c, ok := contract.Lookup("forge")
	if !ok {
		panic("gitlab: no forge contract")
	}
	s := mcpserver.New(serverName, serverVersion, c.ID)
	s.SecretEnv(tokenEnv)
	s.SetStderr(stderr)
	f := newForge(lookup)
	if f.configErr != nil {
		s.Log("the configuration is invalid: " + f.configErr.Error())
	}
	for i := range c.Tools {
		t := &c.Tools[i]
		s.Tool(t.Name, t.InputSchema(), f.bind(t), mcpserver.MaxOutputBytes(t.MaxOutputBytes))
	}
	return s
}
