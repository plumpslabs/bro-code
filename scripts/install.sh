#!/usr/bin/env bash
set -euo pipefail

# BroCode Universal Installer
# Installs the latest binary release from GitHub to /usr/local/bin or ~/.local/bin

REPO="plumpslabs/bro-code"
BINARY_NAME="brocode"

# 1. Detect OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
    darwin) OS="darwin" ;;
    linux) OS="linux" ;;
    freebsd) OS="freebsd" ;;
    mingw*|msys*|cygwin*) OS="windows" ;;
    *)
        echo "❌ Unsupported operating system: $OS"
        exit 1
        ;;
esac

# 2. Detect Architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *)
        echo "❌ Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# 3. Target Install Directory
if [ -w "/usr/local/bin" ]; then
    INSTALL_DIR="/usr/local/bin"
elif [ -n "${XDG_BIN_HOME:-}" ] && [ -d "$XDG_BIN_HOME" ]; then
    INSTALL_DIR="$XDG_BIN_HOME"
else
    INSTALL_DIR="$HOME/.local/bin"
    mkdir -p "$INSTALL_DIR"
fi

echo "🚀 Installing BroCode for $OS/$ARCH into $INSTALL_DIR..."

# 4. Fetch latest release version
LATEST_TAG=$(curl -s "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)

if [ -z "$LATEST_TAG" ]; then
    LATEST_TAG="v0.1.1"
fi

EXT="tar.gz"
if [ "$OS" = "windows" ]; then
    EXT="zip"
fi

ARCHIVE_NAME="${BINARY_NAME}_${LATEST_TAG#v}_${OS}_${ARCH}.${EXT}"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$LATEST_TAG/$ARCHIVE_NAME"

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

echo "📥 Downloading $DOWNLOAD_URL..."
if curl -sLf "$DOWNLOAD_URL" -o "$TMP_DIR/$ARCHIVE_NAME"; then
    if [ "$EXT" = "zip" ]; then
        unzip -q "$TMP_DIR/$ARCHIVE_NAME" -d "$TMP_DIR"
    else
        tar -xzf "$TMP_DIR/$ARCHIVE_NAME" -C "$TMP_DIR"
    fi
    mv "$TMP_DIR/$BINARY_NAME" "$INSTALL_DIR/$BINARY_NAME"
    chmod +x "$INSTALL_DIR/$BINARY_NAME"
    echo "✅ BroCode successfully installed to $INSTALL_DIR/$BINARY_NAME"
else
    # Fallback to `go install` if pre-built binary is not yet available
    if command -v go >/dev/null 2>&1; then
        echo "⚠️ Binary download failed, compiling from source with Go..."
        go install "github.com/$REPO/cmd/brocode@$LATEST_TAG"
        echo "✅ Installed via Go to $(go env GOPATH)/bin/brocode"
    else
        echo "❌ Failed to download prebuilt binary. Please check release availability or install with Go."
        exit 1
    fi
fi

# 5. Verify installation
if command -v brocode >/dev/null 2>&1; then
    brocode --version
else
    echo "💡 Note: Please ensure $INSTALL_DIR is in your PATH."
    echo "   export PATH=\"\$PATH:$INSTALL_DIR\""
fi
