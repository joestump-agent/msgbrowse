# ADR-0009: Configuration & CLI — Cobra (commands) + Viper (config)

- **Status:** Accepted (amended 2026-07-11 — see *Amendment*)
- **Date:** 2026-06-27
- **Relates to:** [ADR-0003](0003-dual-source-archive.md) (per-source archive roots), [ADR-0010](0010-security-privacy-posture.md) (secrets via env)

## Amendment (2026-07-11): the LLM API key may be stored in the config file

The Settings → LLM tab (issue #191) lets a **desktop** user edit the LLM API
key in the UI and persists it to the config file. This softens the original
"secrets never live in the config file" rule to a narrower one:

- **The key may live in the config file when the user puts it there** — a
  desktop user has no convenient shell to export `MSGBROWSE_LLM_API_KEY`, and the
  file is written mode `0600` under [ADR-0010](0010-security-privacy-posture.md)'s
  loopback single-user trust. The tab shows only whether a key is set (never the
  value), a blank field keeps the current key, and an explicit *Clear* control
  wipes it.
- **An env-provided key is still env-only.** When `MSGBROWSE_LLM_API_KEY` is set
  it overrides the file at boot (unchanged precedence) *and* is never written back
  to the config file — saving the tab suppresses the on-disk copy so an
  env-scoped secret cannot leak onto disk (`internal/cli/common.go`
  `newLLMApplier`, `llm.Settings.APIKeyFromEnv`). Deployments that inject the key
  via env keep exactly the old posture.
- **Committing the key is still discouraged.** The file remains the *local* app
  config, not a committed one; `SECURITY.md` and [ADR-0010](0010-security-privacy-posture.md)
  keep the "don't commit a real key" guidance.

## Context

msgbrowse is a multi-command binary (`import`, `signal-import`,
`imessage-import`, `whatsapp-import`, `devices`, `doctor`, `export`, `sync`,
`embed`, `facts`, `media`, `serve`, `mcp`, `watch`, `journal`, `version`) that
must be configurable three ways for three deployment styles: a committed-ish `config.yaml` for stable
settings, environment variables for Docker/secrets, and flags for one-off
overrides. It needs `--help`, subcommands, and a single resolved config object
each command can rely on — with a predictable precedence and a hard rule that
secrets never live in the config file.

## Decision

**Cobra for the command tree, Viper for configuration, wired so precedence is
defaults < config.yaml < `MSGBROWSE_*` env < flags.**

1. **Cobra command tree.** `NewRootCommand` (`internal/cli/root.go`) defines the
   root `msgbrowse` command and attaches every subcommand (each in its own file).
   The root owns persistent flags shared by all commands (`--config`,
   `--archive-root`, `--imessage-archive-root`, `--whatsapp-archive-root`,
   `--data-dir`, `--log-level`) and
   sets `SilenceUsage`/`SilenceErrors` so failures render through the logger, not
   as a usage dump.
2. **Viper config, single lifecycle.** The root's `PersistentPreRunE` runs
   `initConfig` once (after flag parsing, before any subcommand `RunE`):
   `config.Load` builds a `*viper.Viper` with defaults + optional config file +
   env, then `BindPFlag` binds each persistent flag onto its config key. Each
   subcommand calls `resolveConfig`, which unmarshals to a `*config.Config`,
   validates, and configures the logger.
3. **Precedence: defaults < file < env < flags.** `config.SetDefaults` is the
   single source of truth for built-in defaults. `Load` then reads
   `config.yaml` (searched in `.`, `$HOME/.config/msgbrowse`, `/etc/msgbrowse`, or
   an explicit `--config`), layers `AutomaticEnv`, and the CLI binds flags last.
   Because Viper consults `pflag.Changed` for bound flags, **only flags the user
   actually set** override file/env values.
4. **Env prefix + replacer.** `SetEnvPrefix("MSGBROWSE")` plus
   `SetEnvKeyReplacer(strings.NewReplacer(".", "_"))` maps nested keys to env
   vars — e.g. `MSGBROWSE_LLM_API_KEY` → `llm.api_key`,
   `MSGBROWSE_ARCHIVE_ROOT` → `archive_root`.
5. **Per-source archive roots.** Distinct read-only roots — `archive_root`
   (signal-export), `imessage_archive_root` (imessage-exporter), and
   `whatsapp_archive_root` (WhatsApp-Chat-Exporter,
   [ADR-0016](0016-whatsapp-source-exporter.md)) — are separate config
   keys/flags ([ADR-0003](0003-dual-source-archive.md)), each with its own
   `errorHint` when mis-pointed.
6. **Secrets via env (or the 0600 config file the user chose).**
   `LLMConfig.APIKey` still defaults to `""` and env injection
   (`MSGBROWSE_LLM_API_KEY`) remains the recommended path for server/Docker
   deployments. As of the *Amendment* above, a desktop user may also store the
   key in the mode-`0600` config file via the Settings → LLM tab; an env-provided
   key is never persisted there. The key is never committed by default and the UI
   never renders it back.
7. **Validation up front.** `config.Validate` rejects an invalid
   `vector_backend`, an invalid `log_level`, and an empty `data_dir` before any
   command does work.

## Why these choices

- **Cobra + Viper over a hand-rolled flag parser:** they are the de-facto Go pair
  for a multi-command, multi-source config tool — subcommands, help, persistent
  flags, and layered config with flag binding, all without bespoke plumbing.
- **`PersistentPreRunE` over `cobra.OnInitialize`:** it receives the invoked
  command's flag set directly, so flag binding targets the right `pflag.FlagSet`
  and config loads exactly once per invocation.
- **`pflag.Changed`-aware binding:** gives the intuitive precedence (a flag only
  wins when actually passed), so env/file values aren't clobbered by a flag's
  zero default.
- **Env-only secrets:** the cleanest way to keep an API key out of git while still
  supporting Docker/secret-store injection; the default of `""` makes the
  local-by-default LLM route work with no secret at all.

## Consequences

### Positive

- One config object, one precedence rule, three input methods — predictable
  across `serve`, the importers, and `mcp`.
- Docker/secrets work cleanly via `MSGBROWSE_*` env without a config file.
- Adding a setting is a `SetDefaults` entry plus a struct field; adding a source's
  root is a new key/flag, no parser changes.

### Negative

- Config flows through three layers (`Load` → `BindPFlag` → `Unmarshal`), with
  package-level `v` and `cfgFile` vars in `cli` — convenient but global state.
- Viper's case-insensitive keys and the `.`→`_` replacer are conventions a
  contributor must know to name an env var correctly.

### Operational

- The config file is optional; a missing file is not an error (defaults + env +
  flags still apply).
- Secrets must be supplied via env (`MSGBROWSE_LLM_API_KEY`); a key placed in
  `config.yaml` is discouraged and risks being committed.

## Alternatives considered

- **stdlib `flag` + manual env/file parsing.** Rejected: reimplements
  subcommands, help, and layered precedence that Cobra+Viper provide.
- **Flags only (no config file).** Rejected: a long-lived `serve`/journal setup
  wants a stable `config.yaml`; flags alone are unwieldy for many keys.
- **Secrets in the config file (as the only mechanism).** Rejected: env injection
  stays the recommended path for server/Docker so a key need never touch disk. The
  *Amendment* adds config-file storage as an **opt-in desktop convenience** (mode
  `0600`, env still wins and is never persisted), not a replacement — committing a
  real key remains discouraged and matches the egress model in SECURITY.md.

## References

- `internal/cli/root.go` (command tree, `initConfig`, `resolveConfig`, flag binding)
- `internal/config/config.go` (`SetDefaults`, `Load`, env prefix/replacer, `Validate`)
- [ADR-0003: Dual-source archive](0003-dual-source-archive.md)
- [ADR-0010: Security & privacy posture](0010-security-privacy-posture.md)
