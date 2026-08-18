#!/usr/bin/env bash
set -euo pipefail

# BroCode Semantic Version Bumper Script
# Usage:
#   ./scripts/bump_version.sh patch   # v0.1.0 -> v0.1.1
#   ./scripts/bump_version.sh minor   # v0.1.0 -> v0.2.0
#   ./scripts/bump_version.sh major   # v0.1.0 -> v1.0.0
#   ./scripts/bump_version.sh v0.2.5  # explicit version

VERSION_FILE="internal/version/version.go"

if [ ! -f "$VERSION_FILE" ]; then
    echo "❌ Error: $VERSION_FILE not found. Run this script from the repository root."
    exit 1
fi

CURRENT_RAW=$(grep -E 'Version\s*=\s*"[^"]+"' "$VERSION_FILE" | sed -E 's/.*"([^"]+)".*/\1/')
CURRENT_VER="${CURRENT_RAW#v}"

IFS='.' read -r MAJOR MINOR PATCH <<< "$CURRENT_VER"

TYPE="${1:-patch}"

case "$TYPE" in
    patch)
        PATCH=$((PATCH + 1))
        NEW_VER="v${MAJOR}.${MINOR}.${PATCH}"
        ;;
    minor)
        MINOR=$((MINOR + 1))
        PATCH=0
        NEW_VER="v${MAJOR}.${MINOR}.${PATCH}"
        ;;
    major)
        MAJOR=$((MAJOR + 1))
        MINOR=0
        PATCH=0
        NEW_VER="v${MAJOR}.${MINOR}.${PATCH}"
        ;;
    v[0-9]*)
        NEW_VER="$TYPE"
        ;;
    [0-9]*)
        NEW_VER="v$TYPE"
        ;;
    *)
        echo "Usage: $0 [patch | minor | major | vX.Y.Z]"
        exit 1
        ;;
esac

echo "🚀 Bumping version: $CURRENT_RAW -> $NEW_VER"

# Update version in internal/version/version.go
sed -i.bak -E "s/Version = \"[^\"]+\"/Version = \"$NEW_VER\"/" "$VERSION_FILE"
rm -f "${VERSION_FILE}.bak"

# Verify tests pass
echo "🧪 Running full test suite..."
go test ./...

echo "📦 Committing version bump..."
git add "$VERSION_FILE"
git commit -m "chore(release): bump version to $NEW_VER"

echo "🏷️ Tagging git release $NEW_VER..."
git tag -a "$NEW_VER" -m "Release $NEW_VER"

echo "✨ Successfully bumped version to $NEW_VER!"
echo "👉 To publish release tag, run: git push origin main --tags"
