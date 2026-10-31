//go:build !linux && !darwin

package plugin

import (
	"os"
	"os/exec"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

const noFollow = 0

// supported refuses: pinned placement checks and process-group control are
// unsupported here, as for extensions.
func supported() error {
	return mcp.Fail(mcp.NotInstalled, "", "plugins run only on Linux and macOS")
}

func configure(*exec.Cmd) {}

func ownedByMe(os.FileInfo) bool { return false }
