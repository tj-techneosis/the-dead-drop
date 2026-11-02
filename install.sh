#!/usr/bin/env bash
# Install the Dead Drop for the current user.
#
#   curl -fsSL https://raw.githubusercontent.com/tj-techneosis/the-dead-drop/main/install.sh | bash
#
# Run that way (or as a lone downloaded file) it fetches the latest release for
# this machine, checks its SHA-256, unpacks it into
# ~/.local/share/deaddrop-install/releases/<version>/ and installs from there; running
# it again upgrades. Run from a checkout, it builds from source (Go 1.26 or
# newer, or mise); with neither, it installs the latest release instead.
# Options go after `bash -s --`, e.g. `| bash -s -- --claude`.
#
#   ./install.sh                install `deaddrop` and set up Claude Code
#   ./install.sh --version v0.1.0   that release rather than the latest (download only)
#   ./install.sh --claude       ...and add the CLAUDE.md section without asking
#   ./install.sh --no-claude    skip Claude Code entirely
#   ./install.sh --claude-only  only the Claude Code part
#   ./install.sh --uninstall    remove it (the mail and the config stay)
#
# It installs one binary, ~/.local/bin/deaddrop. Nothing runs in the
# background: there is no daemon. The store is made by the first send.
#
# Claude Code: when ~/.claude (or $CLAUDE_CONFIG_DIR) exists, the deaddrop
# skill is linked into its skills/, and a short section is offered for its
# CLAUDE.md, between marker comments, so re-running updates it in place and
# --uninstall takes it out. CLAUDE.md is written in place, never replaced.
set -euo pipefail

repo=tj-techneosis/the-dead-drop
releases=${DEADDROP_RELEASES:-https://github.com/$repo/releases}
# Not .../deaddrop: that's where the store (everyone's mail) lives by default,
# and --uninstall removes this folder.
data=${XDG_DATA_HOME:-$HOME/.local/share}/deaddrop-install
bindir=${DEADDROP_BIN_DIR:-$HOME/.local/bin}
claude=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
BEGIN='<!-- deaddrop:begin (managed by the-dead-drop/install.sh; edit agents/claude-md-snippet.md upstream, not here) -->'
END='<!-- deaddrop:end -->'

# Where this script runs from decides what it installs. Piped into bash it has
# no file at all, and the working directory says nothing about it.
if [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]]; then
  src=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
else
  src=""
fi
if [[ -n $src && -f $src/cmd/deaddrop/main.go ]]; then
  mode=checkout
elif [[ -n $src && -f $src/VERSION && -x $src/build/deaddrop ]]; then
  mode=release
else
  mode=download
fi

uninstall=0 claude_only=0 claude_md=ask want=latest pass=()
while [[ $# -gt 0 ]]; do
  case $1 in
    --uninstall)   uninstall=1 ;;
    --claude-only) claude_only=1; pass+=("$1") ;;
    --claude)      claude_md=yes; pass+=("$1") ;;
    --no-claude)   claude_md=never; pass+=("$1") ;;
    --version)     want=${2:?--version needs a release, e.g. v0.1.0}; shift ;;
    --version=*)   want=${1#*=} ;;
    -h|--help)
      if [[ -n $src ]]; then sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'
      else echo "Installs the Dead Drop's latest release. Options (after bash -s --): --version vX.Y.Z, --claude, --no-claude, --claude-only, --uninstall. See https://github.com/$repo#readme"; fi
      exit 0 ;;
    *) echo "install.sh: unknown option $1 (try --help)" >&2; exit 2 ;;
  esac
  shift
done
case $want in
  latest|v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "install.sh: --version wants a release tag like v0.1.0, not \"$want\"" >&2; exit 2 ;;
esac

have_go() { command -v go >/dev/null 2>&1 || command -v mise >/dev/null 2>&1; }
if [[ $mode == checkout && $uninstall == 0 && $claude_only == 0 ]] && ! have_go; then
  echo "install.sh: no Go (or mise) to build this checkout, so installing the latest release instead"
  mode=download
fi
if [[ $mode != download && $want != latest ]]; then
  echo "install.sh: --version is for downloading a release; this is a $mode install" >&2
  exit 2
