// Package tester boots disk images in QEMU with OVMF (optionally Secure
// Boot in setup mode) and a software TPM (swtpm), and checks the report the
// image prints when the hypervisor passes the foundry.boottest credential.
//
// A Machine keeps its disk, firmware variables and TPM state across boots,
// so multi-step scenarios (enroll → reboot, update → fallback, change the
// UKI → TPM refuses to unlock) run like on real hardware. The source image
// is never modified.
package tester

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"foundry.dev/foundry/internal/procreg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Config locates the emulator and firmware.
type Config struct {
	QEMU         string
	QEMUImg      string
	Swtpm        string
	OVMFCode     string // non-Secure-Boot firmware
	OVMFCodeSB   string // Secure Boot capable firmware (SMM)
	OVMFVars     string // template with no keys (setup mode)
	Accel        string // kvm | tcg | "" (auto)
	MemoryMiB    int
	CPUs         int
	BootTimeout  time.Duration // per boot
	ExtraDiskGiB int           // virtual disk = image + this (room for the root partition)
	WorkDir      string
	Procs        procreg.Dir // records VM/swtpm process groups for the crash reaper
}

// DefaultConfig uses Ubuntu's OVMF paths.
func DefaultConfig(work string) Config {
	return Config{QEMU: "qemu-system-x86_64", QEMUImg: "qemu-img", Swtpm: "swtpm",
		OVMFCode: "/usr/share/OVMF/OVMF_CODE_4M.fd", OVMFCodeSB: "/usr/share/OVMF/OVMF_CODE_4M.secboot.fd",
		OVMFVars: "/usr/share/OVMF/OVMF_VARS_4M.fd", MemoryMiB: 2048, CPUs: 2, BootTimeout: 15 * time.Minute, ExtraDiskGiB: 4, WorkDir: work}
}

// Scenario is one boot test of an image.
type Scenario struct {
	Name       string `json:"name"`
	SecureBoot bool   `json:"secureBoot"` // SB firmware in setup mode; image must enroll its keys
	TPM        bool   `json:"tpm"`
	Reboot     bool   `json:"reboot"` // reboot once after the first report
	// Expectations (empty = not checked)
	Version    string `json:"version,omitempty"`
	Slot       string `json:"slot,omitempty"`
	Crypt      string `json:"crypt,omitempty"` // yes|no
	TPM2       string `json:"tpm2,omitempty"`  // yes|no
	SBEnforced bool   `json:"sbEnforced,omitempty"`
	// InitrdOnlyUnseal: the image carries a signed PCR policy for the initrd
	// phase only, so the running system must not be able to unseal the key.
	InitrdOnlyUnseal bool `json:"initrdOnlyUnseal,omitempty"`
	// PCR11 is the predicted PCR 11 after boot (phase "ready"), checked
	// against the TPM's value (remote attestation foundation).
	PCR11 string `json:"pcr11,omitempty"`
}

// Boot is one parsed report.
type Boot struct {
	Fields  map[string]string `json:"fields"`
	Cmdline string            `json:"cmdline,omitempty"`
	Seconds float64           `json:"seconds"`
}

// Report is the result of a scenario.
type Report struct {
	Scenario Scenario `json:"scenario"`
	Passed   bool     `json:"passed"`
	Boots    []Boot   `json:"boots"`
	Events   []string `json:"events,omitempty"`
	Failures []string `json:"failures,omitempty"`
	Seconds  float64  `json:"seconds"`
	Accel    string   `json:"accel"`
	Serial   string   `json:"-"` // path of the serial log
}

func (c Config) accel() string {
	if c.Accel != "" {
		return c.Accel
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		return "kvm"
	}
	return "tcg"
}

// ------------------------------------------------------------------ machine

// Machine is a VM whose disk, UEFI variables and TPM persist across boots.
type Machine struct {
	c      Config
	Name   string
	dir    string
	Disk   string // working disk (raw when mutable, else qcow2 overlay)
	format string
	vars   string
	tpmDir string
	sb     bool
	extra  []string
	rw     []string // writable extra raw disks (install targets)
	boots  int
	Accel  string
	// Credentials are passed as SMBIOS type 11 binary credentials (name → value).
	Credentials map[string][]byte
}

