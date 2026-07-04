package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/joestump/msgbrowse/internal/archivepath"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/imageconv"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/whatsapp"
	"github.com/spf13/cobra"
)

// Doctor is a read-only environment/setup diagnostic. It walks a series of
// checks over the resolved config, the data dir, the read-only archives, and the
// imported attachment rows, printing one human-readable line per check and a
// one-line summary. It is intentionally side-effect free: it opens the store
// read-only (Open does create the data dir and apply migrations, which is the
// only write, and only to msgbrowse's own data dir), reads attachment metadata,
// stats files in the archive, and — only behind --check-llm — does a bare TCP
// connect to the configured llm.base_url host:port (no bytes sent). With
// device sync enabled it additionally probes the SUPERVISED Syncthing
// daemon's loopback REST API (#158; SPEC-0014 REQ "Status and Doctor
// Surfacing") — machine-local by construction, never an egress.

// glyphs prefixing each check line. Plain text so output stays grep-friendly.
const (
	glyphPass = "✓"
	glyphWarn = "⚠"
	glyphFail = "✗"
)

// checkStatus is the outcome of a single doctor check.
type checkStatus int

const (
	statusPass checkStatus = iota
	statusWarn
	statusFail
)

func (s checkStatus) glyph() string {
	switch s {
	case statusFail:
		return glyphFail
	case statusWarn:
		return glyphWarn
	default:
		return glyphPass
	}
}

// report accumulates check results and writes them to a single Writer. Keeping
// the writer here means every line — including the summary — goes to one stream
// (stdout), never the slog logger (which is reserved for stderr).
type report struct {
	w        io.Writer
	warnings int
	fails    int
}

// add prints one check line: "<glyph> <title>" plus an optional indented hint on
// the next line when the check did not pass.
func (r *report) add(status checkStatus, title, hint string) {
	fmt.Fprintf(r.w, "%s %s\n", status.glyph(), title)
	if status != statusPass && hint != "" {
		fmt.Fprintf(r.w, "    %s\n", hint)
	}
	switch status {
	case statusWarn:
		r.warnings++
	case statusFail:
		r.fails++
	}
}