fi

link() {  # link <from> <to>, refusing to clobber anything that isn't a link
  local from=$1 dst=$2
  mkdir -p "$(dirname "$dst")"
  if [[ -e $dst && ! -L $dst ]]; then
    echo "install.sh: refusing to replace $dst: it is a real file, not a link" >&2
    return 1
  fi
  ln -sfn "$from" "$dst"
  echo "linked $dst"
}

# Rewrite a file through any symlink: content in place, never a rename.
rewrite() {  # rewrite <file> <awk program> [awk args...]
  local f=$1 prog=$2 tmp; shift 2
  tmp=$(mktemp)
  awk "$@" "$prog" "$f" >"$tmp" && cat "$tmp" >"$f"
  rm -f "$tmp"
}

has_block() { [[ -f $1 ]] && grep -qF '<!-- deaddrop:begin' "$1"; }

claude_install() {
  [[ $claude_md == never ]] && return 0
  if [[ ! -d $claude ]]; then
    echo "Claude Code not found at $claude; skipping its setup"
    return 0
  fi
  # A release's folder is replaced on upgrade, so the skill is copied into
  # the data dir's stable place rather than linked into a versioned one.
  local skill=$src/agents/skills/deaddrop
  if [[ $mode == release ]]; then
    mkdir -p "$data/agents"
    rm -rf "$data/agents/skills"
    cp -R "$src/agents/skills" "$data/agents/skills"
    skill=$data/agents/skills/deaddrop
  fi
  link "$skill" "$claude/skills/deaddrop" || return 0

  local md=$claude/CLAUDE.md
  if has_block "$md"; then
    rewrite "$md" '
      index($0, "<!-- deaddrop:begin") == 1 { print b; while ((getline l < snip) > 0) print l; skip = 1; next }
      index($0, "<!-- deaddrop:end") == 1   { skip = 0; print e; next }
      !skip' -v b="$BEGIN" -v e="$END" -v snip="$src/agents/claude-md-snippet.md"
    echo "updated the deaddrop section of $md"
    return 0
  fi
  if [[ $claude_md == ask ]]; then
    # Piped from curl, stdin is the script; a person at a terminal still has
    # /dev/tty, and an unattended run has neither.
    local from=""
    if [[ -t 0 ]]; then from=/dev/stdin; elif [[ -t 1 ]] && { : </dev/tty; } 2>/dev/null; then from=/dev/tty; fi
    if [[ -n $from ]]; then
      echo
      echo "Add a short section to $md so Claude passes documents through"
      echo "the Dead Drop (rather than copy and paste)?"
      read -r -p "[Y/n] " reply <"$from"
      [[ -z $reply || $reply == [Yy]* ]] || { echo "skipped; ./install.sh --claude adds it later"; return 0; }
    else
      echo "not adding a CLAUDE.md section without asking; run the installer again with --claude to add it"
      return 0
    fi
  fi
  { [[ -s $md ]] && echo; echo "$BEGIN"; cat "$src/agents/claude-md-snippet.md"; echo "$END"; } >>"$md"
  echo "added a deaddrop section to $md"
}

claude_uninstall() {
  [[ -L $claude/skills/deaddrop ]] && rm "$claude/skills/deaddrop" && echo "removed $claude/skills/deaddrop"
  if has_block "$claude/CLAUDE.md"; then
    rewrite "$claude/CLAUDE.md" '
      held { if (index($0, "<!-- deaddrop:begin") != 1) print ""; held = 0 }
      index($0, "<!-- deaddrop:begin") == 1 { skip = 1; next }
      index($0, "<!-- deaddrop:end") == 1   { skip = 0; next }
      skip { next }
      $0 == "" { held = 1; next }
      { print }
      END { if (held) print "" }'
    echo "removed the deaddrop section from $claude/CLAUDE.md"
  fi
}

if [[ $uninstall == 1 ]]; then
  claude_uninstall
  if [[ $claude_only == 0 ]]; then
    rm -f "$bindir/deaddrop" && echo "removed $bindir/deaddrop"
    rm -rf "$data" && echo "removed $data"
    echo "The mail (~/.local/share/deaddrop, or \$DEADDROP_DIR) and the config (~/.config/deaddrop) are left as they were."
  fi
  exit 0
