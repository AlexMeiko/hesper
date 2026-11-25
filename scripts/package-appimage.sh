#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${VERSION:-0.1.0}"
ARCH="${ARCH:-x86_64}"
BIN_PATH="${ROOT_DIR}/bin/sql-manager"
APPDIR="${ROOT_DIR}/build/linux/AppDir"
OUT_DIR="${ROOT_DIR}/dist"
OUT_APPIMAGE="${OUT_DIR}/SQL-Manager-${VERSION}-${ARCH}.AppImage"

APPIMAGETOOL="${APPIMAGETOOL:-$(command -v appimagetool || echo "${HOME}/.local/bin/appimagetool")}"
if [[ ! -x "${APPIMAGETOOL}" ]]; then
  echo "error: appimagetool not found at ${APPIMAGETOOL}" >&2
  echo "Download from: https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage" >&2
  exit 1
fi

if [[ ! -x "${BIN_PATH}" ]]; then
  echo "==> Building frontend assets & native Linux GTK4 binary (${BIN_PATH})..."
  (cd "${ROOT_DIR}/frontend" && pnpm build)
  (cd "${ROOT_DIR}" && go build -trimpath -ldflags="-s -w" -o "${BIN_PATH}" .)
fi

# Ensure crisp 256x256 PNG icon exists (renders clean rounded blue database icon if missing)
PNG_ICON="${ROOT_DIR}/build/linux/sql-manager.png"
if [[ ! -f "${PNG_ICON}" ]]; then
  echo "==> Generating 256x256 PNG icon..."
  python3 - "${PNG_ICON}" <<'PYEOF'
import struct, sys, zlib

out_path = sys.argv[1]
W = H = 256
raw = bytearray()

def in_round_rect(x, y, x0, y0, x1, y1, r):
    cx = min(max(x, x0 + r), x1 - r)
    cy = min(max(y, y0 + r), y1 - r)
    return (x - cx) ** 2 + (y - cy) ** 2 <= r * r

def in_ellipse(x, y, cx, cy, rx, ry):
    return ((x - cx) / rx) ** 2 + ((y - cy) / ry) ** 2 <= 1.0

for y in range(H):
    raw.append(0)
    for x in range(W):
        if not in_round_rect(x, y, 16, 16, 240, 240, 48):
            raw.extend((0, 0, 0, 0))
            continue
        t = (x + y) / (W + H)
        px = (int(37 + (29 - 37) * t), int(99 + (78 - 99) * t), int(235 + (216 - 235) * t), 255)

        if in_ellipse(x, y, 128, 82, 64, 21):
            px = (255, 255, 255, 255)
        elif 82 <= y <= 116 and 64 <= x <= 192 and (y < 100 or in_ellipse(x, y, 128, 116, 64, 21)):
            if not in_ellipse(x, y, 128, 82, 64, 21):
                px = (219, 234, 254, 255)
        elif in_ellipse(x, y, 128, 116, 64, 21):
            px = (219, 234, 254, 255)

        if 126 <= y <= 156 and 64 <= x <= 192:
            if not in_ellipse(x, y, 128, 126, 64, 21):
                px = (191, 219, 254, 255)
        elif in_ellipse(x, y, 128, 156, 64, 21):
            px = (191, 219, 254, 255)

        if 166 <= y <= 186 and 64 <= x <= 192:
            if not in_ellipse(x, y, 128, 166, 64, 21):
                px = (147, 197, 253, 255)
        elif in_ellipse(x, y, 128, 186, 64, 21):
            px = (147, 197, 253, 255)

        raw.extend(px)

def chunk(tag, data):
    return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

png = bytearray(b"\x89PNG\r\n\x1a\n")
png += chunk(b"IHDR", struct.pack(">IIBBBBB", W, H, 8, 6, 0, 0, 0))
png += chunk(b"IDAT", zlib.compress(bytes(raw), 9))
png += chunk(b"IEND", b"")
with open(out_path, "wb") as f:
    f.write(png)
PYEOF
fi

echo "==> Assembling GTK4 AppDir at ${APPDIR}..."
rm -rf "${APPDIR}"
mkdir -p \
  "${APPDIR}/usr/bin" \
  "${APPDIR}/usr/share/applications" \
  "${APPDIR}/usr/share/icons/hicolor/256x256/apps" \
  "${APPDIR}/usr/share/icons/hicolor/scalable/apps" \
  "${APPDIR}/usr/share/metainfo"

