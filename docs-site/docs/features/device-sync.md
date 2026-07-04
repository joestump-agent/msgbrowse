---
title: Device sync
sidebar_label: Device sync
sidebar_position: 7
---

# Device sync

Device sync makes one msgbrowse install's archives browsable on another
machine — a second Mac, a home server — by synchronizing the **archive files,
never the database**. Under the hood msgbrowse bundles and supervises
[Syncthing](https://syncthing.net/) as the transfer engine: msgbrowse
generates its entire configuration, drives it over a loopback REST API, and
surfaces its state in msgbrowse's own Settings, Status, Logs, and `doctor` —
**you never see or touch Syncthing's own UI**.

Device sync is **off by default**. With it disabled (the default), no sync
process runs and msgbrowse keeps its loopback-only posture.

## The model: importers and replicas

Each source (Signal, iMessage, WhatsApp) has exactly **one importer** across
your paired devices:

- The **importer** is the machine that can actually run the exporters — the
  one with Signal Desktop's key, Full Disk Access to `chat.db`, and so on. It
  Enables and Refreshes the source from the Providers page, exporting into
  its managed archive.
- A **replica** receives that archive — messages *and* media — over the LAN
  and runs its own local import. Its database is derived locally from the
  synced files; **no database file ever crosses the wire**, and each device
  ends up with the same conversations because it ran the same import over the
  same archive.

msgbrowse enforces the single-importer rule. On a replica, a synced-in
source's Providers card shows a **Synced** badge naming the importer (for
example "Synced from studio-mac"), and Enable/Refresh are not offered — trying
to enable it anyway is refused with a message naming the existing importer.
New messages appear on the replica automatically: when a sync completes,
msgbrowse triggers an incremental import of just the delta.

If you want a different machine to become the importer for a source, unpair
the current importer first — unpairing releases its claim.

## Enabling sync

In your config file (both machines):

```yaml
device_sync:
  enabled: true
  # Optional: the P2P port (default :8788) and this device's friendly name.
  # listen_addr: ":8788"
  # device_name: "studio-mac"
```

- **Desktop app**: the engine is bundled inside the `.app` — version-pinned
  and integrity-checked at launch. Nothing to install.
- **`msgbrowse serve`**: bring your own engine — install `syncthing` on
  `$PATH` or set `device_sync.syncthing_bin`.

When msgbrowse starts with sync enabled, it launches the engine as a
supervised child process (stopped cleanly on quit, restarted with backoff if
it crashes) and configures it itself: the only synced folders are the managed
archive roots under `<data_dir>/archives/<source>`, with ignore rules that
keep the database, its WAL/SHM files, and OS cruft out of every synced
folder.

## Pairing two devices

Pairing is a device-ID exchange from **Settings → Device sync**, and it must
happen **on both machines** — each device explicitly accepts the other before
any data flows:

1. On the first machine, open Settings. It shows this device's **pairing QR
   code** and the same payload as a copyable **manual code** (`MSGB2.…`) plus
   the bare device ID.
2. On the second machine, paste that code (or the device ID) into its own
   Settings pairing form and submit.
3. Repeat in the other direction: carry the second machine's code back to the
   first and pair it there too.
4. Once both sides have accepted each other, the archives sync over the LAN
   and the replica imports them automatically.

The pairing code is **not a secret**. A Syncthing device ID is a public
identifier — the SHA-256 of the device's TLS certificate. Every connection is
mutual TLS with that identity pinned, and possession of a device ID grants
nothing: sync starts only after *both* devices have explicitly paired with
each other. A photographed QR or a leaked code is inert.

## LAN-only by design

msgbrowse configures the engine with **global discovery, relaying, and NAT
traversal off**, and local (LAN) discovery on. Paired devices find each other
on your network with zero configuration, and no archive metadata or bytes
leave the LAN. Usage reporting and crash reporting are permanently declined,
and the engine never self-upgrades — the bundled version only changes with a
msgbrowse release.

The engine's REST/GUI control API binds loopback with a msgbrowse-generated
API key; the only listener beyond loopback is the P2P sync port itself, which
the engine protects with device-ID mutual TLS.

## What runs when

| State | What is running |
|-------|-----------------|
| `device_sync.enabled: false` (default) | Nothing. No engine process, no P2P listener, loopback-only posture. |
| Enabled, app running | One supervised engine child per msgbrowse instance, plus msgbrowse's folder-watch worker that triggers imports when a sync completes. |
| Enabled, app quit | Nothing — the engine is stopped with the app; sync resumes at the next launch. |

## Watching sync health

You never need the engine's own UI:

- **Settings** shows each paired device with its live state (Connected /
  Disconnected / Paused) and last-seen time.
- **Status & backups** has a Device sync card: engine running?, per-device
  connection, and per-archive health with completion percentages — a paused
  or errored folder shows up here, not in some hidden engine console.
- **Logs** carries a Device sync feed of this session's events: pairings,
  accepted folder offers, completed syncs and the imports they triggered,
  peers connecting and disconnecting.
- **`msgbrowse doctor`** reports the full ladder — sync enabled?, engine
  resolved (bundled or bring-your-own)?, engine running?, peers connected?,
  folders healthy with completion — with a remediation hint on anything
  amiss.
- **`msgbrowse devices status`** prints the same engine/peer/folder tables in
  the terminal, and `msgbrowse devices list` shows the paired registry with
  each peer's role.

## Unpairing

Unpair from **Settings** (each paired device row has an Unpair control with a
confirmation step) or from the CLI:

```sh
msgbrowse devices list
msgbrowse devices unpair XW4UY46      # full device ID or any unique prefix
```

Unpairing takes effect immediately and locally: the device is removed from
the engine's configuration and every archive folder is unshared from it —
without needing the other machine to be reachable or cooperative. What it
does **not** do is delete data: archives already synced to either machine
stay on disk and remain browsable; only future synchronization stops. If the
unpaired device was a source's importer, that source becomes enable-able
locally again.

## Troubleshooting

- **"The sync engine is not running" in Settings** — check the Logs page for
  the engine's output; for `serve`, confirm `syncthing` resolves (`msgbrowse
  doctor` shows the resolution row).
- **Peers never connect** — both machines must be on the same LAN with
  msgbrowse (and device sync) running; sync is LAN-only by default, so
  devices on different networks will not find each other.
- **A folder shows Paused or Error** — `msgbrowse doctor` names the condition
  and the fix; folder errors are usually file permissions under the archive
  root.
