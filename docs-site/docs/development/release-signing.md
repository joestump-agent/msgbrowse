---
title: macOS signing & notarization
sidebar_position: 2
description: Owner runbook — from today's ad-hoc-signed .app to Developer ID + notarization, via the six CI secrets desktop.yml already reads.
---

# macOS signing & notarization

This is the **owner runbook** for taking the macOS desktop `.app` from
today's ad-hoc signature to a real **Developer ID + notarized** release. The
CI side is already done — the workflow only waits for the Apple identity and
six repository secrets.

## Current state: ad-hoc signed, owner-gated real signing

The
[`desktop.yml`](https://github.com/joestump/msgbrowse/blob/main/.github/workflows/desktop.yml)
darwin job **ad-hoc signs** (`codesign -s -`) every embedded Mach-O binary
(the bundled Python runtime, the exporter venv, `imessage-exporter`,
`syncthing`) and then the `.app` itself. Ad-hoc signatures are not notarized
and not trusted by Gatekeeper, so today users must strip quarantine before
first launch:

```sh
xattr -dr com.apple.quarantine msgbrowse.app
```

Two user-facing costs follow from this:

- **First launch is blocked** until the `xattr` strip (or the
  Privacy & Security "Open Anyway" dance — and on macOS 15+ the `xattr` strip
  is still the reliable path for the *embedded* binaries).
- **TCC grants die on every update.** Ad-hoc signatures carry no stable
  signing identity, so macOS treats each new build as a different program —
  the Full Disk Access grant that lets the bundled `imessage-exporter` read
  `chat.db` has to be re-granted after every upgrade.

The **real** Developer ID codesign + `notarytool` steps already exist in
`desktop.yml`, each gated on its secret being non-empty. With no secrets set
they skip cleanly and the job stays green; the moment the secrets exist they
take over from the ad-hoc signature automatically. Nothing in the workflow
needs to change.

## One-time owner setup

### 1. Enroll in the Apple Developer Program

Enroll at <https://developer.apple.com/programs/enroll/> (~USD 99/year,
approval typically 24–48 hours). An **individual** membership is fine — the
enrollment name is what Gatekeeper shows users as the publisher.

### 2. Create the "Developer ID Application" certificate

1. Xcode → **Settings → Accounts** → select the Apple ID →
   **Manage Certificates…** → **+** → **Developer ID Application**.
2. In **Keychain Access**, export that certificate (with its private key) as
   a `.p12` file, protected with a strong password.
3. Get the exact identity string CI will sign with:

   ```sh
   security find-identity -v -p codesigning
   ```

   It looks like `Developer ID Application: Your Name (TEAMID)` — that full
   string is the `MACOS_DEVELOPER_ID_APP` secret value.

### 3. Create an App Store Connect API key (for notarytool)

At <https://appstoreconnect.apple.com>: **Users and Access → Integrations →
App Store Connect API → Team Keys** → generate a key with the **Developer**
role.

:::warning The .p8 downloads exactly once
Apple lets you download the private key **one time only**. Download it, store
it somewhere safe, and note the **Key ID** (shown next to the key) and the
**Issuer ID** (shown at the top of the Team Keys page).
:::

### 4. Base64-encode the two files

The workflow decodes the certificate and the API key from base64:

```sh
base64 -i YourDeveloperID.p12 | pbcopy   # → MACOS_CERT_P12
base64 -i AuthKey_XXXXXXXXXX.p8 | pbcopy # → AC_API_KEY_P8
```

## The six repository secrets

Add these under the repo's **Settings → Secrets and variables → Actions**
(New repository secret). The names must match exactly — they are what
`desktop.yml` reads:

| Secret | Value |
| --- | --- |
| `MACOS_DEVELOPER_ID_APP` | The full identity string, e.g. `Developer ID Application: Your Name (TEAMID)` |
| `MACOS_CERT_P12` | The exported certificate `.p12`, base64-encoded |
| `MACOS_CERT_P12_PASSWORD` | The password you set on the `.p12` export |
| `AC_API_KEY_ID` | The App Store Connect API **Key ID** |
| `AC_API_KEY_ISSUER_ID` | The App Store Connect API **Issuer ID** |
| `AC_API_KEY_P8` | The `.p8` private key, base64-encoded |

:::danger Handle key material carefully
Enter the secret values directly into the GitHub secrets form. Never paste
the `.p12`, the `.p8`, or their base64 blobs into chats, issues, commits, or
anywhere else.
:::

## What CI does once the secrets exist

On the next `v*` tag (or manual dispatch) the gated steps in the darwin job
activate, in order:

1. **Import the certificate** into a throwaway CI keychain
   (`MACOS_CERT_P12` + `MACOS_CERT_P12_PASSWORD`).
2. **Developer ID codesign** — every embedded Mach-O under
   `Contents/Resources/tools` is re-signed with
   `codesign --force --options runtime --timestamp` (hardened runtime +
   secure timestamp, both required for notarization), then the `.app` itself,
   then `codesign --verify --deep --strict` proves the result.
3. **Notarize + staple** — the `.app` is zipped and submitted with
   `xcrun notarytool submit --wait` using the three `AC_*` secrets, then the
   ticket is attached with `xcrun stapler staple` and checked with
   `xcrun stapler validate`.

**What changes for users:** download → unzip → open, with only the standard
one-time Gatekeeper "downloaded from the internet" dialog. No `xattr`, no
Open Anyway. And because the signing identity is now stable across releases,
**TCC grants survive updates** — Full Disk Access granted once keeps working
on every subsequent version.

## Verifying and troubleshooting

Confirm a downloaded build really is signed and notarized:

```sh
spctl -a -vv msgbrowse.app          # want: "accepted" + "source=Notarized Developer ID"
xcrun stapler validate msgbrowse.app # want: "The validate action worked!"
codesign -dv --verbose=2 msgbrowse.app # shows the Authority chain + Team ID
```

- `spctl` says `rejected` / `stapler` fails → that build predates the secrets
  (or a gated step was skipped); check the tag's `desktop.yml` run to see
  whether the "Developer ID codesign" and "Notarize + staple" steps executed
  or were skipped.
- If `notarytool submit` reports `Invalid`, fetch the reasons with
  `xcrun notarytool log <submission-id>` using the same `--key/--key-id/--issuer`
  arguments — the usual culprit is an embedded binary that missed the
  hardened-runtime re-sign.
- **Old (ad-hoc) builds keep working** exactly as before: the
  `xattr -dr com.apple.quarantine msgbrowse.app` workaround remains valid for
  anything downloaded before signing landed.
