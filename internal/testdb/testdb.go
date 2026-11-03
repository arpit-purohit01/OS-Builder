// Package testdb gives each test package a fresh, migrated Postgres database.
// Set FOUNDRY_TEST_PG to an admin DSN (default: local postgres on 127.0.0.1).
// Tests are skipped when Postgres is unreachable, unless FOUNDRY_REQUIRE_PG=1.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"foundry.dev/foundry/internal/ids"
	"foundry.dev/foundry/internal/store"
	"github.com/jackc/pgx/v5"
)

// AdminDSN is the DSN used to create/drop test databases.
func AdminDSN() string {
	if v := os.Getenv("FOUNDRY_TEST_PG"); v != "" {
		return v
	}
	return "postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable"
}

// New creates a database, migrates it and registers cleanup.
func New(t testing.TB) *store.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, AdminDSN())
	if err != nil {
		if os.Getenv("FOUNDRY_REQUIRE_PG") == "1" {
			t.Fatalf("postgres required: %v", err)
		}
		t.Skipf("postgres not available: %v", err)
	}
	name := "ftest_" + strings.ToLower(ids.Secret(8))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	u, _ := url.Parse(AdminDSN())
	u.Path = "/" + name
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		c, err := pgx.Connect(context.Background(), AdminDSN())
		if err == nil {
			c.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
			c.Close(context.Background())
		}
	})
	return db
}

// Seed is a minimal tenant: org, owner, project and one spec version.
type Seed struct {
	DB      *store.DB
	Org     *store.Org
	User    *store.User
	Project *store.Project
	Spec    *store.SpecVersion
}

// NewSeed creates a tenant named slug with a trivial spec version.
func NewSeed(t testing.TB, db *store.DB, slug string) *Seed {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	o, err := db.CreateOrg(ctx, "Org "+slug, slug, "pilot")
	must(err)
	u, err := db.UpsertUser(ctx, slug+"@example.com", slug)
	must(err)
	must(db.AddMember(ctx, o.ID, u.ID, "owner"))
	must(db.EnsureCatalogVersion(ctx, "testcat", []byte{1}, false))
	p, err := db.CreateProject(ctx, o.ID, "OS "+slug, "os-"+slug)
	must(err)
	spec := []byte(`{"apiVersion":"foundry.dev/v1alpha1","kind":"OSSpec","metadata":{"name":"os-` + slug + `","version":"1.0.0"},"foundation":{"base":"ubuntu-24.04","profile":"server","arch":["x86_64"]}}`)
	sv, _, err := db.CreateSpecVersion(ctx, &store.SpecVersion{OrgID: o.ID, ProjectID: p.ID, Digest: "sha256:" + strings.Repeat(fmt.Sprintf("%02x", len(slug)), 32),
		APIVersion: "foundry.dev/v1alpha1", Spec: spec, CatalogVersion: "testcat", Warnings: []byte("[]"), CreatedBy: u.ID})
	must(err)
	return &Seed{DB: db, Org: o, User: u, Project: p, Spec: sv}
}

// Build creates a queued build for the seed's spec.
func (s *Seed) Build(t testing.TB, priority int, minutes float64) *store.Build {
	t.Helper()
	b, err := s.DB.CreateBuild(context.Background(), &store.Build{OrgID: s.Org.ID, ProjectID: s.Project.ID, SpecVersionID: s.Spec.ID,
		Arch: "x86_64", CatalogVersion: "testcat", TreeDigest: "sha256:" + strings.Repeat("ab", 32), Priority: priority,
		PredictedMinutes: minutes, CreatedBy: s.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