// summary writes the trailing one-liner and reports whether any check failed.
func (r *report) summary() bool {
	fmt.Fprintf(r.w, "doctor: %s, %s\n", plural(r.warnings, "warning"), plural(r.fails, "problem"))
	return r.fails > 0
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func newDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose msgbrowse setup and archive/attachment health (read-only)",
		Long: "doctor runs read-only checks over your configuration, data directory, and\n" +
			"imported archives, then prints a report with a status glyph per check\n" +
			"(✓ pass, ⚠ warn, ✗ problem) and a one-line summary. It exits non-zero only\n" +
			"if a check fails (✗), so it is safe to use in scripts.\n" +
			"\n" +
			"The headline check inspects imported attachment paths: an iMessage export\n" +
			"done WITHOUT copy-mode records absolute ~/Library paths that point outside\n" +
			"the archive, so no media is browsable. doctor flags that and tells you how\n" +
			"to re-export.\n" +
			"\n" +
			"doctor makes NO off-machine network calls except an OPTIONAL TCP-connect\n" +
			"reachability probe (no data sent) to the single configured llm.base_url,\n" +
			"behind --check-llm. With device sync enabled it also reads the local sync\n" +
			"engine's loopback REST API (peer connections, folder completion) — that\n" +
			"traffic never leaves this machine.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			checkLLM, err := cmd.Flags().GetBool("check-llm")
			if err != nil {
				return err
			}
			failed := runDoctor(cmd.Context(), cmd.OutOrStdout(), cfg, checkLLM)
			if failed {
				// Non-zero exit for scripts. Suppress usage/error rendering: the
				// report is the user-facing output, not an error message.
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return errDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().Bool("check-llm", false, "additionally TCP-probe the configured llm.base_url for reachability (the single configured egress; no data is sent)")
	return cmd
}

// errDoctorFailed signals at least one ✗ check so Execute exits non-zero. It is
// rendered specially (no logger line) since the report already explained things.
var errDoctorFailed = doctorFailedError{}

type doctorFailedError struct{}

func (doctorFailedError) Error() string { return "doctor found one or more problems" }

// runDoctor executes every check in order, writing to w, and returns true if any
// check failed (✗). It never returns an error: a failed check is reported as a
// line, not a Go error, so the report is always complete.
func runDoctor(ctx context.Context, w io.Writer, cfg *config.Config, checkLLM bool) bool {
	r := &report{w: w}

	st := checkDataDir(ctx, r, cfg)
	if st != nil {
		defer st.Close()
	}
	checkSignalArchive(r, cfg)
	checkIMessageArchive(r, cfg)
	checkWhatsAppArchive(r, cfg)
	checkAttachments(ctx, r, cfg, st)
	checkConverter(ctx, r, cfg, st)
	checkEmbeddings(ctx, r, cfg, st)
	checkExporters(r)
	checkDeviceSync(ctx, r, cfg, st)
	checkLLMEndpoint(r, cfg, checkLLM)

	return r.summary()
}

// checkDataDir verifies the data dir is writable and reports DB presence, schema
// version, and corpus totals. It returns an open *store.Store (caller closes) or
// nil if the DB couldn't be opened — later checks degrade gracefully on nil.
func checkDataDir(ctx context.Context, r *report, cfg *config.Config) *store.Store {
	if cfg.DataDir == "" {
		r.add(statusFail, "data_dir is not set", "set data_dir (config), --data-dir, or MSGBROWSE_DATA_DIR to a writable directory")
		return nil
	}

	// doctor is a read-only diagnostic: it must NOT create the data dir or the
	// database (a typo'd --data-dir should be reported, not silently created).
	info, err := os.Stat(cfg.DataDir)
	switch {
	case os.IsNotExist(err):
		r.add(statusWarn, fmt.Sprintf("data_dir %q does not exist yet", cfg.DataDir),
			"it's created on first import; run `msgbrowse import` once your archives are configured")
		return nil // nothing to open; don't create anything
	case err != nil:
		r.add(statusFail, fmt.Sprintf("data_dir %q: %v", cfg.DataDir, err), "check the path and permissions")
		return nil
	case !info.IsDir():
		r.add(statusFail, fmt.Sprintf("data_dir %q is not a directory", cfg.DataDir), "point data_dir at a directory")
		return nil
	}
	if err := writable(cfg.DataDir); err != nil {
		r.add(statusFail, fmt.Sprintf("data_dir %q is not writable: %v", cfg.DataDir, err),
			"the database and caches live here; grant write access or choose another data_dir")
		return nil
	}
	r.add(statusPass, fmt.Sprintf("data_dir %q exists and is writable", cfg.DataDir), "")

	if !fileExists(dbPath(cfg)) {
		r.add(statusWarn, "no database yet (no import has run)",
			"run `msgbrowse import` after configuring your archive roots")
		return nil
	}

	// Open read-only and WITHOUT migrating, so we report the true on-disk schema
	// version (drift is meaningful) and never write to the user's DB.
	st, err := store.OpenReadOnly(dbPath(cfg))
	if err != nil {
		r.add(statusFail, fmt.Sprintf("cannot open database (read-only): %v", err), "check data_dir permissions")
		return nil
	}

	if v, err := st.UserVersion(ctx); err != nil {
		r.add(statusWarn, fmt.Sprintf("could not read schema version: %v", err), "")
	} else if v == store.SchemaVersion() {
		r.add(statusPass, fmt.Sprintf("database schema is current (version %d)", v), "")
	} else {
		r.add(statusWarn, fmt.Sprintf("database schema version %d, binary expects %d", v, store.SchemaVersion()),
			"run any msgbrowse command (e.g. `import`) to migrate it forward")
	}

	convs, cerr := st.ListConversations(ctx)
	msgs, merr := st.CountMessages(ctx)
	if cerr != nil || merr != nil {
		r.add(statusWarn, "could not count conversations/messages", firstErr(cerr, merr).Error())
		return st
	}
	if len(convs) == 0 || msgs == 0 {
		r.add(statusWarn, fmt.Sprintf("%d conversations, %d messages", len(convs), msgs),
			"nothing imported yet — run `msgbrowse import`")
	} else {
		r.add(statusPass, fmt.Sprintf("%d conversations, %d messages imported", len(convs), msgs), "")
	}
	return st
}

// checkSignalArchive validates the signal archive root — the EFFECTIVE one:
// the configured archive_root when set, else the managed root a desktop
// onboarding populated (<data_dir>/archives/signal, issue #160). The classic
// mistake it catches is pointing archive_root AT the export/ subdir instead of
// its parent.
func checkSignalArchive(r *report, cfg *config.Config) {
	root := setup.EffectiveRoot(cfg, source.Signal)
	if root == "" {
		r.add(statusWarn, "archive_root (Signal) is not set",
			"set it to the folder that CONTAINS export/ if you want to import Signal; ignore if you only use iMessage")
		return
	}
	info, err := os.Stat(root)
	if err != nil {
		r.add(statusFail, fmt.Sprintf("archive_root %q: %v", root, err),
			"check the path; it must be the read-only signal-export archive root")
		return
	}
	if !info.IsDir() {
		r.add(statusFail, fmt.Sprintf("archive_root %q is not a directory", root), "")
		return
	}
	switch classifyArchiveRoot(root) {
	case archiveRootOK:
		r.add(statusPass, fmt.Sprintf("Signal archive_root %q contains export/", root), "")
	case archiveRootPointsAtExport:
		r.add(statusFail, fmt.Sprintf("archive_root %q points AT export/ (or its contents), not the archive root", root),
			"set archive_root to the PARENT folder — the one that CONTAINS export/, e.g. .../Signal-Archive not .../Signal-Archive/export")
	default: // archiveRootNoExport
		r.add(statusWarn, fmt.Sprintf("archive_root %q has no export/ subdirectory", root),
			"archive_root must contain an export/ folder of per-conversation directories; check you exported with signal-export")
	}
}

// checkIMessageArchive validates the effective iMessage archive root (the
// configured imessage_archive_root, else the managed root — issue #160): it
// should be the flat directory of <ChatName>.txt files.
func checkIMessageArchive(r *report, cfg *config.Config) {
	root := setup.EffectiveRoot(cfg, source.IMessage)
	if root == "" {
		r.add(statusWarn, "imessage_archive_root is not set",
			"set it to the imessage-exporter output directory if you want to import iMessage; ignore if you only use Signal")
		return
	}
	info, err := os.Stat(root)
	if err != nil {
		r.add(statusFail, fmt.Sprintf("imessage_archive_root %q: %v", root, err),
			"check the path; it must be the imessage-exporter output directory")
		return
	}
	if !info.IsDir() {
		r.add(statusFail, fmt.Sprintf("imessage_archive_root %q is not a directory", root), "")
		return
	}
	n, err := countTxtFiles(root)
	if err != nil {
		r.add(statusWarn, fmt.Sprintf("could not scan imessage_archive_root %q: %v", root, err), "")
		return
	}
	if n == 0 {
		r.add(statusWarn, fmt.Sprintf("imessage_archive_root %q has no *.txt files", root),
			"this should be the imessage-exporter output (a folder of <ChatName>.txt files); re-run with `-f txt` and point here")
		return
	}
	r.add(statusPass, fmt.Sprintf("imessage_archive_root %q has %d *.txt file(s)", root, n), "")
}

// checkWhatsAppArchive validates the whatsapp_archive_root per SPEC-0009
// REQ-0009-009: the directory must exist and contain the exporter's
// result.json, no chat may reference media through an absolute media_base
// outside the root, and a sample of media references must resolve to files
// inside the root. Remediation hints are platform-aware (the export records
// the device type): iOS needs a local Finder/iTunes backup, Android the
// backup plus its 64-digit key.
func checkWhatsAppArchive(r *report, cfg *config.Config) {
	// Effective root: the configured whatsapp_archive_root, else the managed
	// root a desktop onboarding populated (issue #160).
	root := setup.EffectiveRoot(cfg, source.WhatsApp)
	if root == "" {
		r.add(statusWarn, "whatsapp_archive_root is not set",
			"set it to the WhatsApp-Chat-Exporter output directory (the folder containing result.json) if you want to import WhatsApp; ignore otherwise")
		return
	}
	info, err := os.Stat(root)
	if err != nil {
		r.add(statusFail, fmt.Sprintf("whatsapp_archive_root %q: %v", root, err),
			"check the path; it must be the wtsexporter output directory (the folder containing result.json)")
		return
	}
	if !info.IsDir() {
		r.add(statusFail, fmt.Sprintf("whatsapp_archive_root %q is not a directory", root), "")
		return
	}

	path := filepath.Join(root, whatsapp.ResultFile)
	f, err := os.Open(path)
	if err != nil {
		r.add(statusWarn, fmt.Sprintf("whatsapp_archive_root %q has no %s", root, whatsapp.ResultFile),
			fmt.Sprintf("run `msgbrowse export` (wtsexporter with --json) into this directory; %s", whatsappBackupHint("")))
		return
	}
	defer f.Close()
	sum, err := whatsapp.ScanExport(f, whatsappMediaSampleLimit)
	if err != nil {
		r.add(statusFail, fmt.Sprintf("could not parse %s: %v", path, err),
			fmt.Sprintf("this should be the wtsexporter JSON export; re-run `msgbrowse export`; %s", whatsappBackupHint(sum.Device)))
		return
	}
	r.add(statusPass, fmt.Sprintf("whatsapp_archive_root %q has %s (%d chats, %d messages)",
		root, whatsapp.ResultFile, sum.Chats, sum.Messages), "")

	// Headline WhatsApp check: a chat-level media_base that is absolute and
	// points outside the root means the exporter referenced the media folder
	// in place instead of copying it under the root — none of that media is
	// browsable from the archive.
	if n, example := absMediaBasesOutside(root, sum.MediaBaseChats); n > 0 {
		r.add(statusFail, fmt.Sprintf("%d WhatsApp chat(s) reference media through an absolute media_base outside the archive (e.g. %q)", n, example),
			fmt.Sprintf("media was not copied into the export; re-run the exporter so the media folder lands under whatsapp_archive_root (wtsexporter copies -m into -o by default), then `msgbrowse import --full`; %s", whatsappBackupHint(sum.Device)))
	}

	if len(sum.MediaRefs) == 0 {
		r.add(statusPass, "no WhatsApp media references to check", "")
		return
	}
	var s whatsappMediaStats
	for _, ref := range sum.MediaRefs {
		s.Add(classifyWhatsAppMedia(root, ref, os.Stat))
	}
	status, hint := whatsappMediaVerdict(sum.Device, &s)
	r.add(status, fmt.Sprintf("WhatsApp media references: %d ok, %d outside the root, %d missing (of %d sampled)",
		s.Present, s.Outside, s.Missing, s.Total()), hint)
}

// checkAttachments is the headline check: sample image attachments and classify
// each by how its stored rel_path resolves on disk. A large absolute-path or
// missing-file share for iMessage means the export was not copy-mode.
func checkAttachments(ctx context.Context, r *report, cfg *config.Config, st *store.Store) {
	if st == nil {
		return // no DB; nothing imported to inspect
	}
	items, err := st.ListImageAttachments(ctx)
	if err != nil {
		r.add(statusWarn, "could not list image attachments", err.Error())
		return
	}
	if len(items) == 0 {
		r.add(statusPass, "no image attachments to check", "")
		return
	}

	sample := items
	if len(sample) > attachmentSampleLimit {
		sample = sample[:attachmentSampleLimit]
	}

	bySource := map[string]*attachmentStats{}
	roots := archiveRoots(cfg)
	for _, it := range sample {
		s := bySource[it.Source]
		if s == nil {
			s = &attachmentStats{}
			bySource[it.Source] = s
		}
		s.Add(classifyAttachment(it.Source, roots, it.ConversationName, it.RelPath, os.Stat))
	}

	for _, src := range sortedSources(bySource) {
		s := bySource[src]
		label := source.Label(src)
		status, hint := attachmentVerdict(src, s)
		title := fmt.Sprintf("%s attachments: %d ok, %d absolute, %d missing (of %d sampled)",
			label, s.Present, s.Absolute, s.Missing, s.Total())
		if len(items) > len(sample) {
			title = fmt.Sprintf("%s attachments: %d ok, %d absolute, %d missing (of %d sampled, %d total images)",
				label, s.Present, s.Absolute, s.Missing, s.Total(), len(items))
		}
		r.add(status, title, hint)
	}
}

// checkConverter reports the image converter and how many convertible (HEIC/
// TIFF) attachments lack a cached derivative.
func checkConverter(ctx context.Context, r *report, cfg *config.Config, st *store.Store) {
	conv, ok := imageconv.Detect()
	if ok {
		r.add(statusPass, fmt.Sprintf("image converter found: %s", conv.Name), "")
	} else {
		r.add(statusWarn, "no image converter found (sips / magick / convert / heif-convert)",
			"HEIC/TIFF attachments will show placeholders; install one (e.g. ImageMagick or libheif) and run `msgbrowse media`")
	}

	if st == nil {
		return
	}
	items, err := st.ListImageAttachments(ctx)
	if err != nil {
		return // already surfaced in checkAttachments
	}
	derivedDir := imageconv.DerivedDir(cfg.DataDir)
	roots := archiveRoots(cfg)
	var needDeriv int
	for _, it := range items {
		if !imageconv.Convertible(it.RelPath) {
			continue
		}
		abs, resolved := archivepath.Resolve(it.Source, roots, it.ConversationName, it.RelPath)
		if !resolved {
			continue // unresolvable (e.g. absolute path) — not a transcode candidate
		}
		if _, serr := os.Stat(abs); serr != nil {
			continue // source missing — transcode can't help
		}
		if _, derr := os.Stat(imageconv.DerivedPath(derivedDir, abs)); derr != nil {
			needDeriv++
		}
	}
	if needDeriv > 0 {
		status := statusWarn
		hint := fmt.Sprintf("run `msgbrowse media` to transcode %d HEIC/TIFF image(s) for the gallery", needDeriv)
		if !ok {
			hint = "install an image converter first, then run `msgbrowse media`"
		}
		r.add(status, fmt.Sprintf("%d convertible image(s) lack a cached derivative", needDeriv), hint)
	}
}

// checkEmbeddings reports how many messages still need embedding.
func checkEmbeddings(ctx context.Context, r *report, cfg *config.Config, st *store.Store) {
	if st == nil {
		return
	}
	n, err := st.CountMissingEmbeddings(ctx, cfg.LLM.EmbedModel)
	if err != nil {
		r.add(statusWarn, "could not count missing embeddings", err.Error())
		return
	}
	if n == 0 {
		r.add(statusPass, "all messages are embedded", "")
		return
	}
	r.add(statusWarn, fmt.Sprintf("%d message(s) not embedded for model %q", n, cfg.LLM.EmbedModel),
		"run `msgbrowse embed` (needs the configured LLM endpoint) to enable semantic search")
}

// checkExporters looks for the upstream export tools msgbrowse may shell out to
// for the planned export feature. Missing tools are informational warnings.
func checkExporters(r *report) {
	for _, e := range []struct{ bin, hint string }{
		// The Signal exporter's console command is `sigexport` (the pip *package*
		// is signal-export); `msgbrowse export` looks up this same binary.
		{"sigexport", "needed only if you want msgbrowse to run Signal exports; install via pipx: `pipx install signal-export` (the command is `sigexport`)"},
		{"imessage-exporter", "needed only if you want msgbrowse to run iMessage exports; install via Homebrew: `brew install imessage-exporter`"},
		// Same pip-package-vs-command confusion as sigexport: the package is
		// whatsapp-chat-exporter, the console command `wtsexporter`.
		{"wtsexporter", "needed only if you want msgbrowse to run WhatsApp exports; install via pipx: `pipx install whatsapp-chat-exporter` (the command is `wtsexporter`); " + whatsappBackupHint("")},
	} {
		if _, err := exec.LookPath(e.bin); err == nil {
			r.add(statusPass, fmt.Sprintf("exporter %q found on PATH", e.bin), "")
		} else {
			r.add(statusWarn, fmt.Sprintf("exporter %q not found on PATH", e.bin), e.hint)
		}
	}
}

// checkLLMEndpoint optionally TCP-probes the configured llm.base_url. It is the
// only network operation doctor performs, and only with --check-llm. No request
// body is sent — it opens and closes a connection to confirm reachability.
func checkLLMEndpoint(r *report, cfg *config.Config, checkLLM bool) {
	if !checkLLM {
		return
	}
	host, err := hostPort(cfg.LLM.BaseURL)
	if err != nil {
		r.add(statusWarn, fmt.Sprintf("could not parse llm.base_url %q: %v", cfg.LLM.BaseURL, err), "")
		return
	}
	conn, err := net.DialTimeout("tcp", host, llmProbeTimeout)
	if err != nil {
		r.add(statusWarn, fmt.Sprintf("llm endpoint %s not reachable: %v", host, err),
			"embed/facts/journal need this endpoint; this is the single configured egress (llm.base_url)")
		return
	}
	_ = conn.Close()
	r.add(statusPass, fmt.Sprintf("llm endpoint %s reachable (TCP connect only; no data sent)", host), "")
}

// --- testable decision logic -------------------------------------------------
//
// The archive/attachment-health decision primitives moved to internal/setup so
// the desktop Setup surface (SPEC-0013) and this CLI report share one
// implementation (ADR-0020 "reusable package"). The unexported aliases below
// keep doctor's call sites, tests, and — crucially — its report wording
// byte-for-byte unchanged: they delegate to setup and translate setup.Health
// into the report's ✓/⚠/✗ checkStatus.

// attachmentSampleLimit caps how many image attachments checkAttachments
// inspects; sampling keeps doctor fast on large archives while still being
// representative of a misconfigured export.
const attachmentSampleLimit = setup.AttachmentSampleLimit

// llmProbeTimeout bounds the optional TCP reachability probe.
const llmProbeTimeout = 2 * time.Second

// attachmentClass and its values alias setup's so doctor's tests keep referring
// to the local names while the logic lives in one place.
type attachmentClass = setup.AttachmentClass

const (
	attachPresent  = setup.AttachPresent
	attachAbsolute = setup.AttachAbsolute
	attachMissing  = setup.AttachMissing
)

// classifyAttachment delegates to setup.ClassifyAttachment.
func classifyAttachment(src string, roots archivepath.Roots, convName, rel string, statFn func(string) (os.FileInfo, error)) attachmentClass {
	return setup.ClassifyAttachment(src, roots, convName, rel, statFn)
}

// attachmentStats aliases setup's so tests can construct it with the same
// field names and pass it to attachmentVerdict.
type attachmentStats = setup.AttachmentStats

// attachmentVerdict delegates to setup.AttachmentVerdict and maps its
// setup.Health back to the report's checkStatus, preserving doctor's exact
// hint wording and pass/warn/fail behavior.
func attachmentVerdict(src string, s *attachmentStats) (checkStatus, string) {
	h, hint := setup.AttachmentVerdict(src, s)
	return statusFromHealth(h), hint
}

// statusFromHealth maps a setup.Health verdict onto the report's checkStatus so
// the ✓/⚠/✗ glyphs and exit code are unchanged.
func statusFromHealth(h setup.Health) checkStatus {
	switch h {
	case setup.HealthProblem:
		return statusFail
	case setup.HealthWarn:
		return statusWarn
	default:
		return statusPass
	}
}

// --- WhatsApp export health (SPEC-0009 REQ-0009-009) -------------------------
//
// As with the attachment logic above, the WhatsApp media-health primitives live
// in internal/setup; these unexported aliases delegate so doctor's report
// wording and its checks' fail/warn behavior are unchanged while the desktop
// Setup surface shares the same code.

// whatsappMediaSampleLimit caps how many media references checkWhatsAppArchive
// samples from result.json, mirroring attachmentSampleLimit's rationale.
const whatsappMediaSampleLimit = setup.WhatsAppMediaSampleLimit

// whatsappMediaClass and its values alias setup's.
type whatsappMediaClass = setup.WhatsAppMediaClass

const (
	whatsappMediaPresent = setup.WhatsAppMediaPresent
	whatsappMediaOutside = setup.WhatsAppMediaOutside
	whatsappMediaMissing = setup.WhatsAppMediaMissing
)

// classifyWhatsAppMedia delegates to setup.ClassifyWhatsAppMedia.
func classifyWhatsAppMedia(root string, ref whatsapp.MediaRef, statFn func(string) (os.FileInfo, error)) whatsappMediaClass {
	return setup.ClassifyWhatsAppMedia(root, ref, statFn)
}

// whatsappMediaStats aliases setup's.
type whatsappMediaStats = setup.WhatsAppMediaStats

// whatsappMediaVerdict delegates to setup.WhatsAppMediaVerdict and maps its
// verdict onto the report's checkStatus.
func whatsappMediaVerdict(device string, s *whatsappMediaStats) (checkStatus, string) {
	h, hint := setup.WhatsAppMediaVerdict(device, s)
	return statusFromHealth(h), hint
}

// absMediaBasesOutside delegates to setup.AbsMediaBasesOutside.
func absMediaBasesOutside(root string, mediaBaseChats map[string]int) (int, string) {
	return setup.AbsMediaBasesOutside(root, mediaBaseChats)
}

// whatsappBackupHint delegates to setup.WhatsAppBackupHint.
func whatsappBackupHint(device string) string {
	return setup.WhatsAppBackupHint(device)
}

// archiveRootKind and its values alias setup's.
type archiveRootKind = setup.ArchiveRootKind

const (
	archiveRootOK             = setup.ArchiveRootOK
	archiveRootPointsAtExport = setup.ArchiveRootPointsAtExport
	archiveRootNoExport       = setup.ArchiveRootNoExport
)

// classifyArchiveRoot delegates to setup.ClassifyArchiveRoot.
func classifyArchiveRoot(root string) archiveRootKind {
	return setup.ClassifyArchiveRoot(root)
}

// hostPort extracts a dialable host:port from an llm.base_url, defaulting the
// port from the scheme when absent. Returns an error for an unparseable URL.
func hostPort(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host in URL")
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	port := "80"
	if u.Scheme == "https" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// --- small filesystem helpers ------------------------------------------------

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// writable confirms dir accepts a write by creating and removing a temp file.
func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".doctor-write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// countTxtFiles counts *.txt files directly under dir (non-recursive — the
// imessage-exporter txt output is a flat directory of <ChatName>.txt files).
func countTxtFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var n int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) == ".txt" {
			n++
		}
	}
	return n, nil
}

func sortedSources(m map[string]*attachmentStats) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
