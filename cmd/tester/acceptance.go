package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"foundry.dev/foundry/internal/tester"
)

// acceptance runs the Phase 0 suite against the layout written by
// scripts/phase0-acceptance.sh:
//
//	DIR/v1/<stem>.raw            unsigned v1 disk     DIR/v1/<stem>.efi   unsigned v1 UKI
//	DIR/v1-signed/<stem>.signed.raw                   signed v1 disk
//	DIR/foreign/<stem>.efi       v1 UKI signed with another tenant's keys
//	DIR/v2-signed/update/        v2 update payload
func acceptance(args []string) int {
	fs := flag.NewFlagSet("acceptance", flag.ExitOnError)
	dir := fs.String("dir", "out/phase0", "artifact directory")
	stem := fs.String("stem", "ci-server_1.0.0", "v1 image stem")
	v2 := fs.String("v2-version", "1.0.1", "v2 version")
	only := fs.String("only", "", "comma-separated subset: matrix,unsigned,verity,tpm,ab")
	work := fs.String("work", "", "scratch directory (default DIR/work)")
	timeout := fs.Duration("timeout", 15*time.Minute, "per-boot timeout")
	out := fs.String("report", "", "write JSON report here (default DIR/acceptance.json)")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *work == "" {
		*work = filepath.Join(*dir, "work")
	}
	os.MkdirAll(*work, 0o755)
	c := tester.DefaultConfig(*work)
	c.BootTimeout = *timeout
	if err := c.Check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	i := strings.LastIndex(*stem, "_")
	im := tester.Images{Stem: *stem, Version: (*stem)[i+1:], V2Version: *v2,
		Unsigned:    filepath.Join(*dir, "v1", *stem+".raw"),
		UnsignedUKI: filepath.Join(*dir, "v1", *stem+".efi"),
		Signed:      filepath.Join(*dir, "v1-signed", *stem+".signed.raw"),
		SignedUKI:   filepath.Join(*dir, "v1-signed", *stem+".efi"),
		ForeignUKI:  filepath.Join(*dir, "foreign", *stem+".efi"),
		UpdateDir:   filepath.Join(*dir, "v2-signed", "update")}
	sel := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n != "" {
			sel[n] = true
		}
	}
	reps := tester.Acceptance(ctx, c, im, sel, os.Stderr)
	pass := true
	fmt.Println("Phase 0 acceptance")
	for _, r := range reps {
		v := "PASS"
		if !r.Passed {
			v, pass = "FAIL", false
		}
		fmt.Printf("  %-4s %-22s %5.0fs  %s\n", v, r.Scenario.Name, r.Seconds, r.Accel)
		for _, e := range r.Events {
			fmt.Printf("         · %s\n", e)
		}
		for _, f := range r.Failures {
			fmt.Printf("         ✗ %s\n", f)
		}
	}
	if *out == "" {
		*out = filepath.Join(*dir, "acceptance.json")
	}
	b, _ := json.MarshalIndent(map[string]any{"passed": pass, "at": time.Now().UTC(), "reports": reps}, "", "  ")
	os.WriteFile(*out, b, 0o644)
	if !pass {
		return 1
	}
	return 0
}
