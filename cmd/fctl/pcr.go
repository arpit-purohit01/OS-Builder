package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"foundry.dev/foundry/internal/uki"
)

// cmdPCR prints the PCR 11 values systemd-stub will measure for a UKI
// (used for remote attestation and to verify boot measurements).
func cmdPCR(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: fctl pcr <uki.efi>")
	}
	p, err := uki.PredictPCR11(ctx, "", args[0])
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(p)
}
