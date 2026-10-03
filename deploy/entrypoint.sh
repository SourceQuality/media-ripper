#!/bin/sh
set -eu

# First start: lay down a config the user can edit. Paths default to the
# container volumes; keys come from the environment until they are filled in.
if [ ! -f "${MR_CONFIG:-/config/config.yaml}" ]; then
  mkdir -p "$(dirname "${MR_CONFIG:-/config/config.yaml}")"
  sed -e 's#^  path: /mnt/media#  path: /output#' \
      -e 's#^workspace: /var/lib/media-ripper#workspace: /workspace#' \
      /config.example.yaml > "${MR_CONFIG:-/config/config.yaml}"
fi

# makemkvcon looks for settings in $HOME/.MakeMKV; keep that on the config
# volume so the key survives container recreation.
export HOME="${HOME:-/config}"

exec media-ripper "$@" -config "${MR_CONFIG:-/config/config.yaml}"
