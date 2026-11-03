# Foundry OS Builder Development Container
# Provides a complete Ubuntu 24.04 environment with all tools needed for Phase 0-1 development
FROM ubuntu:24.04

ARG USER_UID=1000
ARG USER_GID=1000

# Prevent interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install base packages and development tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    # Build essentials
    build-essential \
    git \
    curl \
    wget \
    ca-certificates \
    gnupg \
    lsb-release \
    software-properties-common \
    # Go toolchain
    golang-go \
    # QEMU/KVM for boot testing
    qemu-system-x86 \
    qemu-utils \
    ovmf \
    swtpm \
    swtpm-tools \
    libvirt-daemon-system \
    libvirt-clients \
    bridge-utils \
    # mkosi and image building tools
    systemd-boot-efi \
    systemd-ukify \
    sbsigntool \
    efitools \
    openssl \
    mtools \
    dosfstools \
    cryptsetup-bin \
    e2fsprogs \
    fdisk \
    zstd \
    xz-utils \
    # PostgreSQL
    postgresql-16 \
    postgresql-client-16 \
    # Python for scripts
    python3 \
    python3-pip \
    python3-venv \
    # Shell tools
    shellcheck \
    jq \
    # Node.js for web frontend
    nodejs \
    npm \
    # Additional utilities
    vim \
    htop \
    net-tools \
    iproute2 \
    && rm -rf /var/lib/apt/lists/*

# Install Go tools
RUN go install golang.org/x/tools/cmd/goimports@latest \
    && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest \
    && go install github.com/pressly/goose/v3/cmd/goose@latest

# Install mkosi from pinned version (matching deploy/versions.lock)
ARG MKOSI_VERSION=24
RUN pip3 install --no-cache-dir "mkosi==${MKOSI_VERSION}.*"

# Create vscode user with same UID/GID as host
RUN groupadd --gid ${USER_GID} vscode \
    && useradd --uid ${USER_UID} --gid ${USER_GID} -m -s /bin/bash vscode \
    && usermod -aG kvm,libvirt,docker vscode

# Configure libvirt for non-root usage
RUN sed -i 's/#unix_sock_group = "libvirt"/unix_sock_group = "libvirt"/' /etc/libvirt/libvirtd.conf \
    && sed -i 's/#unix_sock_rw_perms = "0770"/unix_sock_rw_perms = "0770"/' /etc/libvirt/libvirtd.conf

# Set up PostgreSQL data directory permissions
RUN mkdir -p /var/lib/postgresql/16/main \
    && chown -R postgres:postgres /var/lib/postgresql

# Configure sudo for vscode user (needed for some operations)
RUN echo "vscode ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/vscode \
    && chmod 0440 /etc/sudoers.d/vscode

# Create workspace directory
WORKDIR /workspaces/OS-Builder

# Switch to vscode user
USER vscode

# Set up Go environment
ENV GOPATH=/home/vscode/go
ENV PATH="${GOPATH}/bin:/usr/local/go/bin:${PATH}"

# Configure git
RUN git config --global --add safe.directory /workspaces/OS-Builder

# Default command
CMD ["sleep", "infinity"]