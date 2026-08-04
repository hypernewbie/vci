package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/hypernewbie/vci/internal/model"
)

func spawnRun(id model.RunID) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Vci executable: %w", err)
	}
	child := exec.Command(executable, "internal-run", string(id))
	child.Env = os.Environ()
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	child.Stdout, child.Stderr = devNull, devNull
	if err := child.Start(); err != nil {
		_ = devNull.Close()
		return fmt.Errorf("start detached run: %w", err)
	}
	_ = devNull.Close()
	return child.Process.Release()
}
