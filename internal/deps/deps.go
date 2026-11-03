// Package deps pins third-party imports so `go mod vendor` keeps them.
package deps

import (
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/santhosh-tekuri/jsonschema/v6"
	_ "gopkg.in/yaml.v3"
)
