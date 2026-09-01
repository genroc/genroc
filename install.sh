#!/bin/sh
# Installs genctl. https://genroc.org/install.sh
#
#   curl -fsSL https://genroc.org/install.sh | sh                        newest stable
#   curl -fsSL https://genroc.org/install.sh | sh -s -- --preview         newest prerelease
#   curl -fsSL https://genroc.org/install.sh | sh -s -- --edge            tip of main
#   curl -fsSL https://genroc.org/install.sh | sh -s -- --version 0.1.0
#   curl -fsSL https://genroc.org/install.sh | sh -s -- --uninstall [--purge]
#
# $GENROC_VERSION does the same as --version, for environments that cannot pass arguments.
#
# The SERVER is a container (ghcr.io/genroc/genroc) — this installs the client only.
set -eu

REPO=genroc/genroc
BIN=genctl
VERSION=${GENROC_VERSION:-}
INSTALL_DIR=${GENROC_INSTALL_DIR:-}

die() { echo "install: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

# genctl's own config directory, matching Go's os.UserConfigDir. It holds an API TOKEN, which
# is why uninstall reports it rather than leaving a credential behind silently.
config_dir() {
  if [ "$(uname -s)" = Darwin ]; then echo "$HOME/Library/Application Support/genroc"
  else echo "${XDG_CONFIG_HOME:-$HOME/.config}/genroc"
  fi
}

uninstall() {
  found=""
  for d in ${GENROC_INSTALL_DIR:-} /usr/local/bin "$HOME/.local/bin" "$HOME/bin"; do
    [ -n "$d" ] && [ -f "$d/$BIN" ] || continue
    rm -f "$d/$BIN" && echo "removed $d/$BIN" && found=1
  done
  # Anything still on PATH was installed by something else — a package manager, or by hand.
  other=$(command -v "$BIN" 2>/dev/null || true)
  [ -z "$other" ] || echo "note: $BIN is still on your PATH at $other"
  [ -n "$found" ] || [ -n "$other" ] || echo "no $BIN found in the usual locations"

  cfg=$(config_dir)
  if [ -d "$cfg" ]; then
    if [ "${PURGE:-}" = 1 ]; then
      rm -rf "$cfg" && echo "removed $cfg"
    else
      echo
      echo "  config kept at $cfg"
      echo "  it holds an API token."
      echo "  hint: remove it too by re-running with --purge"
    fi
  fi
  exit 0
}

PURGE=
while [ $# -gt 0 ]; do
  case "$1" in
    --uninstall)   DO_UNINSTALL=1 ;;
    --purge)       PURGE=1 ;;
    --preview|--prerelease) VERSION=preview ;;
    --edge)        VERSION=edge ;;
    --version)     shift; [ $# -gt 0 ] || die "--version needs a value"; VERSION=$1 ;;
    --version=*)   VERSION=${1#--version=} ;;
    -h|--help)     sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
  shift
done
[ "${DO_UNINSTALL:-}" = 1 ] && uninstall

need curl
need tar

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64)   arch=amd64 ;;
  aarch64|arm64)  arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS: $os — Windows builds are attached to the GitHub release" ;;
esac

# grep -o, not sed: on a single-line JSON body a greedy `.*` captures the LAST tag_name rather
# than the first, which silently picks the oldest release in a list.
first_tag() { grep -o '"tag_name" *: *"[^"]*"' | head -1 | cut -d'"' -f4; }
version_tag() { grep -o '"tag_name" *: *"v[^"]*"' | head -1 | cut -d'"' -f4; }

# /releases/latest excludes prereleases, and during 0.x there may be nothing else. Fall back to
# the newest release of any kind rather than reporting "not found" for a project that has them.
api="https://api.github.com/repos/$REPO/releases"
prerelease=
case "${VERSION:-latest}" in
  latest|stable)
    # Newest stable, falling back to any release: during 0.x there may be no stable at all,
    # and refusing to install is a worse answer than installing what exists and saying so.
    VERSION=$(curl -fsSL "$api/latest" 2>/dev/null | first_tag) || true
    if [ -z "$VERSION" ]; then
      VERSION=$(curl -fsSL "$api?per_page=1" | first_tag)
      prerelease=1
    fi
    ;;
  preview|prerelease|rc)
    # A page, not one entry: `edge` is a rolling release and is always the newest, so the
    # prerelease channel picks the newest VERSION tag instead of whatever was pushed last.
    VERSION=$(curl -fsSL "$api?per_page=20" | version_tag)
    ;;
  edge|main|nightly)
    VERSION=edge
    ;;
esac
[ -n "$VERSION" ] || die "no releases found for $REPO"

# `0.1.0` and `v0.1.0` both name the same tag; only one of them is the tag. `edge` is a tag in
# its own right and takes no prefix.
case "$VERSION" in v*|edge) ;; *) VERSION="v$VERSION" ;; esac
case "$VERSION" in *-*) prerelease=1 ;; esac

ver=${VERSION#v}
archive="${BIN}_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading $BIN $VERSION ($os/$arch)"
[ -z "$prerelease" ] || echo "note: $VERSION is a prerelease"
curl -fsSL -o "$tmp/$archive" "$base/$archive" 2>/dev/null || die "no build for $os/$arch in $VERSION"

# Verified, because the point of piping a script to a shell is that you trusted the script and
# nothing else. A tampered archive is caught here rather than run.
if curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" 2>/dev/null; then
  want=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
  if [ -n "$want" ]; then
    if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$archive" | awk '{print $1}')
    elif command -v shasum   >/dev/null 2>&1; then got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
    else got=""; echo "warning: no sha256 tool, checksum not verified" >&2; fi
    [ -z "$got" ] || [ "$got" = "$want" ] || die "checksum mismatch for $archive"
  fi
else
  echo "warning: checksums.txt unavailable, not verified" >&2
fi

tar -xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/$BIN" ] || die "archive did not contain $BIN"

# A writable directory beats one needing sudo: a pipe to sh cannot prompt for a password.
if [ -z "$INSTALL_DIR" ]; then
  if [ -w /usr/local/bin ]; then INSTALL_DIR=/usr/local/bin
  else INSTALL_DIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"
cp "$tmp/$BIN" "$INSTALL_DIR/$BIN" 2>/dev/null || die "cannot write to $INSTALL_DIR — set GENROC_INSTALL_DIR"
chmod 0755 "$INSTALL_DIR/$BIN"

# Checked rather than assumed: a failed copy inside an `||` chain still reports success.
[ -x "$INSTALL_DIR/$BIN" ] || die "install failed: $INSTALL_DIR/$BIN is not executable"
# The version comes from the binary rather than the tag: it is the one check that what landed
# is what was asked for, and on a rolling channel it carries the commit.
got=$("$INSTALL_DIR/$BIN" --version 2>/dev/null || echo "$VERSION")
echo
echo "$BIN $got installed successfully"
echo "  $INSTALL_DIR/$BIN"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "  warning: $INSTALL_DIR is not on your PATH" ;;
esac
echo
echo "(uninstall: curl -fsSL https://genroc.org/install.sh | sh -s -- --uninstall)"
