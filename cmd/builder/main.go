// Command builder runs inside the ephemeral build VM (started by
// foundry-builder.service in the golden builder image). It reads the job
// disk, runs mkosi with downloads forced through the egress proxy, writes
// outputs and result.json to the output disk and powers the VM off.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"foundry.dev/foundry/internal/sandbox"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "vm" {
		fmt.Fprintln(os.Stderr, "usage: foundry-builder vm   (runs only inside a Foundry build VM)")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	err := sandbox.RunGuest(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "foundry-builder:", err)
	}
	// Always power off: the host collects outputs from the disks.
	exec.Command("systemctl", "poweroff", "--no-block").Run()
}
