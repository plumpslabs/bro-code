#!/usr/bin/env bash
set -euo pipefail

# BroCode Semantic Version Bumper Script
# Usage:
#   ./scripts/bump_version.sh patch   # v0.1.3 -> v0.1.4
#   ./scripts/bump_version.sh minor   # v0.1.3 -> v0.2.0
#   ./scripts/bump_version.sh major   # v0.1.3 -> v1.0.0
#   ./scripts/bump_version.sh v0.1.4  # explicit version

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

# 1. Update internal/version/version.go
sed -i.bak -E "s/Version = \"[^\"]+\"/Version = \"$NEW_VER\"/" "$VERSION_FILE"
rm -f "${VERSION_FILE}.bak"

# 2. Update docs/index.html
if [ -f "docs/index.html" ]; then
    sed -i.bak -E "s/<span class=\"brand-tag\">v[0-9]+\.[0-9]+\.[0-9]+<\/span>/<span class=\"brand-tag\">$NEW_VER<\/span>/g" docs/index.html
    sed -i.bak -E "s/BroCode v[0-9]+\.[0-9]+\.[0-9]+/BroCode $NEW_VER/g" docs/index.html
    rm -f "docs/index.html.bak"
fi

# 3. Update README.md badge
if [ -f "README.md" ]; then
    sed -i.bak -E "s/release-v[0-9]+\.[0-9]+\.[0-9]+/release-$NEW_VER/g" README.md
    rm -f "README.md.bak"
fi

# 4. Update docs markdown files
for doc in docs/ARCHITECTURE.md docs/CLI_REFERENCE.md; do
    if [ -f "$doc" ]; then
        sed -i.bak -E "s/\*\*Version\*\*: v[0-9]+\.[0-9]+\.[0-9]+/**Version**: $NEW_VER/g" "$doc"
        rm -f "${doc}.bak"
    fi
done

# 5. Update fallback tag in scripts/install.sh
if [ -f "scripts/install.sh" ]; then
    sed -i.bak -E "s/LATEST_TAG=\"v[0-9]+\.[0-9]+\.[0-9]+\"/LATEST_TAG=\"$NEW_VER\"/g" scripts/install.sh
    rm -f "scripts/install.sh.bak"
fi

# 6. Verify full test suite
echo "🧪 Running full test suite..."
go test ./...

echo "📦 Staging and committing version bump..."
git add -A
git commit -m "chore(release): bump version to $NEW_VER"

echo "🏷️ Creating git release tag $NEW_VER..."
git tag -a "$NEW_VER" -m "Release $NEW_VER"

echo "✨ Successfully bumped version to $NEW_VER!"
echo "👉 To publish release tag, run: git push origin main --tags"