// NewMachine prepares a VM for image. mutable=true makes a sparse raw copy
// whose bytes tests may edit between boots (ESP files, corruption).
func NewMachine(ctx context.Context, c Config, name, image string, sb, tpm, mutable bool) (*Machine, error) {
	dir, err := os.MkdirTemp(c.WorkDir, "vm-"+safe(name)+"-")
	if err != nil {
		return nil, err
	}
	m := &Machine{c: c, Name: name, dir: dir, sb: sb, Accel: c.accel()}
	// qemu resolves a relative backing file against the overlay's directory.
	if image, err = filepath.Abs(image); err != nil {
		m.Close()
		return nil, err
	}
	st, err := os.Stat(image)
	if err != nil {
		m.Close()
		return nil, err
	}
	size := st.Size() + int64(c.ExtraDiskGiB)<<30
	if mutable {
		m.Disk, m.format = filepath.Join(dir, "disk.raw"), "raw"
		if err := sparseCopy(image, m.Disk, size); err != nil {
			m.Close()
			return nil, err
		}
	} else {
		m.Disk, m.format = filepath.Join(dir, "overlay.qcow2"), "qcow2"
		if out, err := exec.CommandContext(ctx, c.QEMUImg, "create", "-q", "-f", "qcow2", "-F", "raw", "-b", image, m.Disk, fmt.Sprint(size)).CombinedOutput(); err != nil {
			m.Close()
			return nil, fmt.Errorf("qemu-img: %v: %s", err, out)
		}
	}
	m.vars = filepath.Join(dir, "vars.fd")
	if err := copyFile(c.OVMFVars, m.vars); err != nil {
		m.Close()
		return nil, err
	}
	if tpm {
		// unix socket paths are limited to 108 bytes: keep the TPM dir short.
		if m.tpmDir, err = os.MkdirTemp("", "ftpm-"); err != nil {
			m.Close()
			return nil, err
		}
	}
	return m, nil
}

// AddDisk attaches a read-only raw disk (e.g. an update medium).
func (m *Machine) AddDisk(path string) { m.extra = append(m.extra, path) }

// AddWritableDisk attaches a writable raw disk (e.g. an installation target).
func (m *Machine) AddWritableDisk(path string) { m.rw = append(m.rw, path) }

// Vars is the machine's UEFI variable store (NVRAM).
func (m *Machine) Vars() string { return m.vars }

// UseVars replaces the NVRAM with a copy of another machine's (boot entries
// written by an installer carry over to the installed disk's first boot).
func (m *Machine) UseVars(path string) error { return copyFile(path, m.vars) }

// Close removes the machine's files.
func (m *Machine) Close() {
	if m.tpmDir != "" {
		os.RemoveAll(m.tpmDir)
	}
	os.RemoveAll(m.dir)
}

// Expect says when a boot session ends.
type Expect struct {
	Mode    string         // foundry.boottest credential value ("" = none)
	Reports int            // stop after this many FOUNDRY-BOOT-OK lines
	Match   *regexp.Regexp // stop (success) when a serial line matches
	Forbid  *regexp.Regexp // stop (failure) when a serial line matches
	PerBoot time.Duration  // max time without progress (a new boot or report)
	Timeout time.Duration  // overall limit
}

// Session is the outcome of one QEMU run (which may span several reboots).
type Session struct {
	Boots   []Boot   `json:"boots"`
	Events  []string `json:"events,omitempty"`  // FOUNDRY-* markers other than report fields
	Kernels []string `json:"kernels,omitempty"` // usrhash= of every kernel started (one per boot)
	Matched string   `json:"matched,omitempty"`
	Failure string   `json:"failure,omitempty"`
	Seconds float64  `json:"seconds"`
	Serial  string   `json:"-"`
}

func (m *Machine) firmware() string {
	if m.sb {
		return m.c.OVMFCodeSB
	}
	return m.c.OVMFCode
}

