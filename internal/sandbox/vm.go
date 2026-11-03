package sandbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"foundry.dev/foundry/internal/procreg"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"foundry.dev/foundry/internal/builder"
	"foundry.dev/foundry/internal/compiler"
)

// GuestProxy is the address the egress proxy appears at inside build VMs.
const GuestProxy = "10.0.2.100:3128"

// JobSpec is job.json inside the job disk.
type JobSpec struct {
	BuildID    string `json:"buildId"`
	SpecDigest string `json:"specDigest"`
	ImageName  string `json:"imageName"`
	Proxy      string `json:"proxy,omitempty"`
	TimeoutSec int    `json:"timeoutSec"`
	ZstdLevel  int    `json:"zstdLevel"`
	SelfTest   bool   `json:"selfTest,omitempty"` // boot, fetch through the proxy, return; no mkosi
}

// GuestResult is result.json inside the output disk.
type GuestResult struct {
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Roothash string            `json:"roothash,omitempty"`
	Files    map[string]string `json:"files,omitempty"` // kind → file name in the tar
	Seconds  float64           `json:"seconds"`
	SelfTest string            `json:"selfTest,omitempty"`
}

// VM runs builds in ephemeral QEMU VMs.
type VM struct {
	Image      string // golden builder image (qcow2), never written
	QEMU       string
	QEMUImg    string
	OVMFCode   string
	OVMFVars   string
	Accel      string // kvm | tcg | "" auto
	MemoryMiB  int
	CPUs       int
	ScratchGiB int
	OutGiB     int
	Timeout    time.Duration
	WorkDir    string
	ZstdLevel  int
	// PipeCmd connects one guest connection (stdin/stdout) to the egress
	// proxy; QEMU runs it per connection (guestfwd ...-cmd:). Default:
	// this executable's "egress-pipe ADDR" subcommand.
	PipeCmd []string
	Procs   procreg.Dir // records the VM's process group for the crash reaper
	// NoSeccomp disables QEMU's seccomp sandbox (debugging only).
	NoSeccomp bool
}

// Name implements Sandbox.
func (v *VM) Name() string { return "vm" }

func (v *VM) defaults() {
	if v.QEMU == "" {
		v.QEMU = "qemu-system-x86_64"
	}
	if v.QEMUImg == "" {
		v.QEMUImg = "qemu-img"
	}
	if v.OVMFCode == "" {
		v.OVMFCode = "/usr/share/OVMF/OVMF_CODE_4M.fd"
	}
	if v.OVMFVars == "" {
		v.OVMFVars = "/usr/share/OVMF/OVMF_VARS_4M.fd"
	}
	if v.MemoryMiB == 0 {
		v.MemoryMiB = 8192
	}
	if v.CPUs == 0 {
		v.CPUs = 4
	}
	if v.ScratchGiB == 0 {
		v.ScratchGiB = 60
	}
	if v.OutGiB == 0 {
		v.OutGiB = 12
	}
	if v.Timeout == 0 {
		v.Timeout = 90 * time.Minute
	}
	if v.Accel == "" {
		v.Accel = "tcg"
		if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
			f.Close()
			v.Accel = "kvm"
		}
	}
}

