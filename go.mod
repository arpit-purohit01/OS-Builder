module foundry.dev/foundry

go 1.24.0

toolchain go1.24.7

require (
	github.com/jackc/pgx/v5 v5.7.6
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	golang.org/x/crypto v0.37.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.30.0 // indirect
)

replace (
	golang.org/x/crypto => ./third_party/x-crypto
	golang.org/x/net => ./third_party/x-net
	golang.org/x/sync => ./third_party/x-sync
	golang.org/x/sys => ./third_party/x-sys
	golang.org/x/term => ./third_party/x-term
	golang.org/x/text => ./third_party/x-text
	gopkg.in/check.v1 => ./third_party/check.v1
	gopkg.in/yaml.v3 => ./third_party/yaml.v3
)
