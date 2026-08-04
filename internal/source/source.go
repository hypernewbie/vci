package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hypernewbie/vci/internal/process"
)

type Repository struct {
	Root string `json:"root"`
	Name string `json:"name"`
}

func Discover(ctx context.Context, path string, runner process.Runner) (Repository, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Repository{}, fmt.Errorf("resolve source path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return Repository{}, fmt.Errorf("inspect source path: %w", err)
	}
	if !info.IsDir() {
		absolute = filepath.Dir(absolute)
	}
	var out strings.Builder
	result, runErr := runner.Run(ctx, process.Command{Executable: "git", Args: []string{"-C", absolute, "rev-parse", "--show-toplevel"}, Stdout: &out})
	if runErr != nil || result.ExitCode != 0 {
		return Repository{}, fmt.Errorf("source is not a Git repository: %w", runErr)
	}
	root := strings.TrimSpace(out.String())
	if root == "" {
		return Repository{}, fmt.Errorf("git returned an empty source root")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Repository{}, fmt.Errorf("resolve Git root: %w", err)
	}
	name := filepath.Base(root)
	return Repository{Root: root, Name: name}, nil
}

func MatchProject(repositoryName string, configured []string) (string, error) {
	for _, name := range configured {
		if name == repositoryName {
			return name, nil
		}
	}
	for _, name := range configured {
		if strings.EqualFold(name, repositoryName) {
			return name, nil
		}
	}
	return "", fmt.Errorf("project %q is not configured", repositoryName)
}
