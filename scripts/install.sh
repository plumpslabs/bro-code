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
    LATEST_TAG="v0.1.45"
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
    # Verify checksum against the release's checksums.txt (GoReleaser-published)
    # so a tampered/mismatched binary is never installed.
    echo "🔐 Verifying checksum..."
    if curl -sLf "https://github.com/$REPO/releases/download/$LATEST_TAG/checksums.txt" -o "$TMP_DIR/checksums.txt"; then
        if command -v sha256sum >/dev/null 2>&1; then
            ACTUAL=$(sha256sum "$TMP_DIR/$ARCHIVE_NAME" | awk '{print $1}')
        elif command -v shasum >/dev/null 2>&1; then
            ACTUAL=$(shasum -a 256 "$TMP_DIR/$ARCHIVE_NAME" | awk '{print $1}')
        else
            ACTUAL=""
        fi
        if [ -n "$ACTUAL" ]; then
            EXPECTED=$(grep -E "(^|[^A-Za-z0-9])$ARCHIVE_NAME([^A-Za-z0-9]|$)" "$TMP_DIR/checksums.txt" | awk '{print $1}' | head -1)
            if [ -n "$EXPECTED" ] && [ "$EXPECTED" != "$ACTUAL" ]; then
                echo "❌ Checksum mismatch! expected $EXPECTED got $ACTUAL"
                exit 1
            fi
            echo "✅ Checksum verified ($ACTUAL)"
        else
            echo "⚠️ No sha256 tool available; skipping checksum verification"
        fi
    else
        echo "⚠️ checksums.txt not published; skipping verification"
    fi
    # GoReleaser wraps each archive in a per-build directory
    # (e.g. brocode_0.1.3_darwin_arm64/brocode), so locate the binary rather
    # than assuming it sits at the archive root. Handles .exe on Windows too.
    BIN=$(find "$TMP_DIR" -type f \( -name "$BINARY_NAME" -o -name "${BINARY_NAME}.exe" \) ! -name '*.txt' ! -name '*.md' ! -name 'LICENSE' | head -1)
    if [ -z "$BIN" ]; then
        echo "❌ Could not locate the $BINARY_NAME binary inside the archive."
        exit 1
    fi
    mv "$BIN" "$INSTALL_DIR/$BINARY_NAME"
    if [ "$OS" = "windows" ]; then
        mv "$INSTALL_DIR/$BINARY_NAME" "$INSTALL_DIR/${BINARY_NAME}.exe" 2>/dev/null || true
    fi
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
