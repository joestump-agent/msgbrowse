---
title: Exporting your archives
sidebar_position: 3
description: Produce the on-disk Signal, iMessage, and WhatsApp archives msgbrowse reads, with msgbrowse export or the upstream tools directly.
---

# Exporting your archives

msgbrowse never reads Signal's database, iMessage's `chat.db`, or WhatsApp's
`ChatStorage.sqlite` directly. It reads **on-disk archives** produced by three
upstream exporters, and treats those archives as strictly read-only. You can
run the exporters yourself, or let `msgbrowse export` orchestrate them in one
step.

## The layout msgbrowse expects

**Signal** (`signal-export`): a root folder containing an `export/`
subdirectory with one folder per conversation:

```
Signal-Archive/
├── export/
│   └── ConversationName/
│       ├── chat.md          # the conversation, plaintext Markdown
│       └── media/           # attachments for this conversation
└── .snapshots/              # optional encrypted DB backups (listed, never opened)
```

**iMessage** (`imessage-exporter`, `-f txt`): a flat directory of
`ChatName.txt` files plus copied attachments.

**WhatsApp** ([`whatsapp-chat-exporter`](https://github.com/KnugiHK/WhatsApp-Chat-Exporter),
JSON mode): a root folder containing the tool's single `result.json` plus the
media directories it copies:

```
WhatsApp-Archive/
├── result.json              # every chat, keyed by JID
└── Message/
    └── Media/               # attachments, one folder per chat
```

## Option 1: `msgbrowse export`

`msgbrowse export` runs the upstream tools into your configured archive
roots, streaming their output to your terminal:

```sh
msgbrowse --archive-root ~/Signal-Archive \
          --imessage-archive-root ~/iMessage-Archive \
          --whatsapp-archive-root ~/WhatsApp-Archive \
          export
```

Under the hood it runs:

- **Signal:** `sigexport <archive_root>/export` — so each chat lands at
  `<archive_root>/export/<conversation>/chat.md` plus its `media/` folder,
  exactly the layout the importer scans.
- **iMessage:** `imessage-exporter -f txt -c clone -o <imessage_archive_root>`
  — copy mode is **always** used, so attachments are bundled into the archive.
- **WhatsApp:** `wtsexporter` with JSON output directed into
  `<whatsapp_archive_root>` — see the
  [platform prerequisites](#whatsapp-platform-prerequisites) below, because
  unlike the other two tools it needs you to point it at a WhatsApp database
  first.

A source whose archive root is unset is simply skipped, so a Signal-only or
iMessage-only (or any other subset) setup just works. msgbrowse stores no
secrets and reads no Keychain — the invoked tools do, with your consent.

### `export` flags

| Flag | What it does |
| --- | --- |
| `--signal-export-bin` | Path to the Signal exporter (default: `sigexport` on `PATH`; or set `signal_export_bin` in config) |
| `--imessage-exporter-bin` | Path to `imessage-exporter` (default: on `PATH`; or set `imessage_exporter_bin`) |
| `--whatsapp-exporter-bin` | Path to `wtsexporter` (default: on `PATH`; or set `whatsapp_exporter_bin`) |
| `--signal-export-args` | Extra arg passed only to `sigexport` (repeatable) |
| `--imessage-exporter-args` | Extra arg passed only to `imessage-exporter` (repeatable) |
| `--whatsapp-exporter-args` | Extra arg passed only to `wtsexporter` (repeatable — this is how you point it at your database/backup, see below) |
| `--skip-on-error` | Log and skip a failing or missing source instead of aborting (the run still exits non-zero) |

Trailing `-- <args>` are appended to **every** tool's command line — use the
per-tool flags for arguments meant for one tool only:

```sh
# pass --verbose to both exporters
msgbrowse export -- --verbose

# pass an argument to sigexport only
msgbrowse export --signal-export-args=--overwrite
```

If a configured source's tool is missing from `PATH`, `export` fails with an
error naming the tool and how to install it (`pipx install signal-export` /
`brew install imessage-exporter` / `pipx install whatsapp-chat-exporter`).

## Option 2: run the upstream exporters yourself

```sh
# Signal — writes export/<conversation>/chat.md + media/
sigexport ~/Signal-Archive/export

# iMessage — txt format, copy mode, into the archive root
imessage-exporter -f txt -c clone -o ~/iMessage-Archive

# WhatsApp — JSON output into the archive root (see the prerequisites below
# for where the database and media come from)
cd ~/WhatsApp-Archive
wtsexporter -i \
  -d "$HOME/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite" \
  -m Message \
  -j result.json --no-html
```

## iMessage: two gotchas

:::warning Full Disk Access is required
`imessage-exporter` reads `~/Library/Messages/chat.db`, which macOS protects.
The terminal (or scheduled job) running the export must have **Full Disk
Access** granted in System Settings → Privacy & Security. Without it the
export fails or comes up empty. This is a one-time manual grant.
:::

:::warning Always export in copy mode (`-c clone`)
Run `imessage-exporter` with `-c`/`--copy-method` (e.g. `-c clone`). Without
copy mode, the export records attachments as **absolute `~/Library/...` path
references** instead of copying the files into the archive — so msgbrowse can
index your messages but **no media will render**. `msgbrowse export` always
passes `-c clone` for exactly this reason, and `msgbrowse doctor` diagnoses
this exact case: if your imported iMessage attachments are mostly absolute
paths, it tells you to re-export with `-c clone` and re-run
`msgbrowse import --full`.
:::

## WhatsApp: platform prerequisites

Unlike the other two exporters, `wtsexporter` doesn't talk to a running app —
you first have to hand it a WhatsApp **database**
(`pipx install whatsapp-chat-exporter`; the console command is
`wtsexporter`). Where that database comes from depends on your platform.

### iPhone route 1 (recommended): the Mac companion app's local database

If you use the WhatsApp **macOS app** paired to your iPhone, it already keeps
a local `ChatStorage.sqlite` on your Mac — no phone backup needed:

```sh
ls ~/Library/Group\ Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite
```

Copy the app's media folder into your archive root, then run the exporter
from inside it so every path lands root-relative:

```sh
mkdir -p ~/WhatsApp-Archive
cp -R "$HOME/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/Message" \
      ~/WhatsApp-Archive/Message
cd ~/WhatsApp-Archive
wtsexporter -i \
  -d "$HOME/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite" \
  -m Message \
  -j result.json --no-html
```

:::warning Companion-app media is shallow
The Mac app only syncs messages (and especially media) from around the time
you linked it onward, and it lazily downloads older attachments. Expect a
complete-looking *text* history with many older media files missing —
msgbrowse renders those as "media missing" chips. For deeper media history,
use the iPhone-backup route below.
:::

### iPhone route 2 (deeper history): a local Finder/iTunes backup

Back up the iPhone to this computer (Finder on macOS, or iTunes/Apple Devices
on Windows — see [Apple's guide](https://support.apple.com/HT211229)), then
point the exporter at the backup directory:

```sh
cd ~/WhatsApp-Archive
wtsexporter -i -b ~/Library/Application\ Support/MobileSync/Backup/<device-id> \
  -j result.json --no-html
```

The exporter extracts both the database and the media that exists on the
phone from the backup. If your backup is **encrypted**, install the extra
decryption dependency first
(`pip install git+https://github.com/KnugiHK/iphone_backup_decrypt`).

### Android

Copy `msgstore.db` (and optionally `wa.db` for contact names) from the phone
into your archive root and run `wtsexporter -a`. For encrypted backups
(`msgstore.db.crypt14`/`crypt15`) you also need the key — either the `key`
file (rooted devices) or the **64-digit end-to-end encryption key** WhatsApp
shows under *Settings → Chats → Chat backup → End-to-end encrypted backup*:

```sh
cd ~/WhatsApp-Archive
wtsexporter -a -k encrypted_backup.key -b msgstore.db.crypt15 -j result.json --no-html
# or with the 64-digit hex key directly:
wtsexporter -a -k <64-digit-hex-key> -b msgstore.db.crypt15 -j result.json --no-html
```

However you produce it, the archive root you point `whatsapp_archive_root` at
must contain the tool's `result.json`; media render when the referenced files
also live under that root. Voice notes render as file chips (no transcription)
and stickers render as images.

## One-command refresh: `msgbrowse sync`

Once things work, `msgbrowse sync` chains the whole pipeline — **export →
import → media → embed → facts** — reusing each command's logic and sharing
one database handle:

```sh
msgbrowse sync
```

Stage-skipping flags: `--no-export`, `--no-media`, `--no-embed`, `--no-facts`.
`sync` also accepts the same exporter flags as `export`
(`--signal-export-bin`, `--imessage-exporter-bin`, `--whatsapp-exporter-bin`,
`--signal-export-args`, `--imessage-exporter-args`,
`--whatsapp-exporter-args`, and trailing `-- <args>`) — see the
[CLI reference](../reference/cli.md) for the full flag tables.

Failures in the hard stages (export, import, media) abort the run unless
`--skip-on-error`, which logs a warning and continues (the run still exits
non-zero). The LLM-dependent stages (embed, facts) **always** warn and
continue on failure — so a fully local run with no reachable LLM endpoint
still completes export, import, and media, and exits successfully.

## Automating the exports

The archives grow as you re-export, and imports are incremental — so a daily
scheduled export plus `msgbrowse sync` keeps everything fresh. The
[README](https://github.com/joestump/msgbrowse#scheduling-daily-exports-with-claude-cowork)
includes ready-to-paste Claude Cowork prompts that set up daily `launchd` jobs
on macOS for both sources, including snapshot retention for Signal.

## Next step

Archives on disk? Load them: [First import](first-import.md).