fi
if [[ $claude_only == 1 ]]; then
  [[ $mode == download ]] && { echo "install.sh: --claude-only runs from a checkout or a release" >&2; exit 2; }
  claude_install
  exit 0
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m); [[ $arch == x86_64 ]] && arch=amd64; [[ $arch == aarch64 ]] && arch=arm64

if [[ $mode == download ]]; then
  case $os-$arch in
    darwin-arm64|darwin-amd64|linux-amd64|linux-arm64) ;;
    *) echo "install.sh: no release for $os-$arch; build from a checkout of the repo instead, with Go" >&2; exit 1 ;;
  esac
  asset=deaddrop-$os-$arch.tar.gz
  if [[ $want == latest ]]; then url=$releases/latest/download; else url=$releases/download/$want; fi
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  echo "fetching $asset ($want)"
  get() { curl -fsSL --connect-timeout 15 --max-time 300 "$1" -o "$2"; }
  get "$url/$asset" "$tmp/$asset" || { echo "install.sh: could not download $url/$asset (is $want a release?)" >&2; exit 1; }
  get "$url/SHA256SUMS" "$tmp/SHA256SUMS" || { echo "install.sh: could not download $url/SHA256SUMS" >&2; exit 1; }
  expect=$(awk -v a="$asset" '$2 == a || $2 == "*"a {print $1}' "$tmp/SHA256SUMS")
  if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$asset"); else got=$(shasum -a 256 "$tmp/$asset"); fi
  got=${got%% *}
  if [[ -z $expect || $expect != "$got" ]]; then
    echo "install.sh: checksum mismatch for $asset: expected ${expect:-nothing}, got $got. Not installing." >&2
    exit 1
  fi
  echo "checksum ok"
  tar -xzf "$tmp/$asset" -C "$tmp"
  ver=$(cat "$tmp/deaddrop-$os-$arch/VERSION")
  # It names a folder about to be replaced: a release tag and nothing else.
  [[ $ver =~ ^v[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.-]*$ ]] || { echo "install.sh: the release's VERSION isn't a release tag; not installing" >&2; exit 1; }
  mkdir -p "$data/releases"
  rm -rf "${data:?}/releases/$ver"
  mv "$tmp/deaddrop-$os-$arch" "$data/releases/$ver"
  # Keep this release and the one installed before it; older ones go.
  prev=$("$bindir/deaddrop" version 2>/dev/null | awk '{print $2}' || true)
  for d in "$data"/releases/*/; do
    d=$(basename "$d")
    [[ $d == "$ver" || $d == "$prev" ]] || rm -rf "${data:?}/releases/$d"
  done
  rm -rf "$tmp"
  trap - EXIT
  exec bash "$data/releases/$ver/install.sh" "${pass[@]+"${pass[@]}"}"
fi

mkdir -p "$bindir"
if [[ $mode == release ]]; then
  ver=$(cat "$src/VERSION")
  cp "$src/build/deaddrop" "$bindir/deaddrop.new"
else
  command -v go >/dev/null 2>&1 || go() { mise exec -- go "$@"; }
  ver=$(git -C "$src" describe --tags --always --dirty 2>/dev/null || echo dev)
  (cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$ver" -o "$bindir/deaddrop.new" ./cmd/deaddrop)
fi
mv "$bindir/deaddrop.new" "$bindir/deaddrop"
echo "installed $bindir/deaddrop ($ver)"

claude_install

case ":$PATH:" in
  *":$bindir:"*) ;;
  *) echo "note: $bindir is not on PATH. Add it to your shell's profile:"
     echo "  export PATH=\"$bindir:\$PATH\"" ;;
esac
cat <<'MSG'

Next:
  deaddrop send --to me --subject "Hello" <<< "The Dead Drop is up."
  deaddrop inbox                 what's waiting for you
  On a machine without the store: host and me in ~/.config/deaddrop/config.toml
  (see examples/config.toml).
MSG
