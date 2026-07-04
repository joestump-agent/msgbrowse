// EffectiveRoots: the single answer to "where is this source's archive?" for
// every consumer of archive roots — the web media handler, the imageconv
// transcode pipeline, and doctor.
//
// The desktop app never sets cfg.*ArchiveRoot: it imports into the app-owned
// MANAGED roots (<data_dir>/archives/<source>, SPEC-0013 REQ "App-owned, hidden
// data and archive roots"). Before this helper existed, every root consumer
// built archivepath.Roots straight from the config fields, so on desktop every
// /media resolve failed ("invalid path", broken image tiles — issue #160).
//
// Precedence, per source:
//  1. an explicitly configured cfg root (the CLI/server BYO-archive path) wins;
//  2. otherwise the managed root, but only if it actually EXISTS as a directory
//     on disk — a merely computable path is not an archive;
//  3. otherwise empty: the source stays unconfigured and its attachments do not
//     resolve, exactly as before.
//
// Governing: ADR-0020 (the app owns the managed archive roots), SPEC-0013
// (managed layout; the imported archive must be browsable), issue #160.
package setup

import (
	"os"

	"github.com/joestump/msgbrowse/internal/archivepath"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/source"
)

// EffectiveRoot resolves one source's archive root: the configured root when
// set, else that source's managed root iff it exists on disk, else "".
func EffectiveRoot(cfg *config.Config, src string) string {
	var configured string
	switch src {
	case source.Signal:
		configured = cfg.ArchiveRoot
	case source.IMessage:
		configured = cfg.IMessageArchiveRoot
	case source.WhatsApp:
		configured = cfg.WhatsAppArchiveRoot
	default:
		return ""
	}
	if configured != "" {
		return configured
	}
	managed, err := ManagedRoot(cfg.DataDir, src)
	if err != nil {
		return ""
	}
	info, err := os.Stat(managed)
	if err != nil || !info.IsDir() {
		return ""
	}
	return managed
}

// EffectiveRoots bundles every source's effective root for
// archivepath.Resolve consumers (web media serving, imageconv, doctor).
func EffectiveRoots(cfg *config.Config) archivepath.Roots {
	return archivepath.Roots{
		Signal:   EffectiveRoot(cfg, source.Signal),
		IMessage: EffectiveRoot(cfg, source.IMessage),
		WhatsApp: EffectiveRoot(cfg, source.WhatsApp),
	}
}
