#!/bin/bash
# Deploy tsstore to trv-pi-001
# Usage: ./deploy.sh [version]
# Example: ./deploy.sh v0.3.0-rc1

set -euo pipefail

VERSION="${1:?Usage: ./deploy.sh <version>}"
REPO="tviviano/ts-store"
BINARY_NAME="tsstore-linux-arm64"
INSTALL_DIR="$HOME/bin"
TSSTORE_DIR="$HOME/tsstore"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

SERVICES=(
    tsstore
    journal-to-tsstore
    sensehat-to-tsstore
    system-stats-to-tsstore
)

echo "=== Deploying tsstore $VERSION ==="

# Create directory structure
echo "Creating directory structure..."
mkdir -p "$TSSTORE_DIR/scripts"
mkdir -p "$TSSTORE_DIR/services"
mkdir -p "$INSTALL_DIR"

# Copy scripts and services from this repo checkout
echo "Copying scripts..."
cp "$SCRIPT_DIR/journal-to-tsstore.sh" "$TSSTORE_DIR/scripts/"
cp "$SCRIPT_DIR/sensehat-to-tsstore.py" "$TSSTORE_DIR/scripts/"
cp "$SCRIPT_DIR/system-stats-to-tsstore.py" "$TSSTORE_DIR/scripts/"
chmod +x "$TSSTORE_DIR/scripts/"*.sh "$TSSTORE_DIR/scripts/"*.py

echo "Copying service files..."
cp "$SCRIPT_DIR/../services/"*.service "$TSSTORE_DIR/services/"

# Download binary from GitHub release
echo "Downloading $BINARY_NAME $VERSION..."
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/$BINARY_NAME"
curl -fSL "$DOWNLOAD_URL" -o "/tmp/$BINARY_NAME"
chmod +x "/tmp/$BINARY_NAME"

# Stop services
echo "Stopping services..."
for svc in "${SERVICES[@]}"; do
    systemctl --user stop "$svc" 2>/dev/null || true
done

# Install binary
echo "Installing binary..."
mv "/tmp/$BINARY_NAME" "$INSTALL_DIR/tsstore"

# Install systemd service files
echo "Installing systemd services..."
mkdir -p "$HOME/.config/systemd/user"
for svc in "${SERVICES[@]}"; do
    cp "$TSSTORE_DIR/services/$svc.service" "$HOME/.config/systemd/user/"
done
systemctl --user daemon-reload

# Enable and start services
echo "Starting services..."
for svc in "${SERVICES[@]}"; do
    systemctl --user enable "$svc"
    systemctl --user start "$svc"
done

# Verify
echo ""
echo "=== Verification ==="
"$INSTALL_DIR/tsstore" version
echo ""
for svc in "${SERVICES[@]}"; do
    status=$(systemctl --user is-active "$svc" 2>/dev/null || echo "inactive")
    echo "$svc: $status"
done

echo ""
echo "Deploy complete!"
