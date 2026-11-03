// Command tester boots a disk image in QEMU/OVMF (+swtpm) and checks the
// image's boot report. Exit status 0 = all scenarios passed.
//
//	tester -image disk.raw -scenario uefi-tpm,sb-tpm -version 1.0.0 -slot ci-server_1.0.0 -crypt yes
//	tester install -image signed.raw [-scenario install-erase,install-alongside,install-bitlocker-refused]
//	tester acceptance -dir out/phase0 [-only matrix,unsigned,verity,tpm,ab]   (Phase 0 suite)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"foundry.dev/foundry/internal/tester"
)

// scenarios by name (docs/plan/phase-0 boot matrix).
var scenarios = map[string]tester.Scenario{
	"uefi":            {Name: "uefi"},
	"uefi-tpm":        {Name: "uefi-tpm", TPM: true},
	"uefi-tpm-reboot": {Name: "uefi-tpm-reboot", TPM: true, Reboot: true},
	"sb-tpm":          {Name: "sb-tpm", SecureBoot: true, TPM: true, SBEnforced: true},
	"sb-tpm-reboot":   {Name: "sb-tpm-reboot", SecureBoot: true, TPM: true, SBEnforced: true, Reboot: true},
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "acceptance" {
		os.Exit(acceptance(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "install" {
		os.Exit(installCmd(os.Args[2:]))
	}
	image := flag.String("image", "", "raw disk image")
	names := flag.String("scenario", "uefi-tpm", "comma-separated: "+strings.Join(keys(), ","))
	version := flag.String("version", "", "expected IMAGE_VERSION")
	slot := flag.String("slot", "", "expected usr partition label")
	crypt := flag.String("crypt", "", "expected root encryption (yes|no)")
	work := flag.String("work", os.TempDir(), "scratch directory")
	timeout := flag.Duration("timeout", 15*time.Minute, "per-boot timeout")
	accel := flag.String("accel", "", "kvm|tcg (default auto)")
	mem := flag.Int("mem", 2048, "memory MiB")
	jsonOut := flag.Bool("json", false, "print JSON reports")
	flag.Parse()
	if *image == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg := tester.DefaultConfig(*work)
	cfg.BootTimeout, cfg.Accel, cfg.MemoryMiB = *timeout, *accel, *mem
	if err := cfg.Check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ok := true
	var reports []*tester.Report
	for _, n := range strings.Split(*names, ",") {
		sc, found := scenarios[n]
		if !found {
			fmt.Fprintf(os.Stderr, "unknown scenario %q\n", n)
			os.Exit(2)
		}
		sc.Version, sc.Slot, sc.Crypt = *version, *slot, *crypt
		if *crypt == "yes" && sc.TPM {
			sc.TPM2 = "yes"
		}
		rep, err := tester.Run(ctx, cfg, *image, sc, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		reports = append(reports, rep)
		status := "PASS"
		if !rep.Passed {
			status, ok = "FAIL", false
		}
		fmt.Fprintf(os.Stderr, "%s %s (%.0fs, %s) serial log: %s\n", status, n, rep.Seconds, rep.Accel, rep.Serial)
		for _, f := range rep.Failures {
			fmt.Fprintln(os.Stderr, "  -", f)
		}
	}
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(reports)
	}
	if !ok {
		os.Exit(1)
	}
}

func keys() []string {
	var k []string
	for n := range scenarios {
		k = append(k, n)
	}
	return k
}

// installCmd runs the installer scenarios (Phase 2 acceptance 3).
func installCmd(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	image := fs.String("image", "", "signed raw disk image (with the installer entry)")
	names := fs.String("scenario", "install-erase,install-alongside,install-bitlocker-refused", "scenarios")
	work := fs.String("work", os.TempDir(), "scratch directory")
	mem := fs.Int("mem", 2048, "memory MiB")
	fs.Parse(args)
	if *image == "" {
		fs.Usage()
		return 2
	}
	c := tester.DefaultConfig(*work)
	c.MemoryMiB = *mem
	ok := true
	for _, n := range strings.Split(*names, ",") {
		r := tester.InstallScenario(context.Background(), c, *image, n, os.Stderr)
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		ok = ok && r.Passed
	}
	if !ok {
		return 1
	}
	return 0
}