// writeJobTar writes the job disk: job.json, tree.tar, resolved assets and components.
func writeJobTar(path string, spec JobSpec, t *compiler.Tree, r compiler.Resolver) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	addReader := func(name string, rc io.ReadCloser) error {
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 2<<30))
		if err != nil {
			return err
		}
		return add(name, b)
	}
	js, _ := json.Marshal(spec)
	if err := add("job.json", js); err != nil {
		f.Close()
		return err
	}
	var tb bytes.Buffer
	if err := t.WriteTar(&tb); err != nil {
		f.Close()
		return err
	}
	if err := add("tree.tar", tb.Bytes()); err != nil {
		f.Close()
		return err
	}
	seen := map[string]bool{}
	for _, p := range t.Paths() {
		fi := t.Files[p]
		switch {
		case fi.AssetRef != "" && !seen[fi.AssetRef]:
			seen[fi.AssetRef] = true
			rc, err := r.Asset(fi.AssetRef)
			if err != nil {
				f.Close()
				return err
			}
			if err := addReader("assets/"+strings.TrimPrefix(fi.AssetRef, "sha256:"), rc); err != nil {
				f.Close()
				return err
			}
		case fi.ComponentRef != "" && !seen["c:"+fi.ComponentRef]:
			seen["c:"+fi.ComponentRef] = true
			rc, err := r.Component(fi.ComponentRef)
			if err != nil {
				f.Close()
				return err
			}
			if err := addReader("components/"+fi.ComponentRef, rc); err != nil {
				f.Close()
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return err
	}
	// round up to whole sectors
	st, _ := f.Stat()
	if pad := (512 - st.Size()%512) % 512; pad > 0 {
		f.Write(make([]byte, pad))
	}
	return f.Close()
}

// Run implements Sandbox.
func (v *VM) Run(ctx context.Context, j Job) (*builder.Result, error) {
	return v.run(ctx, j, false)
}

// SelfTest boots the builder VM, checks the egress path and returns.
func (v *VM) SelfTest(ctx context.Context, j Job) (*GuestResult, error) {
	j.Tree = &compiler.Tree{Files: map[string]compiler.File{}}
	res, err := v.run(ctx, j, true)
	if err != nil {
		return nil, err
	}
	var gr GuestResult
	b, err := os.ReadFile(filepath.Join(res.Dir, "result.json"))
	if err != nil {
		return nil, err
	}
	os.RemoveAll(res.Dir)
	return &gr, json.Unmarshal(b, &gr)
}

func (v *VM) run(ctx context.Context, j Job, selfTest bool) (*builder.Result, error) {
	v.defaults()
	start := time.Now()
	if v.Image == "" {
		return nil, errors.New("sandbox: no builder VM image configured")
	}
	img, err := filepath.Abs(v.Image)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(v.WorkDir, "vm-"+j.BuildID+"-")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	proxy := ""
	if j.ProxyAddr != "" {
		proxy = "http://" + j.ProxyCred + "@" + GuestProxy
		if j.ProxyCred == "" {
			proxy = "http://" + GuestProxy
		}
	}
	spec := JobSpec{BuildID: j.BuildID, SpecDigest: j.SpecDigest, ImageName: j.ImageName, Proxy: proxy,
		TimeoutSec: int(v.Timeout.Seconds()), ZstdLevel: v.ZstdLevel, SelfTest: selfTest}
	jobDisk := filepath.Join(dir, "job.tar")
	if err := writeJobTar(jobDisk, spec, j.Tree, j.Resolver); err != nil {
		return nil, fmt.Errorf("sandbox: job disk: %w", err)
	}
	overlay := filepath.Join(dir, "root.qcow2")
	if out, err := exec.CommandContext(ctx, v.QEMUImg, "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", img, overlay).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sandbox: overlay: %v: %s", err, out)
	}
	scratch, outDisk := filepath.Join(dir, "scratch.raw"), filepath.Join(dir, "out.raw")
	for p, gib := range map[string]int{scratch: v.ScratchGiB, outDisk: v.OutGiB} {
		f, err := os.Create(p)
		if err != nil {
			return nil, err
		}
		f.Truncate(int64(gib) << 30)
		f.Close()
	}
	vars := filepath.Join(dir, "vars.fd")
	if b, err := os.ReadFile(v.OVMFVars); err == nil {
		os.WriteFile(vars, b, 0o600)
	} else {
		return nil, err
	}
	if os.Getenv("FOUNDRY_VM_KEEP") != "" {
		ok = true // keep the VM directory for debugging
	}
	logPath := filepath.Join(dir, "build.log")
	os.WriteFile(logPath, nil, 0o600)
	console := filepath.Join(dir, "console.log")
	net := "user,id=n0,restrict=on"
	if j.ProxyAddr != "" {
		pipe := v.PipeCmd
		if len(pipe) == 0 {
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			pipe = []string{exe, "egress-pipe"}
		}
		cmdline := strings.Join(append(append([]string{}, pipe...), j.ProxyAddr), " ")
		if strings.ContainsAny(cmdline, ",\n") {
			return nil, errors.New("sandbox: pipe command must not contain commas")
		}
		net += ",guestfwd=tcp:" + GuestProxy + "-cmd:" + cmdline
	}
	args := []string{"-machine", "q35", "-m", fmt.Sprint(v.MemoryMiB), "-smp", fmt.Sprint(v.CPUs),
		"-display", "none", "-monitor", "none", "-nodefaults", "-no-user-config", "-no-reboot",
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=" + v.OVMFCode,
		"-drive", "if=pflash,format=raw,unit=1,file=" + vars,
		"-drive", "if=none,id=root,format=qcow2,file=" + overlay, "-device", "virtio-blk-pci,drive=root,bootindex=1",
		"-drive", "if=none,id=job,format=raw,readonly=on,file=" + jobDisk, "-device", "virtio-blk-pci,drive=job,serial=job",
		"-drive", "if=none,id=scratch,format=raw,file=" + scratch, "-device", "virtio-blk-pci,drive=scratch,serial=scratch",
		"-drive", "if=none,id=out,format=raw,file=" + outDisk, "-device", "virtio-blk-pci,drive=out,serial=out",
		"-netdev", net, "-device", "virtio-net-pci,netdev=n0",
		"-device", "virtio-rng-pci",
		"-device", "virtio-serial-pci", "-chardev", "file,id=log,path=" + logPath, "-device", "virtserialport,chardev=log,name=foundry.log",
		"-serial", "file:" + console,
	}
	if !v.NoSeccomp {
		args = append(args, "-sandbox", v.seccompProfile())
	}
	if v.Accel == "kvm" {
		args = append(args, "-accel", "kvm", "-cpu", "host")
	} else {
		args = append(args, "-accel", "tcg,thread=multi", "-cpu", "max")
	}
	vctx, cancel := context.WithTimeout(ctx, v.Timeout+5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(vctx, v.QEMU, args...)
	var qerr bytes.Buffer
	cmd.Stderr = &qerr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer v.Procs.Register("buildvm-"+j.BuildID, cmd.Process.Pid)()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	logw := j.Log
	if logw == nil {
		logw = io.Discard
	}
	fmt.Fprintf(logw, "foundry: build VM started (%s, %d MiB, %d vCPU, network: egress proxy only)\n", v.Accel, v.MemoryMiB, v.CPUs)
	var off int64
	pump := func() {
		f, err := os.Open(logPath)
		if err != nil {
			return
		}
		defer f.Close()
		f.Seek(off, io.SeekStart)
		br := bufio.NewReader(f)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break // partial line: read again next time
			}
			off += int64(len(line))
			io.WriteString(logw, builder.Redact(strings.TrimRight(line, "\r\n"))+"\n")
		}
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var exitErr error
loop:
	for {
		select {
		case exitErr = <-done:
			break loop
		case <-vctx.Done():
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			pump()
			return nil, fmt.Errorf("%w: build VM timed out after %s", builder.ErrBuildFailed, v.Timeout)
		case <-t.C:
			pump()
		}
	}
	pump()
	if exitErr != nil {
		return nil, fmt.Errorf("sandbox: VM exited: %v: %s", exitErr, lastLines(qerr.String(), 5))
	}
	outDir := filepath.Join(dir, "out")
	if err := extractTar(outDisk, outDir); err != nil {
		return nil, fmt.Errorf("sandbox: VM produced no output (see console): %w", err)
	}
	os.Remove(outDisk)
	os.Remove(scratch)
	os.Remove(overlay)
	b, err := os.ReadFile(filepath.Join(outDir, "result.json"))
	if err != nil {
		return nil, errors.New("sandbox: no result.json from the build VM")
	}
	var gr GuestResult
	if err := json.Unmarshal(b, &gr); err != nil {
		return nil, err
	}
	if selfTest {
		os.Rename(filepath.Join(outDir, "result.json"), filepath.Join(dir, "result.json"))
		ok = true
		return &builder.Result{Dir: dir}, nil
	}
	if !gr.OK {
		return nil, fmt.Errorf("%w: %s", builder.ErrBuildFailed, gr.Error)
	}
	res := &builder.Result{Dir: dir, Roothash: gr.Roothash, Artifacts: map[string]builder.Artifact{}, Duration: time.Since(start), TreeDigest: j.Tree.Digest()}
	for kind, name := range gr.Files {
		if strings.ContainsAny(name, "/\\") || name == "" {
			return nil, fmt.Errorf("sandbox: bad output name %q", name)
		}
		p := filepath.Join(outDir, name)
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("sandbox: missing output %s", name)
		}
		res.Artifacts[kind] = builder.Artifact{Kind: kind, Path: p, Size: st.Size()}
	}
	if _, ok := res.Artifacts["disk.zst"]; !ok {
		return nil, errors.New("sandbox: build VM returned no disk image")
	}
	ok = true
	return res, nil
}

// extractTar reads a tar stream from the start of a raw disk (untrusted:
// only regular files directly in the root, bounded sizes).
func extractTar(disk, dir string) error {
	f, err := os.Open(disk)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(f)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || strings.ContainsAny(h.Name, "/\\") || h.Name == "" || h.Name[0] == '.' || h.Size > 16<<30 {
			return fmt.Errorf("unexpected entry %q in VM output", h.Name)
		}
		if n++; n > 64 {
			return errors.New("too many files in VM output")
		}
		out, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, h.Size)); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
	if n == 0 {
		return errors.New("empty output")
	}
	return nil
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " | ")
}

// Pipe copies stdin/stdout to a TCP connection (one guest connection to the
// egress proxy). Used as "worker-agent egress-pipe ADDR".
func Pipe(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(c, os.Stdin); c.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(os.Stdout, c); done <- struct{}{} }()
	<-done
	<-done
	return nil
}

// seccompProfile returns QEMU's -sandbox argument. FOUNDRY_VM_SECCOMP overrides
// the default (used to qualify profiles on new QEMU versions).
func (v *VM) seccompProfile() string {
	if p := os.Getenv("FOUNDRY_VM_SECCOMP"); p != "" {
		return p
	}
	return "on"
}
