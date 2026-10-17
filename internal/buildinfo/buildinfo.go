// Package buildinfo carries what the binary was built from, stamped in by the
// Dockerfile with -ldflags "-X".
package buildinfo

import "runtime/debug"

var (
	// Version is the release tag the image was built from; "dev" otherwise.
	Version = "dev"
	// LibdaveVersion is the libdave release the binary links against.
	LibdaveVersion = "unknown"
)

// Revision returns the short commit the binary was built from, suffixed with
// "-dirty" for uncommitted changes, or "" when the build carries no VCS stamp.
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if revision != "" && modified == "true" {
		revision += "-dirty"
	}
	return revision
}
