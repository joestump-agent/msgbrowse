# msgbrowse

Self-hosted, local-only browser, search engine, and AI-editorialized journal over
your Signal, iMessage, and WhatsApp archives. Go + HTMX + SQLite; nothing leaves
the machine except calls to one configurable OpenAI-compatible LLM endpoint —
plus, when opt-in device sync is enabled (ADR-0021), LAN-only Syncthing traffic
to explicitly paired devices.

See [README.md](README.md) for usage, [ARCHITECTURE.md](ARCHITECTURE.md) for the
layering, and [SECURITY.md](SECURITY.md) for the threat model.

## Architecture Context

- Architecture Decision Records are in docs/adr/
- Specifications are in docs/openspec/specs/

Each spec is a paired artifact: `spec.md` (requirements) and `design.md`
(architecture + rationale). ADRs use MADR format.

### SDD Configuration

#### Tracker
- **Type**: github
- **Owner**: joestump
- **Repo**: msgbrowse

#### Branch Conventions
- **Enabled**: true
- **Prefix**: feature
- **Epic Prefix**: epic

#### PR Conventions
- **Enabled**: true
- **Ref Keyword**: Part of
- **Include Spec Reference**: true

#### Review
- **Max Pairs**: 2
- **Merge Strategy**: squash
- **Auto Cleanup**: false
