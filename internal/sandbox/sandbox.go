// Package sandbox runs one image build in isolation (build-pipeline.md §4).
//
//   - VM (production): an ephemeral QEMU VM booted from the golden builder
//     image (read-only qcow2 backing file + per-build overlay). The job is a
//     tar on a virtio disk, outputs come back as a tar on another, build logs
//     stream over a virtio-serial port. The VM's only network path is a
//     guest-forward to the egress proxy (user networking with restrict=on).
//     The VM is killed and its disks deleted after the build.
//   - Process (development, CI without /dev/kvm): mkosi runs on the host in
//     its own namespaces, with downloads forced through the egress proxy.
package sandbox

import (
	"context"
	"io"

	"foundry.dev/foundry/internal/builder"
	"foundry.dev/foundry/internal/compiler"
)

// Job is one build to run.
type Job struct {
	BuildID    string
	SpecDigest string
	ImageName  string
	Tree       *compiler.Tree
	Resolver   compiler.Resolver
	Log        io.Writer
	// ProxyURL reaches the egress proxy from the host (process mode);
	// VM mode rewrites the address to the guest-forwarded one.
	ProxyURL  string
	ProxyAddr string // host:port of the egress proxy (VM mode guestfwd target)
	ProxyCred string // user:pass for the proxy
}

// Sandbox runs builds.
type Sandbox interface {
	Name() string
	Run(ctx context.Context, j Job) (*builder.Result, error)
}

// Process runs mkosi on the host.
type Process struct {
	Config builder.Config
}

// Name implements Sandbox.
func (p *Process) Name() string { return "process" }

// Run implements Sandbox.
func (p *Process) Run(ctx context.Context, j Job) (*builder.Result, error) {
	cfg := p.Config
	if j.ProxyURL != "" {
		cfg.Proxy = j.ProxyURL
	}
	return builder.Run(ctx, cfg, builder.Job{BuildID: j.BuildID, SpecDigest: j.SpecDigest, Tree: j.Tree, ImageName: j.ImageName, Resolver: j.Resolver, Log: j.Log})
}
