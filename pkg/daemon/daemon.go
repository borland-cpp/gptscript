package daemon

import (
	"context"
	"io"
	"os"
	"os/exec"
)

func SysDaemon() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_, _ = io.ReadAll(os.Stdin)
		cancel()
	}()

	cmd := exec.CommandContext(ctx, os.Args[2], os.Args[3:]...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
