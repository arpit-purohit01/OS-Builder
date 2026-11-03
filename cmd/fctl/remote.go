package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foundry.dev/foundry/internal/client"
)

// Remote commands talk to a Foundry API. Credentials are stored in
// $XDG_CONFIG_HOME/foundry/fctl.json (mode 0600) or taken from
// FOUNDRY_URL / FOUNDRY_TOKEN / FOUNDRY_ORG.
const remoteUsage = `Remote (API):
  fctl login -url URL -email EMAIL [-org ORG_ID | -create-org "Name:slug"]
  fctl whoami
  fctl projects | fctl project-create NAME OSID
  fctl push SPEC.yaml -project ID                 store a new spec version
  fctl submit SPEC.yaml -project ID [-formats qcow2,img.xz] [-follow]   push + build
  fctl builds | fctl status BUILD | fctl logs BUILD [-f] | fctl cancel BUILD
  fctl artifacts BUILD | fctl download BUILD [-kind qcow2] [-o DIR]
  fctl review BUILD approve|reject|changes_requested [-m COMMENT]
`

type remoteConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Org   string `json:"org"`
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "foundry", "fctl.json")
}

func loadRemote() (*client.Client, error) {
	var rc remoteConfig
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, &rc)
	}
	if v := os.Getenv("FOUNDRY_URL"); v != "" {
		rc.URL = v
	}
	if v := os.Getenv("FOUNDRY_TOKEN"); v != "" {
		rc.Token = v
	}
	if v := os.Getenv("FOUNDRY_ORG"); v != "" {
		rc.Org = v
	}
	if rc.URL == "" || rc.Token == "" || rc.Org == "" {
		return nil, errors.New("not logged in: run fctl login (or set FOUNDRY_URL, FOUNDRY_TOKEN, FOUNDRY_ORG)")
	}
	c := client.New(rc.URL)
	c.Token, c.Org = rc.Token, rc.Org
	return c, nil
}

func saveRemote(rc remoteConfig) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rc, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

var remoteCommands = map[string]func(context.Context, []string) error{
	"login": cmdLogin, "whoami": cmdWhoami, "projects": cmdProjects, "project-create": cmdProjectCreate,
	"push": cmdPush, "submit": cmdSubmit, "builds": cmdBuilds, "status": cmdStatus, "logs": cmdLogs,
	"cancel": cmdCancel, "artifacts": cmdArtifacts, "download": cmdDownload, "review": cmdReview,
}

