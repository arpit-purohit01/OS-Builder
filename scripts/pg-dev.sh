#!/bin/bash
# Starts (or initializes) a local development Postgres 16 on 127.0.0.1:5432 with trust auth.
set -euo pipefail
PGDIR=${PGDIR:-/var/lib/foundry/pg}
bin=/usr/lib/postgresql/16/bin
if pg_isready -h 127.0.0.1 -q 2>/dev/null; then exit 0; fi
mkdir -p "$PGDIR" && chown postgres "$PGDIR"
if [[ ! -f $PGDIR/data/PG_VERSION ]]; then
  su postgres -c "$bin/initdb -D $PGDIR/data -A trust -U postgres" >/dev/null
fi
rm -f "$PGDIR"/.s.PGSQL.5432*
su postgres -c "$bin/pg_ctl -D $PGDIR/data -o '-k $PGDIR -c listen_addresses=127.0.0.1' -l $PGDIR/log -w start" >/dev/null
echo "postgres ready on 127.0.0.1:5432"