// Command builds the QEMU command for this machine (and starts swtpm when
// the machine has a TPM). display is a QEMU -display/-vnc argument list
// (nil = headless); serial is the serial log path. stop releases swtpm.
func (m *Machine) Command(ctx context.Context, serial, credMode string, display []string) (cmd *exec.Cmd, stop func(), err error) {
	stop = func() {}
	args := []string{"-machine", "q35,smm=on", "-m", fmt.Sprint(m.c.MemoryMiB), "-smp", fmt.Sprint(m.c.CPUs),
		"-monitor", "none", "-nodefaults", "-no-user-config",
		"-global", "driver=cfi.pflash01,property=secure,value=on",
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=" + m.firmware(),
		"-drive", "if=pflash,format=raw,unit=1,file=" + m.vars,
		"-drive", "if=none,id=d0,format=" + m.format + ",file=" + m.Disk, "-device", "virtio-blk-pci,drive=d0,bootindex=1",
		"-netdev", "user,id=n0,restrict=on", "-device", "virtio-net-pci,netdev=n0",
		"-device", "virtio-rng-pci",
		"-serial", "file:" + serial,
	}
	if display == nil {
		args = append(args, "-display", "none")
	} else {
		args = append(args, display...)
	}
	for i, d := range m.extra {
		args = append(args, "-drive", fmt.Sprintf("if=none,id=x%d,format=raw,readonly=on,file=%s", i, d), "-device", fmt.Sprintf("virtio-blk-pci,drive=x%d", i))
	}
	for i, d := range m.rw {
		args = append(args, "-drive", fmt.Sprintf("if=none,id=w%d,format=raw,discard=unmap,detect-zeroes=unmap,file=%s", i, d), "-device", fmt.Sprintf("virtio-blk-pci,drive=w%d,serial=target%d", i, i))
	}
	for name, v := range m.Credentials {
		args = append(args, "-smbios", "type=11,value=io.systemd.credential.binary:"+name+"="+base64.StdEncoding.EncodeToString(v))
	}
	if m.Accel == "kvm" {
		args = append(args, "-accel", "kvm", "-cpu", "host")
	} else {
		args = append(args, "-accel", "tcg,thread=multi", "-cpu", "max")
	}
	if credMode != "" {
		// SMBIOS type 11: the boot-test credential.
		args = append(args, "-smbios", "type=11,value=io.systemd.credential:foundry.boottest="+credMode)
	}
	// A console on the serial port for diagnostics (ignored by systemd-stub
	// under Secure Boot).
	args = append(args, "-smbios", "type=11,value=io.systemd.stub.kernel-cmdline-extra=console=ttyS0,,115200")
	if m.tpmDir != "" {
		sock := filepath.Join(m.tpmDir, "sock")
		os.Remove(sock)
		sw := exec.CommandContext(ctx, m.c.Swtpm, "socket", "--tpmstate", "dir="+m.tpmDir, "--ctrl", "type=unixio,path="+sock, "--tpm2")
		sw.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := sw.Start(); err != nil {
			return nil, stop, fmt.Errorf("swtpm: %w", err)
		}
		unreg := m.c.Procs.Register("swtpm-"+m.Name, sw.Process.Pid)
		stop = func() { sw.Process.Kill(); sw.Wait(); unreg() }
		for i := 0; ; i++ {
			if _, err := os.Stat(sock); err == nil {
				break
			}
			if i == 100 {
				stop()
				return nil, func() {}, errors.New("tester: swtpm did not create its socket")
			}
			time.Sleep(100 * time.Millisecond)
		}
		args = append(args, "-chardev", "socket,id=chrtpm,path="+sock, "-tpmdev", "emulator,id=tpm0,chardev=chrtpm", "-device", "tpm-tis,tpmdev=tpm0")
	}
	cmd = exec.CommandContext(ctx, m.c.QEMU, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, stop, nil
}

// Boot powers the machine on and watches its serial console.
func (m *Machine) Boot(ctx context.Context, e Expect, log io.Writer) (*Session, error) {
	start := time.Now()
	m.boots++
	serial := filepath.Join(m.dir, fmt.Sprintf("serial-%d.log", m.boots))
	keep := filepath.Join(m.c.WorkDir, fmt.Sprintf("serial-%s-%d.log", safe(m.Name), m.boots))
	if e.Timeout == 0 {
		e.Timeout = m.c.BootTimeout * time.Duration(max(1, e.Reports)+1)
	}
	if e.PerBoot == 0 {
		e.PerBoot = m.c.BootTimeout
	}
	qctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	cmd, stopTPM, err := m.Command(qctx, serial, e.Mode, nil)
	if err != nil {
		return nil, err
	}
	defer stopTPM()
	var qerr bytes.Buffer
	cmd.Stderr = &qerr
	if log != nil {
		fmt.Fprintf(log, "tester: %s: boot session %d (%s, secure boot %v, tpm %v, mode %q)\n", m.Name, m.boots, m.Accel, m.sb, m.tpmDir != "", e.Mode)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer m.c.Procs.Register("testvm-"+m.Name, cmd.Process.Pid)()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	s := watch(qctx, serial, e, done, log, m.Name)
	select {
	case <-done:
	default:
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	if s.Failure != "" {
		if q := strings.TrimSpace(qerr.String()); q != "" {
			s.Failure += "; qemu: " + lastLines(q, 5)
		}
	}
	copyFile(serial, keep)
	s.Serial = keep
	s.Seconds = time.Since(start).Seconds()
	return s, nil
}

// watch tails the serial log until the expectation is met or violated.
func watch(ctx context.Context, path string, e Expect, exited <-chan error, log io.Writer, name string) *Session {
	s := &Session{}
	var off int64
	cur := Boot{Fields: map[string]string{}}
	progress, bootStart := time.Now(), time.Now()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var partial string
	ok := 0
	for {
		if f, err := os.Open(path); err == nil {
			f.Seek(off, io.SeekStart)
			data, _ := io.ReadAll(f)
			f.Close()
			off += int64(len(data))
			text := partial + string(data)
			partial = ""
			if i := strings.LastIndexByte(text, '\n'); i < len(text)-1 {
				partial = text[i+1:]
				text = text[:i+1]
			}
			sc := bufio.NewScanner(strings.NewReader(text))
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				raw := strings.Map(func(r rune) rune {
					if r == 0 || r == '\r' {
						return -1
					}
					return r
				}, sc.Text())
				l := strings.TrimSpace(ansiRe.ReplaceAllString(raw, ""))
				if e.Forbid != nil && e.Forbid.MatchString(l) {
					s.Boots = append(s.Boots, cur)
					s.Failure = "unexpected output: " + trimTo(l, 200)
					return s
				}
				if e.Match != nil && e.Match.MatchString(l) {
					s.Matched = trimTo(l, 300)
					if log != nil {
						fmt.Fprintf(log, "tester: %s: matched %q\n", name, s.Matched)
					}
					return s
				}
				if strings.Contains(l, "Linux version") {
					progress = time.Now()
				}
				if strings.Contains(l, "] Command line: ") {
					if m := usrhashRe.FindStringSubmatch(l); m != nil {
						s.Kernels = append(s.Kernels, m[1])
					}
				}
				i := strings.Index(l, "FOUNDRY-")
				if i < 0 {
					continue
				}
				l = l[i:]
				switch {
				case strings.HasPrefix(l, "FOUNDRY-TEST cmdline="):
					cur.Cmdline = strings.TrimPrefix(l, "FOUNDRY-TEST cmdline=")
				case strings.HasPrefix(l, "FOUNDRY-TEST "):
					for _, kv := range strings.Fields(strings.TrimPrefix(l, "FOUNDRY-TEST ")) {
						if k, v, ok := strings.Cut(kv, "="); ok {
							cur.Fields[k] = v
						}
					}
				case l == "FOUNDRY-REBOOTING" || l == "FOUNDRY-BOOT-OK":
					cur.Seconds = time.Since(bootStart).Seconds()
					s.Boots = append(s.Boots, cur)
					if log != nil {
						fmt.Fprintf(log, "tester: %s: boot %d reported after %.0fs: %v\n", name, len(s.Boots), cur.Seconds, cur.Fields)
					}
					cur = Boot{Fields: map[string]string{}}
					progress, bootStart = time.Now(), time.Now()
					ok++ // reports so far (a REBOOTING report counts; only BOOT-OK ends the session)
					if l == "FOUNDRY-REBOOTING" {
						s.Events = append(s.Events, l)
					}
					if l == "FOUNDRY-BOOT-OK" && e.Reports > 0 && ok >= e.Reports {
						return s
					}
				default:
					s.Events = append(s.Events, trimTo(l, 400))
					progress = time.Now()
					if log != nil {
						fmt.Fprintf(log, "tester: %s: %s\n", name, trimTo(l, 200))
					}
					if strings.HasPrefix(l, "FOUNDRY-UPDATE-FAILED") {
						s.Failure = l
						return s
					}
				}
			}
		}
		if time.Since(progress) > e.PerBoot {
			s.Failure = fmt.Sprintf("no progress for %s after %d report(s)", e.PerBoot, len(s.Boots))
			return s
		}
		select {
		case <-ctx.Done():
			s.Failure = "timed out"
			return s
		case err := <-exited:
			s.Failure = fmt.Sprintf("VM exited (%v)", err)
			return s
		case <-t.C:
		}
	}
}

func trimTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Run boots image per a standard scenario (used by the build pipeline).
func Run(ctx context.Context, c Config, image string, sc Scenario, log io.Writer) (*Report, error) {
	start := time.Now()
	m, err := NewMachine(ctx, c, sc.Name, image, sc.SecureBoot, sc.TPM, false)
	if err != nil {
		return nil, err
	}
	defer m.Close()
	mode, reports := "report", 1
	if sc.Reboot {
		mode, reports = "reboot", 2
	}
	sess, err := m.Boot(ctx, Expect{Mode: mode, Reports: reports, Forbid: panicRe}, log)
	if err != nil {
		return nil, err
	}
	rep := &Report{Scenario: sc, Accel: m.Accel, Boots: sess.Boots, Events: sess.Events, Serial: sess.Serial}
	if sess.Failure != "" {
		rep.Failures = append(rep.Failures, sess.Failure)
	}
	rep.Failures = append(rep.Failures, check(sc, completed(sess.Boots))...)
	rep.Passed = len(rep.Failures) == 0
	rep.Seconds = time.Since(start).Seconds()
	return rep, nil
}

// ansiRe matches terminal escape sequences (systemd colours its status lines).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][A-Za-z0-9]`)

var usrhashRe = regexp.MustCompile(`usrhash=([0-9a-f]{64})`)

// panicRe catches kernel panics and emergency mode early.
var panicRe = regexp.MustCompile(`Kernel panic|You are in emergency mode|Entering emergency mode`)

func completed(bs []Boot) []Boot {
	var out []Boot
	for _, b := range bs {
		if len(b.Fields) > 0 {
			out = append(out, b)
		}
	}
	return out
}

func check(sc Scenario, boots []Boot) []string {
	var f []string
	for i, b := range boots {
		n := i + 1
		fl := b.Fields
		eq := func(k, want string) {
			if want != "" && fl[k] != want {
				f = append(f, fmt.Sprintf("boot %d: %s=%q, want %q", n, k, fl[k], want))
			}
		}
		// The health check runs on counted (trial) boots, where it gates
		// boot-complete.target; on regular boots it is not part of the boot.
		if strings.Contains(fl["entry"], "+") {
			eq("health", "active")
			eq("bootcomplete", "active")
		} else if fl["health"] == "failed" {
			f = append(f, fmt.Sprintf("boot %d: health check failed", n))
		}
		if fl["entry"] == "" {
			f = append(f, fmt.Sprintf("boot %d: not booted through systemd-boot (no LoaderEntrySelected)", n))
		}
		eq("verity", "verified")
		eq("version", sc.Version)
		eq("slot", sc.Slot)
		eq("crypt", sc.Crypt)
		eq("tpm2", sc.TPM2)
		if sc.InitrdOnlyUnseal && fl["unseal"] != "refused" {
			f = append(f, fmt.Sprintf("boot %d: unseal from the running system: %q, want \"refused\" (policy must be limited to the initrd)", n, fl["unseal"]))
		}
		if sc.SBEnforced {
			eq("sb", "1")
		}
		eq("pcr11", sc.PCR11)
		if fl["state"] != "running" {
			f = append(f, fmt.Sprintf("boot %d: system state %q (failed units: %s)", n, fl["state"], fl["failed"]))
		}
		if fl["failed"] != "" {
			f = append(f, fmt.Sprintf("boot %d: failed units: %s", n, fl["failed"]))
		}
		if !strings.Contains(b.Cmdline, "usrhash=") {
			f = append(f, fmt.Sprintf("boot %d: /usr is not pinned by usrhash=", n))
		}
	}
	if sc.Reboot && len(boots) == 2 && boots[1].Fields["rebooted"] != "yes" {
		f = append(f, "second boot did not see the first boot's state (root not persistent?)")
	}
	want := 1
	if sc.Reboot {
		want = 2
	}
	if len(boots) < want {
		f = append(f, fmt.Sprintf("%d of %d boot report(s) received", len(boots), want))
	}
	return f
}

// sparseCopy copies src to dst, keeping holes, and extends dst to size.
func sparseCopy(src, dst string, size int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	st, _ := in.Stat()
	if err := out.Truncate(max(size, st.Size())); err != nil {
		out.Close()
		return err
	}
	buf := make([]byte, 1<<20)
	zero := make([]byte, 1<<20)
	for off := int64(0); off < st.Size(); {
		n, err := in.ReadAt(buf, off)
		if n > 0 && !bytes.Equal(buf[:n], zero[:n]) {
			if _, werr := out.WriteAt(buf[:n], off); werr != nil {
				out.Close()
				return werr
			}
		}
		off += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			out.Close()
			return err
		}
	}
	return out.Close()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(s))
}

func lastLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " | ")
}

// ErrNoFirmware is returned when OVMF is missing.
var ErrNoFirmware = errors.New("tester: OVMF firmware not found")

// Check verifies the tools exist.
func (c Config) Check() error {
	for _, p := range []string{c.OVMFCode, c.OVMFVars} {
		if _, err := os.Stat(p); err != nil {
			return ErrNoFirmware
		}
	}
	for _, b := range []string{c.QEMU, c.QEMUImg, c.Swtpm} {
		if _, err := exec.LookPath(b); err != nil {
			return fmt.Errorf("tester: %s not installed", b)
		}
	}
	return nil
}