// remote dispatches API subcommands.
func remote(ctx context.Context, args []string) error {
	if h, ok := remoteCommands[args[0]]; ok {
		return h(ctx, args[1:])
	}
	return fmt.Errorf("unknown command %q (see fctl help)", args[0])
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	u := fs.String("url", os.Getenv("FOUNDRY_URL"), "API URL")
	email := fs.String("email", "", "your email")
	org := fs.String("org", "", "organization ID to use")
	create := fs.String("create-org", "", `create an organization "Name:slug"`)
	fs.Parse(args)
	if *u == "" || *email == "" {
		return errors.New("usage: fctl login -url URL -email EMAIL")
	}
	c := client.New(*u)
	err := c.EmailLogin(ctx, *email, func() (string, error) {
		fmt.Fprint(os.Stderr, "Enter the 6-digit code sent to "+*email+": ")
		s, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimSpace(s), err
	})
	if err != nil {
		return err
	}
	if *create != "" {
		name, slug, ok := strings.Cut(*create, ":")
		if !ok {
			return errors.New(`-create-org expects "Name:slug"`)
		}
		if *org, err = c.CreateOrg(ctx, name, slug); err != nil {
			return err
		}
	}
	if *org == "" {
		var me struct {
			Memberships []struct {
				OrgID string `json:"orgId"`
				Role  string `json:"role"`
			} `json:"memberships"`
		}
		if _, err := c.Do(ctx, "GET", "/v1/me", nil, &me); err != nil {
			return err
		}
		switch len(me.Memberships) {
		case 0:
			return errors.New(`you are not in any organization: use -create-org "Name:slug"`)
		case 1:
			*org = me.Memberships[0].OrgID
		default:
			for _, m := range me.Memberships {
				fmt.Fprintf(os.Stderr, "  %s (%s)\n", m.OrgID, m.Role)
			}
			return errors.New("you are in several organizations: pass -org ID")
		}
	}
	c.Org = *org
	scopes := []string{"specs:write", "builds:write", "fleet:read"}
	tok, err := c.CreateToken(ctx, "fctl "+time.Now().UTC().Format("2006-01-02"), scopes, 90)
	if err != nil {
		return fmt.Errorf("create CLI token: %w", err)
	}
	if err := saveRemote(remoteConfig{URL: *u, Token: tok, Org: *org}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Logged in to %s (org %s). Token saved to %s\n", *u, *org, configPath())
	return nil
}

func cmdWhoami(ctx context.Context, _ []string) error {
	c, err := loadRemote()
	if err != nil {
		return err
	}
	me, err := c.Me(ctx)
	if err != nil {
		return err
	}
	printJSON(me)
	return nil
}

func cmdProjects(ctx context.Context, _ []string) error {
	c, err := loadRemote()
	if err != nil {
		return err
	}
	ps, err := c.Projects(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		fmt.Printf("%s  %-20s %s\n", p.ID, p.OSID, p.Name)
	}
	return nil
}

func cmdProjectCreate(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: fctl project-create NAME OSID")
	}
	c, err := loadRemote()
	if err != nil {
		return err
	}
	p, err := c.CreateProject(ctx, args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Println(p.ID)
	return nil
}

func pushFlags(name string, args []string) (*flag.FlagSet, *string, *string, *bool) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	project := fs.String("project", "", "project ID")
	formats := fs.String("formats", "", "comma-separated output formats (default: from hardware targets)")
	follow := fs.Bool("follow", false, "stream logs until the build finishes")
	return fs, project, formats, follow
}

func doPush(ctx context.Context, c *client.Client, file, project string) (*client.SpecVersion, error) {
	if project == "" {
		return nil, errors.New("-project is required")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	sv, created, err := c.PushSpec(ctx, project, b)
	if err != nil {
		return nil, err
	}
	if created {
		fmt.Fprintf(os.Stderr, "spec version %s (%s)\n", sv.ID, sv.Digest)
	} else {
		fmt.Fprintf(os.Stderr, "spec unchanged: version %s\n", sv.ID)
	}
	return sv, nil
}

func cmdPush(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: fctl push SPEC.yaml -project ID")
	}
	fs, project, _, _ := pushFlags("push", args)
	fs.Parse(args[1:])
	c, err := loadRemote()
	if err != nil {
		return err
	}
	sv, err := doPush(ctx, c, args[0], *project)
	if err == nil {
		fmt.Println(sv.ID)
	}
	return err
}

func cmdSubmit(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: fctl submit SPEC.yaml -project ID [-formats ...] [-follow]")
	}
	fs, project, formats, follow := pushFlags("submit", args)
	fs.Parse(args[1:])
	c, err := loadRemote()
	if err != nil {
		return err
	}
	sv, err := doPush(ctx, c, args[0], *project)
	if err != nil {
		return err
	}
	var fl []string
	if *formats != "" {
		fl = strings.Split(*formats, ",")
	}
	b, err := c.CreateBuild(ctx, sv.ID, fl, "")
	if err != nil {
		return err
	}
	fmt.Println(b.ID)
	if *follow {
		return followBuild(ctx, c, b.ID)
	}
	return nil
}

func followBuild(ctx context.Context, c *client.Client, id string) error {
	last := 0
	for {
		err := c.FollowLogs(ctx, id, last, func(l client.LogLine) {
			last = l.Seq
			fmt.Fprintf(os.Stderr, "[%s] %s\n", l.Stream, l.Line)
		}, func(s string) { fmt.Fprintf(os.Stderr, "== state: %s\n", s) })
		if err != nil && ctx.Err() == nil {
			time.Sleep(2 * time.Second) // reconnect after network errors
			continue
		}
		b, err := c.GetBuild(ctx, id)
		if err != nil {
			return err
		}
		switch b.State {
		case "published", "awaiting_review":
			fmt.Fprintf(os.Stderr, "build %s: %s\n", id, b.State)
			return nil
		case "failed", "cancelled", "rejected":
			return fmt.Errorf("build %s %s: %s", id, b.State, string(b.Failure))
		}
		time.Sleep(2 * time.Second)
	}
}

