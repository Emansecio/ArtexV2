#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
MACOS_DIR="$ROOT_DIR/desktop/macos"
APP_OUT="${1:-/Applications/Artex.app}"
ICON_SRC="$ROOT_DIR/web/src/app/icon.png"

echo "[*] Building native macOS app -> $APP_OUT"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# 1. Generate AppIcon.icns from icon.png
ICONSET="$TMP_DIR/AppIcon.iconset"
mkdir -p "$ICONSET"
for sz in 16 32 128 256 512; do
  sips -z "$sz" "$sz" "$ICON_SRC" --out "$ICONSET/icon_${sz}x${sz}.png" >/dev/null
  sz2=$((sz * 2))
  sips -z "$sz2" "$sz2" "$ICON_SRC" --out "$ICONSET/icon_${sz}x${sz}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$TMP_DIR/AppIcon.icns"

# 2. Compile Swift binary
swiftc -O \
  -framework Cocoa \
  -framework WebKit \
  "$MACOS_DIR/main.swift" \
  -o "$TMP_DIR/Artex"

# 3. Assemble .app bundle
rm -rf "$APP_OUT"
mkdir -p "$APP_OUT/Contents/MacOS" "$APP_OUT/Contents/Resources"
cp "$TMP_DIR/Artex" "$APP_OUT/Contents/MacOS/Artex"
chmod +x "$APP_OUT/Contents/MacOS/Artex"
cp "$MACOS_DIR/Info.plist" "$APP_OUT/Contents/Info.plist"
cp "$TMP_DIR/AppIcon.icns" "$APP_OUT/Contents/Resources/AppIcon.icns"

# 4. Ad-hoc sign so macOS LaunchServices / Dock treat it cleanly
codesign --force --deep --sign - "$APP_OUT" >/dev/null 2>&1 || true

echo "[+] Built: $APP_OUT"
