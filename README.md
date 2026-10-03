# media-ripper

Insert a disc. Walk away. Come back to the movie or the episodes in your
library, named for Plex or Jellyfin, with every audio track and subtitle, and
the tray open for the next one.

media-ripper is an unattended ripping daemon for Blu-ray and DVD. It is in the
same family as [ARM](https://github.com/automatic-ripping-machine/automatic-ripping-machine)
but with a narrower job: no prompts, no per-disc decisions, no transcoding
unless you ask. MakeMKV does the reading, the file is a bit-for-bit remux of
what is on the disc, and everything about how a disc is handled is decided in
one config file before the first disc goes in.

## What it does

For each drive, in a loop:

1. **Detect** a disc through the kernel's CD-ROM ioctl (no udev, works in
   Docker), fingerprint it, lock the tray.
2. **Scan** it with `makemkvcon` to list titles, chapters, segments and tracks.
3. **Identify** it: the volume label is cleaned up (`THE_MATRIX_1999`,
   `FRIENDS_S3_D2`, `WB_INCEPTION_BD`) and looked up on TMDB to get the
   movie's runtime or the season's episode list.
4. **Select** the right titles. Movies: the title whose length matches the
   TMDB runtime, with Blu-ray decoy playlists collapsed by segment map and
   the real one chosen by chapter count and segment order. TV: every title
   that fits the episode length, play-all titles dropped, episodes numbered
   in disc order and carried over to the next disc of the season. Trailers,
   featurettes and menus are never ripped.
5. **Rip** with MakeMKV. No re-encode. All audio and subtitle tracks are kept
   unless you restrict languages.
6. **Remux** (optional, stream copy only) to set the file title and apply the
   language filter, with mkvmerge or ffmpeg. A custom command slot is there
   if you ever want to transcode.
7. **Deliver** to the library path using your naming template. Copies go
   through a `.part` file and are size-checked, so a network share never sees
   a half-written file.
8. **Eject** and record the disc, so reinserting it does not rip it twice.

A disc that cannot be identified is still ripped (longest title, or every
episode-shaped title if it looks like a TV set) into `_unidentified/`, so a
bad label never stops the machine.

## Install

### Docker

```sh
git clone https://github.com/sourcequality/media-ripper
cd media-ripper/deploy
# edit docker-compose.yml: library path, /dev/sr0 and /dev/sg0 for your drive
MR_TMDB_API_KEY=... MR_MAKEMKV_KEY=... docker compose up -d --build
```

The image builds MakeMKV from source (its license does not allow shipping
the binary), so the first build takes a few minutes. On first start a
`config/config.yaml` is created; edit it and `docker compose restart`.

### Debian / Ubuntu service

```sh
git clone https://github.com/sourcequality/media-ripper
cd media-ripper
make build
sudo MAKEMKV_VERSION=1.17.9 sh deploy/install-debian.sh
sudo nano /etc/media-ripper/config.yaml
sudo media-ripper check
sudo systemctl enable --now media-ripper
```

The installer builds MakeMKV, creates a `media-ripper` user in the `cdrom`
group, installs a udev rule so that user can reach the drive's SCSI node,
and installs a systemd unit with `Restart=always`.

### Appliance / laptop

The binary is static (`CGO_ENABLED=0`) and `make release` cross-compiles for
amd64, arm64 and armv7. A minimal Debian netinstall plus the installer above
is enough for a dedicated box. For a laptop that should rip from the moment
it boots: install the service, set the BIOS to power on when the lid opens
or on AC, and mount the library share in `/etc/fstab` with `_netdev,nofail`
so a missing network never blocks boot.

## Configure

Everything lives in one YAML file. `config.example.yaml` documents every key;
the ones you must set:

| key | what |
| --- | --- |
| `output.path` | library root, e.g. `/mnt/media` |
| `metadata.tmdb_api_key` | free key from [themoviedb.org](https://www.themoviedb.org/settings/api); without it discs go to `_unidentified/` |
| `makemkv.key` | your MakeMKV key (or the current beta key) |

Useful knobs:

- `selection.languages: [eng, jpn]` keeps only those audio/subtitle tracks
  (untagged tracks always stay, and a file is never left without audio).
- `metadata.label_overrides` fixes discs with useless labels: map the label
  (`media-ripper label /dev/sr0` prints it) to what to search for, e.g.
  `"LOGICAL_VOLUME_ID": "Friends S1 D2"`.
- `output.movie_template` / `tv_template` control naming. Tokens: `{title}`
  `{year}` `{series}` `{season:02}` `{episode:02}` `{episode_title}` `{label}`
  `{title_id}` `{resolution}` `{source}` `{date}`.
- `postprocess.mode: custom` with `custom_command` runs anything (ffmpeg with
  libx265, HandBrakeCLI) between rip and delivery. Bit rates only change if
  you put that there.
- `notify.ntfy_url` or `notify.webhook_url` for a push when a disc is done
  or fails.

Secrets can be passed as `MR_TMDB_API_KEY`, `MR_MAKEMKV_KEY` and friends
instead of being written into the file.

## Operate

```
media-ripper run                 # the daemon (what the service runs)
media-ripper scan /dev/sr0       # dry run: what would be ripped and why, no rip
media-ripper check               # validate config, tools, drives
media-ripper label /dev/sr0      # print the disc label, fingerprint, parsed query
media-ripper eject /dev/sr0
```

`scan` is the thing to run when a disc confuses the selector: it prints the
title list, the lookup result, which titles were picked and why each other
title was skipped.

The web UI on port 8080 shows each drive, the current rip with progress and
ETA, and a history with per-job logs. Buttons: Cancel, Eject, and Rip (which
forces a disc that was ripped before). `GET /api/status`, `/api/history`,
`/api/series` give the same as JSON; `POST /api/series/reset` with
`{"series":"tmdb:1668","season":1}` restarts episode numbering for a season.

State lives in `workspace/state/`: `history.jsonl`, `discs.json` (ripped
fingerprints) and `series.json` (next episode per season). All plain JSON;
delete an entry to make the daemon forget it.

## TV episode numbering

TMDB knows the episodes of a season but not which ones are on which disc.
media-ripper numbers the episode-length titles on a disc in playback order
starting from the season's stored "next episode", then advances it. Insert
disc 1, 2, 3 of a season in order and the numbering is right. If you go out
of order, reset the season in the UI or API, or set the label override for
that disc. Titles about twice the episode length become `S01E03-E04`.

## Limits

- UHD Blu-ray needs a drive with LibreDrive-compatible firmware; that is a
  MakeMKV concern, not something this daemon can work around.
- The fingerprint is a hash of the disc's volume descriptors. Two pressings
  of the same title usually share it, which is what you want for "already
  ripped"; a boxed set whose discs all carry an identical label and
  descriptors (rare) would need `rerip_same_disc: true`.
- Only Linux. The drive layer is Linux ioctls.

## Develop

```sh
make test      # unit tests plus an end-to-end run against a fake makemkvcon
make vet
make build
```

Layout: `cmd/media-ripper` (CLI), `internal/drive` (ioctls, label,
fingerprint), `internal/makemkv` (robot-mode parser and runner),
`internal/metadata` (label parsing, TMDB), `internal/selector` (title
choice), `internal/naming`, `internal/postprocess`, `internal/pipeline`
(the per-drive state machine), `internal/store`, `internal/web`.
