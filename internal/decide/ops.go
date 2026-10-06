package decide

import (
	"slices"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
)

// TargetOps are the operations.yml entries for one target (see
// docs/reference/operations.md). Every entry limits itself, so inactive
// ones simply do nothing.
type TargetOps struct {
	// RecreateHeads are the heads of recreate entries for the target; a
	// branch is rebuilt only while its head is one of them.
	RecreateHeads []string
	// Forget are PR numbers of forget_declines entries for the target.
	Forget []int64
	// AdoptUnmarked is set while adopt_unmarked is active (until date not
	// past).
	AdoptUnmarked bool
}

// OpsFor collects the entries for target. Entry targets are matched with
// the same rules as targets.yml repo entries: "provider:path" or "path"
// (resolved to the only provider or defaults.provider), paths compared
// case-insensitively. now decides whether dated entries are active (until
// is inclusive, UTC).
//
// Details:
//   - provider is the target's provider id and path its path (the canonical
//     one; a caller that also knows the paths targets.yml wrote may call
//     OpsFor for each and merge). Matching is config.Names, except that a
//     bare path matches nothing when the hub has several providers and
//     targets.yml sets no defaults.provider: an entry that bypasses a
//     safeguard never guesses its provider (`check` rejects such an entry,
//     and delivery leaves out a targets.yml entry of that kind too). A hub
//     without providers in hub.yml has one implicit provider, which a bare
//     path names.
//   - Entries that do not parse, recreate heads that are not full commit
//     ids (40 or 64 hex digits) and PR numbers below 1 are skipped:
//     ParseOperations rejects them, operations built in code may hold them.
//   - Heads are lowercased. Both lists keep file order, each value once.
//   - AdoptUnmarked does not depend on the target.
//
// Nil ops has no entries; nil hub and targets are a legacy hub and an empty
// targets.yml.
func OpsFor(ops *config.Operations, hub *config.Hub, targets *config.Targets, provider, path string, now time.Time) TargetOps {
	var out TargetOps
	if ops == nil {
		return out
	}
	out.AdoptUnmarked = ops.AdoptUnmarked.Active(now)
	for _, r := range ops.Recreate {
		head := strings.ToLower(r.Head)
		if opsCommitID(head) && !slices.Contains(out.RecreateHeads, head) && opsNames(hub, targets, r.Target, provider, path) {
			out.RecreateHeads = append(out.RecreateHeads, head)
		}
	}
	for _, f := range ops.ForgetDeclines {
		if f.PR >= 1 && !slices.Contains(out.Forget, f.PR) && opsNames(hub, targets, f.Target, provider, path) {
			out.Forget = append(out.Forget, f.PR)
		}
	}
	return out
}

// opsNames reports whether the target of an operations.yml entry names the
// repository at path on provider (see OpsFor).
func opsNames(hub *config.Hub, targets *config.Targets, target, provider, path string) bool {
	ref, err := config.ParseRef(target)
	if err != nil {
		return false
	}
	ambiguous := hub != nil && len(hub.Providers) > 1 && (targets == nil || targets.Defaults.Provider == "")
	if ref.Provider == "" && ambiguous {
		return false
	}
	return config.Names(hub, targets, ref, provider, path)
}

// opsCommitID reports whether s is a full commit id: 40 (sha1) or 64
// (sha256) lowercase hex digits.
func opsCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

// ActiveMassClose returns allow_mass_close when its until date is not past,
// else nil.
//
// The result is a copy: changing it leaves ops alone. Nil ops has none.
func ActiveMassClose(ops *config.Operations, now time.Time) *config.MassCloseOp {
	if ops == nil || !ops.AllowMassClose.Active(now) {
		return nil
	}
	allow := *ops.AllowMassClose
	return &allow
}
