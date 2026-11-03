#!/bin/bash
# Post-create script for devcontainer - runs once after container creation
set -euo pipefail

echo "=== Foundry OS Builder DevContainer Setup ==="

# Initialize PostgreSQL
echo "Initializing PostgreSQL..."
sudo mkdir -p /var/lib/postgresql/16/main
sudo chown -R postgres:postgres /var/lib/postgresql/16/main
if [ ! -f /var/lib/postgresql/16/main/PG_VERSION ]; then
    sudo -u postgres /usr/lib/postgresql/16/bin/initdb -D /var/lib/postgresql/16/main -A trust -U postgres
fi

# Start PostgreSQL
echo "Starting PostgreSQL..."
sudo -u postgres /usr/lib/postgresql/16/bin/pg_ctl -D /var/lib/postgresql/16/main -o "-k /var/run/postgresql -c listen_addresses=*" -l /var/log/postgresql.log start

# Create foundry database
echo "Creating foundry database..."
sudo -u postgres psql -c "CREATE DATABASE foundry;" 2>/dev/null || true

# Build Go binaries
echo "Building Go binaries..."
cd /workspaces/OS-Builder
make build components

# Install web dependencies
echo "Installing web dependencies..."
cd /workspaces/OS-Builder/web && npm install

# Make tools tree
echo "Creating tools tree..."
make tools-tree

# Set up libvirt
echo "Setting up libvirt..."
sudo systemctl enable libvirtd 2>/dev/null || true
sudo systemctl start libvirtd 2>/dev/null || true

# Verify KVM access
echo "Checking KVM access..."
if [ -e /dev/kvm ]; then
    echo "KVM device available: $(ls -la /dev/kvm)"
else
    echo "WARNING: /dev/kvm not available - boot tests will use TCG (slow)"
fi

echo "=== DevContainer setup complete! ==="
echo "Run 'make dev-up' to start the development stack"
echo "Run 'make e2e' to run end-to-end tests"