package source

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// NewTarExtract returns an exec.Cmd that runs `tar -xpf -` against
// `stdin` and extracts into `dir`, preserving archived mode bits so a
// partial tree canonicalizes identically to the snapshot it came from.
// The caller is responsible for piping exactly one tar archive through
// Cmd.Stdin.
func NewTarExtract(dir string, stdin io.Reader) (*exec.Cmd, error) {
	if dir == "" {
		return nil, fmt.Errorf("source: tar extract dir is empty")
	}
	cmd := exec.CommandContext(context.Background(), "tar", "-C", dir, "-xpf", "-")
	cmd.Stdin = stdin
	return cmd, nil
}
