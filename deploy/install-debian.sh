#!/bin/sh
# Installs media-ripper as a systemd service on Debian 12 / Ubuntu 22.04+
# including MakeMKV built from source. Run as root on the ripping machine.
#
#   MAKEMKV_ACCEPT_EULA=yes sh deploy/install-debian.sh
#
# MakeMKV's licence (EULA) must be accepted to build it. Run interactively
# to read it and answer the prompt, or set MAKEMKV_ACCEPT_EULA=yes once you
# have read it (makemkv-bin-<version>/src/eula_en_linux.txt). Pin another
# MakeMKV release with MAKEMKV_VERSION=x.y.z; betas expire, so stay current.
#
# Afterwards edit /etc/media-ripper/config.yaml (output path, TMDB key,
# MakeMKV key) and run: systemctl enable --now media-ripper
set -eu

MAKEMKV_VERSION="${MAKEMKV_VERSION:-2.0.0}"
PREFIX="${PREFIX:-/usr/local}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  ffmpeg mkvtoolnix eject tesseract-ocr tesseract-ocr-eng ca-certificates curl \
  build-essential pkg-config libc6-dev libssl-dev libexpat1-dev libavcodec-dev libgl1-mesa-dev zlib1g-dev

# --- MakeMKV ---------------------------------------------------------------
if ! command -v makemkvcon >/dev/null 2>&1 || ! makemkvcon -r --noscan info disc:9999 2>/dev/null | grep -q "MakeMKV v${MAKEMKV_VERSION}"; then
  tmp="$(mktemp -d)"
  cd "$tmp"
  curl -fsSLO "https://www.makemkv.com/download/makemkv-oss-${MAKEMKV_VERSION}.tar.gz"
  curl -fsSLO "https://www.makemkv.com/download/makemkv-bin-${MAKEMKV_VERSION}.tar.gz"
  tar xzf "makemkv-oss-${MAKEMKV_VERSION}.tar.gz"
  tar xzf "makemkv-bin-${MAKEMKV_VERSION}.tar.gz"
  (cd "makemkv-oss-${MAKEMKV_VERSION}" && ./configure --disable-gui --prefix="$PREFIX" && make -j"$(nproc)" && make install)
  if [ "${MAKEMKV_ACCEPT_EULA:-}" = "yes" ]; then
    mkdir -p "makemkv-bin-${MAKEMKV_VERSION}/tmp"
    echo accepted > "makemkv-bin-${MAKEMKV_VERSION}/tmp/eula_accepted"
  elif [ ! -t 0 ]; then
    echo "MakeMKV's licence must be accepted. Read $tmp/makemkv-bin-${MAKEMKV_VERSION}/src/eula_en_linux.txt," >&2
    echo "then rerun with MAKEMKV_ACCEPT_EULA=yes (or run this script in a terminal to be asked)." >&2
    exit 1
  fi
  # Without MAKEMKV_ACCEPT_EULA, make shows the licence and asks.
  (cd "makemkv-bin-${MAKEMKV_VERSION}" && make PREFIX="$PREFIX" && make install PREFIX="$PREFIX")
  ldconfig
  cd /
  rm -rf "$tmp"
fi

# --- media-ripper binary -----------------------------------------------------
if [ -x "$HERE/bin/media-ripper" ]; then
  install -m 0755 "$HERE/bin/media-ripper" "$PREFIX/bin/media-ripper"
elif command -v go >/dev/null 2>&1; then
  (cd "$HERE" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$PREFIX/bin/media-ripper" ./cmd/media-ripper)
else
  echo "no prebuilt bin/media-ripper and no Go toolchain; run 'make build' first" >&2
  exit 1
fi

# --- user, dirs, config, service ---------------------------------------------
id media-ripper >/dev/null 2>&1 || useradd --system --home-dir /var/lib/media-ripper --shell /usr/sbin/nologin --groups cdrom media-ripper
install -d -o media-ripper -g media-ripper -m 0775 /var/lib/media-ripper
# The service owns its config directory so the settings page can save.
install -d -o media-ripper -g media-ripper -m 0750 /etc/media-ripper
[ -f /etc/media-ripper/config.yaml ] || install -m 0640 -o media-ripper -g media-ripper "$HERE/config.example.yaml" /etc/media-ripper/config.yaml
chown media-ripper:media-ripper /etc/media-ripper/config.yaml
install -m 0644 "$HERE/deploy/99-media-ripper.rules" /etc/udev/rules.d/99-media-ripper.rules
udevadm control --reload && udevadm trigger || true
install -m 0644 "$HERE/deploy/media-ripper.service" /etc/systemd/system/media-ripper.service
systemctl daemon-reload

cat <<MSG

Installed. Next:
  1. edit /etc/media-ripper/config.yaml  (output.path, metadata.tmdb_api_key, makemkv.key)
  2. media-ripper check -config /etc/media-ripper/config.yaml
  3. systemctl enable --now media-ripper
  4. open http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080
MSG
