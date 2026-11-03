// Command foundry-installer installs the running live OS onto a disk.
//
//	foundry-installer run            TTY installer, or unattended when the
//	                                 foundry.install credential is present
//	foundry-installer list           disks as JSON
//	foundry-installer plan -target /dev/X -mode erase|alongside
//	foundry-installer auto CONFIG    unattended install from a JSON file
//
// Unattended config (systemd credential "foundry.install"):
//
//	{"target": "/dev/vdb" | "auto", "mode": "erase", "eraseConfirmed": true,
//	 "bitlockerConfirmed": false, "makeDefault": true, "after": "poweroff"|"reboot"|"none"}
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"foundry.dev/foundry/internal/gpt"
	"foundry.dev/foundry/internal/installer"
)

var version = "dev"

type autoConfig struct {
	installer.Request
	After string `json:"after"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		if p := credential("foundry.install"); p != "" {
			err = auto(ctx, p)
		} else {
			err = tui(ctx)
		}
	case "auto":
		if len(os.Args) != 3 {
			usage()
		}
		err = auto(ctx, os.Args[2])
	case "list":
		err = list()
	case "plan":
		err = planCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "foundry-installer:", err)
		console("FOUNDRY-INSTALL-FAILED: " + err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: foundry-installer run | list | plan -target DEV -mode erase|alongside | auto CONFIG")
	os.Exit(2)
}

func credential(name string) string {
	if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
		if _, err := os.Stat(filepath.Join(d, name)); err == nil {
			return filepath.Join(d, name)
		}
	}
	return ""
}

// console reports to the kernel console and the first serial port (test
// harnesses watch it). Best effort.
func console(msg string) {
	for _, p := range []string{"/dev/console", "/dev/ttyS0"} {
		if f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NOCTTY, 0); err == nil {
			fmt.Fprintln(f, msg)
			f.Close()
		}
	}
}

// source finds the medium: through the ESP systemd-boot reported, or (CD
// boots, where firmware reports no partition) through the /usr partition
// whose UUID is the first half of usrhash=.
func source() (installer.Source, error) {
	disk := ""
	if uuid, err := installer.Host.BootESP(); err == nil {
		disk, _ = installer.Host.DiskOfPartUUID(uuid)
	}
	if disk == "" {
		if h := cmdlineValue("usrhash"); len(h) == 64 {
			uuid := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
			disk, _ = installer.Host.DiskOfPartUUID(uuid)
		}
	}
	if disk == "" {
		return installer.Source{}, errors.New("installer: cannot find the boot medium")
	}
	f, err := os.Open(disk)
	if err != nil {
		return installer.Source{}, err
	}
	defer f.Close()
	t, err := gpt.Read(f)
	if err != nil {
		return installer.Source{}, fmt.Errorf("installer: boot medium: %w", err)
	}
	size, _ := f.Seek(0, 2)
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	return installer.Source{Path: disk, Size: size, Table: t, OSID: osRelease("IMAGE_ID"), Arch: arch}, nil
}

func cmdlineValue(key string) string {
	b, _ := os.ReadFile("/proc/cmdline")
	for _, f := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}

func osRelease(key string) string {
	b, _ := os.ReadFile("/usr/lib/os-release")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func randGUID() gpt.GUID {
	var g gpt.GUID
	rand.Read(g[:])
	g[7] = g[7]&0x0f | 0x40 // version 4 (on-disk little-endian field order)
	g[8] = g[8]&0x3f | 0x80
	return g
}

func disks(src installer.Source) ([]installer.Disk, error) {
	ds, err := installer.Host.Disks(src.Path)
	if err != nil {
		return nil, err
	}
	for i := range ds {
		if ds[i].Table != nil && !ds[i].IsSource {
			installer.ProbeESP(&ds[i], installer.ExecRunner{}) // best effort
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].Path < ds[j].Path })
	return ds, nil
}

func fillIDs(r *installer.Request) {
	r.DiskGUID, r.ESPUUID, r.XbootUUID = randGUID(), randGUID(), randGUID()
}

func list() error {
	src, err := source()
	if err != nil {
		return err
	}
	ds, err := disks(src)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(ds)
}

func planCmd(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	target := fs.String("target", "", "target disk")
	mode := fs.String("mode", "erase", "erase | alongside")
	fs.Parse(args)
	src, err := source()
	if err != nil {
		return err
	}
	d, err := findDisk(src, *target)
	if err != nil {
		return err
	}
	r := installer.Request{Target: d.Path, Mode: installer.Mode(*mode), EraseConfirmed: true, BitLockerConfirmed: true}
	fillIDs(&r)
	pl, err := installer.MakePlan(src, d, r)
	if err != nil {
		return err
	}
	fmt.Print(pl.Summary())
	return nil
}

func findDisk(src installer.Source, target string) (installer.Disk, error) {
	ds, err := disks(src)
	if err != nil {
		return installer.Disk{}, err
	}
	if target == "auto" || target == "" {
		// Largest fixed disk that is not the medium.
		var best *installer.Disk
		for i := range ds {
			d := &ds[i]
			if d.IsSource || d.Removable {
				continue
			}
			if best == nil || d.Size > best.Size {
				best = d
			}
		}
		if best == nil {
			return installer.Disk{}, errors.New("no disk to install on")
		}
		return *best, nil
	}
	for _, d := range ds {
		if d.Path == target {
			return d, nil
		}
	}
	return installer.Disk{}, fmt.Errorf("disk %s not found", target)
}

func auto(ctx context.Context, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg autoConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("install config: %w", err)
	}
	src, err := source()
	if err != nil {
		return err
	}
	d, err := findDisk(src, cfg.Target)
	if err != nil {
		return err
	}
	fillIDs(&cfg.Request)
	cfg.Request.Target = d.Path
	pl, err := installer.MakePlan(src, d, cfg.Request)
	if err != nil {
		return err
	}
	fmt.Print(pl.Summary())
	last := -1
	err = installer.Apply(ctx, src, pl, installer.Host, installer.ExecRunner{}, version, func(step string, f float64) {
		if p := int(f * 10); p != last || strings.HasPrefix(step, "Note") {
			last = p
			fmt.Printf("[%3d%%] %s\n", int(f*100), step)
		}
	})
	if err != nil {
		return err
	}
	syscall.Sync()
	console("FOUNDRY-INSTALL-OK " + string(pl.Mode) + " " + pl.Target)
	switch cfg.After {
	case "poweroff":
		return syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	case "reboot":
		return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	}
	return nil
}

// ---------------------------------------------------------------- TTY UI

func tui(ctx context.Context) error {
	in := bufio.NewReader(os.Stdin)
	ask := func(q string) string {
		fmt.Print(q)
		s, _ := in.ReadString('\n')
		return strings.TrimSpace(s)
	}
	name := osRelease("PRETTY_NAME")
	fmt.Printf("\n  %s\n  %s\n\n", strings.TrimSuffix(name, " Installer")+" — installer", strings.Repeat("─", 50))
	src, err := source()
	if err != nil {
		return err
	}
	for {
		ds, err := disks(src)
		if err != nil {
			return err
		}
		var choices []installer.Disk
		fmt.Println("  Disks:")
		for _, d := range ds {
			if d.IsSource {
				continue
			}
			choices = append(choices, d)
			what := "empty"
			switch {
			case d.WindowsBoot && d.BitLocker:
				what = "Windows (BitLocker on)"
			case d.WindowsBoot:
				what = "Windows"
			case d.Foreign:
				what = "other partitioning (MBR)"
			case len(d.Parts) > 0:
				what = fmt.Sprintf("%d partitions", len(d.Parts))
			}
			fmt.Printf("   %d) %-14s %8.1f GiB  %-24s %s\n", len(choices), d.Path, float64(d.Size)/float64(installer.GiB), d.Model, what)
		}
		if len(choices) == 0 {
			ask("\n  No disk found to install on. Connect one and press Enter to look again.")
			continue
		}
		n, _ := strconv.Atoi(ask("\n  Install on which disk? (number, or 0 to rescan) "))
		if n < 1 || n > len(choices) {
			continue
		}
		d := choices[n-1]
		mode := installer.Erase
		if d.WindowsBoot {
			fmt.Println("\n  Windows was found on this disk.")
			fmt.Println("   1) Install alongside Windows (uses unallocated space only; Windows is not changed)")
			fmt.Println("   2) Erase the disk and replace Windows")
			if ask("  Choice [1]: ") != "2" {
				mode = installer.Alongside
			}
		}
		req := installer.Request{Target: d.Path, Mode: mode}
		fillIDs(&req)
		if mode == installer.Alongside {
			req.MakeDefault = strings.HasPrefix(strings.ToLower(ask("  Start this OS by default? [Y/n] ")), "n") == false
		}
		var pl *installer.Plan
		for {
			pl, err = installer.MakePlan(src, d, req)
			if !errors.Is(err, installer.ErrNeedsConfirmation) {
				break
			}
			fmt.Println("\n  " + strings.TrimPrefix(err.Error(), "installer: confirmation required: "))
			if strings.Contains(err.Error(), "BitLocker") {
				if ask("  Type YES if you have the BitLocker recovery key: ") != "YES" {
					break
				}
				req.BitLockerConfirmed = true
			} else {
				if ask("  Type ERASE to continue: ") != "ERASE" {
					break
				}
				req.EraseConfirmed = true
			}
		}
		if err != nil {
			fmt.Println("\n  " + err.Error())
			ask("  Press Enter to start over.")
			continue
		}
		fmt.Println("\n" + pl.Summary())
		if !strings.HasPrefix(strings.ToLower(ask("  Install now? [y/N] ")), "y") {
			continue
		}
		err = installer.Apply(ctx, src, pl, installer.Host, installer.ExecRunner{}, version, func(step string, f float64) {
			bar := int(f * 40)
			fmt.Printf("\r  [%s%s] %3d%%  %-40.40s", strings.Repeat("█", bar), strings.Repeat("·", 40-bar), int(f*100), step)
		})
		fmt.Println()
		if err != nil {
			fmt.Println("\n  Installation failed: " + err.Error())
			ask("  Press Enter to start over.")
			continue
		}
		fmt.Println("\n  Installed. Remove the installation medium, then press Enter to restart.")
		fmt.Println("  On first start the system encrypts its data partition with this computer's TPM.")
		ask("")
		syscall.Sync()
		return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	}
}