func cmdBuilds(ctx context.Context, _ []string) error {
	c, err := loadRemote()
	if err != nil {
		return err
	}
	bs, err := c.Builds(ctx)
	if err != nil {
		return err
	}
	for _, b := range bs {
		tier := ""
		if b.Tier != nil {
			tier = *b.Tier
		}
		fmt.Printf("%s  %-16s %-18s %s\n", b.ID, b.State, tier, b.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func oneArg(args []string, usage string) (string, []string, error) {
	if len(args) < 1 {
		return "", nil, errors.New("usage: " + usage)
	}
	return args[0], args[1:], nil
}

func cmdStatus(ctx context.Context, args []string) error {
	id, _, err := oneArg(args, "fctl status BUILD")
	if err != nil {
		return err
	}
	c, err := loadRemote()
	if err != nil {
		return err
	}
	var out map[string]any
	if _, err := c.Do(ctx, "GET", "/v1/orgs/"+c.Org+"/builds/"+id, nil, &out); err != nil {
		return err
	}
	printJSON(out)
	return nil
}

func cmdLogs(ctx context.Context, args []string) error {
	id, rest, err := oneArg(args, "fctl logs BUILD [-f]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	f := fs.Bool("f", false, "follow")
	fs.Parse(rest)
	c, err := loadRemote()
	if err != nil {
		return err
	}
	if *f {
		return followBuild(ctx, c, id)
	}
	var out struct {
		Items []client.LogLine
		State string
	}
	if _, err := c.Do(ctx, "GET", "/v1/orgs/"+c.Org+"/builds/"+id+"/logs?limit=5000", nil, &out); err != nil {
		return err
	}
	for _, l := range out.Items {
		fmt.Printf("[%s] %s\n", l.Stream, l.Line)
	}
	return nil
}

func cmdCancel(ctx context.Context, args []string) error {
	id, _, err := oneArg(args, "fctl cancel BUILD")
	if err != nil {
		return err
	}
	c, err := loadRemote()
	if err != nil {
		return err
	}
	return c.Cancel(ctx, id)
}

func cmdArtifacts(ctx context.Context, args []string) error {
	id, _, err := oneArg(args, "fctl artifacts BUILD")
	if err != nil {
		return err
	}
	c, err := loadRemote()
	if err != nil {
		return err
	}
	as, err := c.Artifacts(ctx, id)
	if err != nil {
		return err
	}
	for _, a := range as {
		dl := "-"
		if a.URL != "" {
			dl = "downloadable"
		}
		fmt.Printf("%-16s %12d  %-6v %-12s %s\n", a.Kind, a.Size, a.Signed, dl, a.Name)
	}
	return nil
}

func cmdDownload(ctx context.Context, args []string) error {
	id, rest, err := oneArg(args, "fctl download BUILD [-kind K] [-o DIR]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	kind := fs.String("kind", "", "artifact kind (default: all downloadable)")
	dir := fs.String("o", ".", "output directory")
	fs.Parse(rest)
	c, err := loadRemote()
	if err != nil {
		return err
	}
	as, err := c.Artifacts(ctx, id)
	if err != nil {
		return err
	}
	n := 0
	for _, a := range as {
		if a.URL == "" || (*kind != "" && a.Kind != *kind) {
			continue
		}
		p := filepath.Join(*dir, a.Name)
		fmt.Fprintf(os.Stderr, "downloading %s (%d bytes)\n", a.Name, a.Size)
		if err := c.Download(ctx, a, p); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("nothing downloadable (is the build published?)")
	}
	return nil
}

func cmdReview(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: fctl review BUILD approve|reject|changes_requested [-m COMMENT]")
	}
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	m := fs.String("m", "", "comment")
	fs.Parse(args[2:])
	c, err := loadRemote()
	if err != nil {
		return err
	}
	return c.Review(ctx, args[0], args[1], *m)
}
