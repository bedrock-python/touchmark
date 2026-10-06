// Package docsurl names the pages of touchmark's documentation site that its
// messages, flag help and generated files send people to. A message names a
// page anyone can open, never an internal design document.
//
// TestPagesExist checks every URL against docs/: the page exists, and so
// does the heading of its anchor.
package docsurl

// Base is the root of the documentation site.
const Base = "https://bedrock-python.github.io/touchmark/"

const (
	// WriteIsolation explains security.write_isolation: where the write key
	// lives, the protected environment of the distribute job, and the probe
	// that checks no other job sees the key.
	WriteIsolation = Base + "concepts/security/#the-write-key-stays-on-the-default-branch"
	// Migrate is the move from a multi-gitter setup.
	Migrate = Base + "guide/migrate/"
	// GiteaForgejo is a hub on Gitea or Forgejo, and its two ways to run.
	GiteaForgejo = Base + "getting-started/gitea-forgejo/"
)

// GettingStarted returns the page that sets up a hub on platform, github or
// gitlab, by hand.
func GettingStarted(platform string) string { return Base + "getting-started/" + platform + "/" }
