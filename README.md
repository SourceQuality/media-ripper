# media-ripper

## Quick start

You need a Linux machine with a Blu-ray or DVD drive, and
[MakeMKV](https://www.makemkv.com/) 2.x installed with a registration key or
the [current beta key](https://forum.makemkv.com/forum/viewtopic.php?t=1053).
MakeMKV is not part of the download; see [Install](#install) for builds that
include it.

1. **Download** the latest release: the binary for your machine
   (`amd64`, `arm64` for a Pi 4/5, or `armv7`) and the three setup files.

   ```sh
   gh release download --repo SourceQuality/media-ripper \
     -p 'media-ripper-linux-amd64' -p config.example.yaml \
     -p media-ripper.service -p 99-media-ripper.rules
   ```

   The repository is private, so use [`gh`](https://cli.github.com/) (signed
   in with access) or download them from the Releases page in a browser.

2. **Install** the binary, a service user, the config and the service:

   ```sh
   sudo install -m 0755 media-ripper-linux-amd64 /usr/local/bin/media-ripper
   sudo useradd --system --home-dir /var/lib/media-ripper --shell /usr/sbin/nologin --groups cdrom media-ripper
   sudo install -d -o media-ripper -g media-ripper -m 0750 /etc/media-ripper
   sudo install -o media-ripper -g media-ripper -m 0640 config.example.yaml /etc/media-ripper/config.yaml
   sudo install -m 0644 99-media-ripper.rules /etc/udev/rules.d/
   sudo install -m 0644 media-ripper.service /etc/systemd/system/
   sudo udevadm control --reload && sudo udevadm trigger
   ```

3. **Configure** `/etc/media-ripper/config.yaml`. Only these need you:
   - `drives`, e.g. `[/dev/sr0]`
   - `output.path`, where your library lives
   - `makemkv.key`
   - something to identify discs: `arr.radarr` / `arr.sonarr` (URL and API
     key) or `metadata.tmdb_api_key`

   Everything else can wait for the Settings page.

4. **Check and start:**

   ```sh
   sudo -u media-ripper media-ripper check -config /etc/media-ripper/config.yaml
   sudo systemctl enable --now media-ripper
   ```

   `check` prints the MakeMKV version and the drive's state, and fails with
   the reason if MakeMKV's key or beta has expired.

5. **Open** `http://<this machine>:8080` and insert a disc.

To update, download the new binary, install it over the old one and
`sudo systemctl restart media-ripper`. A disc in the middle of a rip
resumes where it stopped.

## What it is

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
6. **Eject** as soon as the last title is ripped and record the disc, so
   reinserting it does not rip it twice. The rest happens with the drive
   free.
7. **Remux** (optional, stream copy only) to set the file title and apply the
   language filter, with mkvmerge or ffmpeg. A custom command slot is there
   if you ever want to transcode.
8. **Deliver** to the library path using your naming template. Copies go
   through a `.part` file and are size-checked, so a network share never sees
   a half-written file. With Radarr or Sonarr configured, the files go to a
   staging folder instead and the app imports them (see below).

A disc that cannot be identified is still ripped (longest title, or every
episode-shaped title if it looks like a TV set) into `_unidentified/`, so a
bad label never stops the machine. With OCR enabled it gets a second chance
first (see below).

## Radarr and Sonarr

Enable `arr.radarr` and/or `arr.sonarr` with the URL and API key. Movies
then go to `output.path/_incoming/` and media-ripper calls Radarr's
`DownloadedMoviesScan`; TV goes the same way to Sonarr's
`DownloadedEpisodesScan`. The app renames and moves the files into its own
root folder with its own naming rules and refreshes Plex or Jellyfin. If
the title is not in the library yet it is added first (unmonitored by
default) using `root_folder` and `quality_profile`. When Radarr runs in a
different container, `path_map` translates the staging path into what
Radarr sees. If the import fails the files stay in `_incoming/` and the job
shows a warning; the rip itself still counts as done.

Radarr and Sonarr also work as a metadata source: with `metadata.provider:
auto` their lookup endpoints are used after TMDB (or instead of it when no
TMDB key is set), so a TMDB key is optional once an *arr app is configured.

## Identifying discs with useless labels

Many discs are labelled `BD_ROM` or `LOGICAL_VOLUME_ID`. Blu-rays usually
carry a real title in their on-disc metadata, which MakeMKV reports and the
pipeline already uses. Beyond that there is no audio or video fingerprint
database for films, so the remaining signal is what is on screen.
`metadata.ocr.enabled: true` samples frames from the first minutes and the
end credits of the ripped file, runs tesseract on them, and searches the
largest recurring text (the title card) on TMDB or Radarr/Sonarr. A match
is only accepted when the title's runtime agrees with the length of what
was ripped, which keeps studio logos and dialogue from producing wrong
names. It runs after the disc has been ejected and costs a few minutes of
CPU per unidentified disc. It needs `tesseract-ocr` and a language pack;
the Docker image includes English.

## Install

### Docker

```sh
git clone https://github.com/sourcequality/media-ripper
cd media-ripper/deploy
# edit docker-compose.yml: library path, /dev/sr0 and /dev/sg0 for your drive
MAKEMKV_ACCEPT_EULA=yes MR_TMDB_API_KEY=... MR_MAKEMKV_KEY=... docker compose up -d --build
```

The image builds MakeMKV from source (its license does not allow shipping
the binary), so the first build takes a few minutes. On first start a
`config/config.yaml` is created; edit it and `docker compose restart`.

### Release binaries

The binaries on the GitHub releases page are media-ripper only. MakeMKV is
a separate program you install yourself (makemkv.com, or the installer and
Docker image below, which build it on your machine): its licence does not
allow shipping it. `media-ripper check` reports the MakeMKV version and
fails when the beta has expired or the stored key is invalid. Each release
also carries `config.example.yaml`, the systemd unit and the udev rule; the
[Quick start](#quick-start) installs them.

### Debian / Ubuntu service

```sh
git clone https://github.com/sourcequality/media-ripper
cd media-ripper
make build
sudo sh deploy/install-debian.sh     # asks you to accept MakeMKV's licence
# or, after reading it: sudo MAKEMKV_ACCEPT_EULA=yes sh deploy/install-debian.sh
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
| `metadata.tmdb_api_key` | free key from [themoviedb.org](https://www.themoviedb.org/settings/api); optional when Radarr/Sonarr are configured |
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
forces a disc that was ripped before). The Settings page edits every key in
the config file; Save validates, writes the file and applies the change to
the running daemon. Drives, workspace, the listen address and logging need a
restart, and the page says so. Keys set through `MR_*` environment variables
show as locked, since the environment would win again on the next start.
Saving rewrites the file without comments, so keep `config.example.yaml` as
the reference. `GET /api/status`, `/api/history`, `/api/series` and
`/api/config` give the same as JSON (`PUT /api/config` saves);
`POST /api/series/reset` with `{"series":"tmdb:1668","season":1}` restarts
episode numbering for a season.

State lives in `workspace/state/`: `history.jsonl`, `discs.json` (ripped
fingerprints) and `series.json` (next episode per season). All plain JSON;
delete an entry to make the daemon forget it.

## TV episode numbering

When the disc is in [TheDiscDB](https://thediscdb.com), its catalogue says
which episode each title is, so episodes get their real numbers and names,
extras are skipped by name, and discs can go in **in any order**. The
catalogue is read from its GitHub repository (`TheDiscDb/data`): one
listing a day, then only the disc files of the identified title, cached
under the workspace. A disc matches when its titles' playlists and exact
sizes agree with a catalogued disc; nothing else is read from the disc.
Turn it off with `metadata.thediscdb.enabled: false`.

TMDB and Sonarr know the episodes of a season but not which ones are on
which disc, so for discs TheDiscDB does not have, media-ripper numbers the
episode-length titles in playback order starting from the season's stored
"next episode", then advances it. Insert disc 1, 2, 3 of such a season in
order and the numbering is right. If you go out of order, reset the season
in the UI or API, or set the label override for that disc. Titles about
twice the episode length become `S01E03-E04`.

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
`internal/metadata` (label parsing, TMDB, *arr provider), `internal/arr`
(Radarr/Sonarr client), `internal/ocr` (title-card recognition),
`internal/selector` (title choice), `internal/naming`,
`internal/postprocess`, `internal/pipeline` (the per-drive state machine),
`internal/store`, `internal/web`.