install -m 0755 "${BIN_PATH}" "${APPDIR}/usr/bin/sql-manager"
install -m 0644 "${ROOT_DIR}/build/linux/sql-manager.desktop" "${APPDIR}/sql-manager.desktop"
install -m 0644 "${ROOT_DIR}/build/linux/sql-manager.desktop" "${APPDIR}/usr/share/applications/sql-manager.desktop"
install -m 0644 "${PNG_ICON}" "${APPDIR}/sql-manager.png"
install -m 0644 "${PNG_ICON}" "${APPDIR}/usr/share/icons/hicolor/256x256/apps/sql-manager.png"
install -m 0644 "${ROOT_DIR}/build/linux/sql-manager.svg" "${APPDIR}/usr/share/icons/hicolor/scalable/apps/sql-manager.svg"
install -m 0644 "${ROOT_DIR}/build/linux/io.sqlmanager.app.metainfo.xml" "${APPDIR}/usr/share/metainfo/io.sqlmanager.app.metainfo.xml"
ln -sf sql-manager.png "${APPDIR}/.DirIcon"

cat > "${APPDIR}/AppRun" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

HERE="$(dirname "$(readlink -f "$0")")"
BIN="${HERE}/usr/bin/sql-manager"

show_error_dialog() {
  local title="$1"
  local msg="$2"

  # Always output to stderr for terminal users
  printf '%s\n\n%s\n' "${title}" "${msg}" >&2

  # Show a native GUI dialog when running inside an X11/Wayland desktop session
  if [[ -n "${DISPLAY:-}" || -n "${WAYLAND_DISPLAY:-}" ]]; then
    if command -v zenity >/dev/null 2>&1; then
      zenity --error --title="${title}" --width=540 --no-markup --text="${msg}" 2>/dev/null || true
      return
    fi
    if command -v kdialog >/dev/null 2>&1; then
      kdialog --title "${title}" --error "${msg}" 2>/dev/null || true
      return
    fi
    if command -v yad >/dev/null 2>&1; then
      yad --error --title="${title}" --width=540 --text="${msg}" 2>/dev/null || true
      return
    fi
    if command -v xmessage >/dev/null 2>&1; then
      xmessage -center -title "${title}" "${msg}" 2>/dev/null || true
      return
    fi
    if command -v notify-send >/dev/null 2>&1; then
      notify-send -u critical "${title}" "${msg}" 2>/dev/null || true
      return
    fi
  fi
}

# Pre-flight check for missing GTK4 / WebKitGTK 6.0 shared libraries, GLIBC version, or undefined symbols
if command -v ldd >/dev/null 2>&1; then
  LDD_OUT="$(ldd -r "${BIN}" 2>&1 || true)"
  MISSING="$(printf '%s\n' "${LDD_OUT}" | awk '/not found/ {print "  • " $1}' | sort -u)"
  GLIBC_ERR="$(printf '%s\n' "${LDD_OUT}" | grep -E "version 'GLIBC_[^']+' not found" || true)"
  UNDEF_SYM="$(printf '%s\n' "${LDD_OUT}" | grep -E "^undefined symbol:" | head -n 5 || true)"

  if [[ -n "${MISSING}" || -n "${GLIBC_ERR}" || -n "${UNDEF_SYM}" ]]; then
    MSG="无法启动 SQL Manager，当前系统缺少必要的运行时依赖（需 GTK4 + WebKitGTK 6.0）。"
    if [[ -n "${MISSING}" ]]; then
      MSG+=$'\n\n缺失的动态库：\n'"${MISSING}"
    fi
    if [[ -n "${GLIBC_ERR}" ]]; then
      MSG+=$'\n\nGLIBC 版本过低（需 GLIBC >= 2.34）：\n'"${GLIBC_ERR}"
    fi
    if [[ -n "${UNDEF_SYM}" ]]; then
      MSG+=$'\n\n运行库符号版本不匹配：\n'"${UNDEF_SYM}"
    fi
    MSG+=$'\n\n请按您的发行版安装 GTK4 与 WebKitGTK 6.0 运行库后重试：\n'
    MSG+=$'• Ubuntu 24.04+ / Debian 13+:\n  sudo apt install libgtk-4-1 libwebkitgtk-6.0-4\n'
    MSG+=$'• CachyOS / Arch Linux:\n  sudo pacman -S gtk4 webkitgtk-6.0'
    show_error_dialog "SQL Manager — 缺少系统运行库" "${MSG}"
    exit 127
  fi
fi

exec "${BIN}" "$@"
EOF
chmod +x "${APPDIR}/AppRun"

if command -v desktop-file-validate >/dev/null 2>&1; then
  echo "==> Validating desktop entry..."
  desktop-file-validate "${APPDIR}/sql-manager.desktop"
fi

mkdir -p "${OUT_DIR}"
rm -f "${OUT_APPIMAGE}"

echo "==> Packaging GTK4 Type 2 AppImage with appimagetool..."
ARCH="${ARCH}" "${APPIMAGETOOL}" --appimage-extract-and-run --no-appstream "${APPDIR}" "${OUT_APPIMAGE}"
chmod +x "${OUT_APPIMAGE}"

(cd "${OUT_DIR}" && sha256sum "$(basename "${OUT_APPIMAGE}")" > SHA256SUMS)
echo "==> Built successfully:"
ls -lh "${OUT_APPIMAGE}" "${OUT_DIR}/SHA256SUMS"
cat "${OUT_DIR}/SHA256SUMS"
