// Command fctl is the Foundry CLI: validate/compile specs locally, run local
// builds and boot tests, and talk to the API (see `fctl help`).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"time"

	"foundry.dev/foundry/internal/builder"
	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/osspec"
	"foundry.dev/foundry/internal/specsvc"
)

const usage = `fctl — Foundry CLI

Local:
  fctl validate <spec.yaml>                  validate all four layers, print digest and warnings
  fctl compile  <spec.yaml> -o <dir>         write the mkosi project tree
  fctl build    <spec.yaml> -w <workdir>     compile and build an image locally (root, loop devices)
  fctl catalog  [-base B] [-arch A]          print catalog CNF (JSON)
  fctl pcr      <uki.efi>                    predicted PCR 11 per boot phase

` + remoteUsage

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "compile":
		err = cmdCompile(os.Args[2:])
	case "build":
		err = cmdBuild(ctx, os.Args[2:])
	case "catalog":
		err = cmdCatalog(os.Args[2:])
	case "pcr":
		err = cmdPCR(ctx, os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = remote(ctx, os.Args[1:])
	}
	if err != nil {
		var ve *osspec.ValidationError
		if errors.As(err, &ve) {
			fmt.Fprintf(os.Stderr, "spec invalid (%s layer):\n", ve.Layer)
			for _, e := range ve.Errors {
				fmt.Fprintf(os.Stderr, "  %-8s %-32s %s\n", e.Code, e.Pointer, e.Message)
			}
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, "fctl:", err)
		os.Exit(1)
	}
}

func findRoot() string {
	if r := os.Getenv("FOUNDRY_ROOT"); r != "" {
		return r
	}
	d, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(d, "catalog")); err == nil {
			if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
				return d
			}
		}
		d = filepath.Dir(d)
	}
	return "."
}

func loadCatalog() (*catalog.Catalog, error) {
	dir := os.Getenv("FOUNDRY_CATALOG")
	if dir == "" {
		dir = filepath.Join(findRoot(), "catalog")
	}
	return catalog.LoadDir(dir)
}

func validateFile(path string) (*specsvc.Validated, *catalog.Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	cat, err := loadCatalog()
	if err != nil {
		return nil, nil, err
	}
	pol := specsvc.DefaultPolicy()
	pol.ExpertAllowed = os.Getenv("FOUNDRY_EXPERT") == "1"
	v, err := specsvc.Validate(data, cat, pol)
	return v, cat, err
}

func cmdValidate(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: fctl validate <spec>")
	}
	v, cat, err := validateFile(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("valid  %s\ncatalog %s\nselected %d options (%d implied), %d packages\n", v.Digest, cat.Version, len(v.Resolution.Selected), len(v.Resolution.Implied), len(v.Resolution.Packages))
	for _, w := range v.Warnings {
		fmt.Printf("warning %s %s: %s\n", w.Code, w.Pointer, w.Message)
	}
	return nil
}

func platformFromEnv() compiler.Platform {
	return compiler.Platform{
		Tenant:    envOr("FOUNDRY_TENANT", "dev"),
		FleetURL:  envOr("FOUNDRY_FLEET_URL", "https://fleet.foundry.test"),
		UpdateURL: envOr("FOUNDRY_UPDATE_URL", "https://updates.foundry.test"),
		SyncURL:   os.Getenv("FOUNDRY_SYNC_URL"),
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func cmdCompile(args []string) error {
	fs := flag.NewFlagSet("compile", flag.ExitOnError)
	out := fs.String("o", "", "output directory")
	if len(args) < 1 {
		return fmt.Errorf("usage: fctl compile <spec> -o <dir>")
	}
	fs.Parse(args[1:])
	if *out == "" {
		return fmt.Errorf("-o required")
	}
	v, cat, err := validateFile(args[0])
	if err != nil {
		return err
	}
	o, err := specsvc.Compile(v, cat.Version, platformFromEnv(), specsvc.Assets{})
	if err != nil {
		return err
	}
	if err := o.Tree.WriteDir(*out, stubResolver{}); err != nil {
		return err
	}
	fmt.Printf("compiled %s -> %s (tree %s)\n", v.Digest, *out, o.Tree.Digest())
	for _, w := range o.Warnings {
		fmt.Println("warning:", w)
	}
	return nil
}

func cmdBuild(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	work := fs.String("w", "/var/lib/foundry/builds", "work directory")
	cache := fs.String("cache", "", "mkosi cache directory (default <work>/cache)")
	keep := fs.Bool("keep", false, "keep the project tree")
	id := fs.String("id", "", "build id (default: timestamp)")
	if len(args) < 1 {
		return fmt.Errorf("usage: fctl build <spec> [-w dir]")
	}
	fs.Parse(args[1:])
	v, cat, err := validateFile(args[0])
	if err != nil {
		return err
	}
	o, err := specsvc.Compile(v, cat.Version, platformFromEnv(), specsvc.Assets{})
	if err != nil {
		return err
	}
	if *id == "" {
		*id = "local-" + time.Now().UTC().Format("20060102T150405")
	}
	cfg := builder.Config{
		WorkDir: *work, CacheDir: *cache, ToolsTree: envOr("FOUNDRY_TOOLS_TREE", "default"),
		ToolsTreeDist: "ubuntu", ToolsTreeRelease: "resolute", ToolsTreeMirror: "http://archive.ubuntu.com/ubuntu",
		Mkosi: envOr("FOUNDRY_MKOSI", "mkosi"), KeepWorkspace: *keep,
		DropProxyEnv: os.Getenv("FOUNDRY_DIRECT_MIRRORS") == "1",
	}
	res, err := builder.Run(ctx, cfg, builder.Job{BuildID: *id, SpecDigest: v.Digest, Tree: o.Tree, ImageName: o.ImageName, Resolver: localResolver{root: findRoot()}, Log: os.Stdout})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdCatalog(args []string) error {
	fs := flag.NewFlagSet("catalog", flag.ExitOnError)
	base := fs.String("base", "ubuntu-24.04", "base")
	arch := fs.String("arch", "x86_64", "arch")
	fs.Parse(args)
	cat, err := loadCatalog()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(cat.Compile(*base, *arch))
}

type stubResolver struct{}

func (stubResolver) Asset(d string) (r ioReadCloser, err error) {
	return strReader("asset placeholder " + d + "\n"), nil
}
func (stubResolver) Component(n string) (ioReadCloser, error) {
	return strReader("component placeholder " + n + "\n"), nil
}

// localResolver serves components from bin/ (built by `make components`).
type localResolver struct{ root string }

func (l localResolver) Asset(d string) (ioReadCloser, error) {
	return nil, fmt.Errorf("local builds cannot resolve uploaded asset %s; use the API", d)
}

func (l localResolver) Component(n string) (ioReadCloser, error) {
	p := filepath.Join(l.root, "bin", "components", n)
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("component %s not built (run `make components`): %w", n, err)
	}
	return f, nil
}
