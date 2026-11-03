#!/bin/bash
# Post-start script for devcontainer - runs each time container starts
set -euo pipefail

echo "=== Starting Foundry DevContainer Services ==="

# Start PostgreSQL if not running
if ! pg_isready -h 127.0.0.1 -q 2>/dev/null; then
    echo "Starting PostgreSQL..."
    sudo -u postgres /usr/lib/postgresql/16/bin/pg_ctl -D /var/lib/postgresql/16/main -o "-k /var/run/postgresql -c listen_addresses=*" -l /var/log/postgresql.log start
    sleep 2
fi

# Start libvirtd if not running
if ! systemctl is-active --quiet libvirtd 2>/dev/null; then
    echo "Starting libvirtd..."
    sudo systemctl start libvirtd 2>/dev/null || sudo /usr/sbin/libvirtd -d
    sleep 2
fi

# Verify services
echo "Verifying services..."
pg_isready -h 127.0.0.1 && echo "PostgreSQL: OK" || echo "PostgreSQL: FAILED"
virsh list --all >/dev/null 2>&1 && echo "libvirtd: OK" || echo "libvirtd: FAILED"

echo "=== DevContainer ready ==="