// Package config reads hub.yml, targets.yml and the target's opt-in file,
// validates them and resolves which packs a target gets.
//
// Decoding is strict: unknown keys are errors, so a typo never silently
// disables a setting. The JSON Schemas in schemas/ describe the same formats
// for editors; tests keep the two in sync.
//
// The formats are v1. The fields only delivery uses (providers, security,
// limits, memory, pr, commit) are parsed and validated here too, so check
// catches their mistakes before plan or distribute runs.
package config

import (
	"fmt"
	"regexp"
	"time"
)

// Default file names.
const (
	HubFile       = "hub.yml"
	TargetsFile   = "targets.yml"
	DefaultOptIn  = ".engineering-assets.yml"
	PlaceholderID = "change-me"
)

// Warning is a non-fatal finding.
type Warning struct {
	File    string
	Message string
}

func (w Warning) String() string { return fmt.Sprintf("%s: %s", w.File, w.Message) }

// Hub is hub.yml. A missing hub.yml means a legacy hub, one from before
// hub.yml existed: Legacy is set and every field has its default.
type Hub struct {
	Version              int                 `yaml:"version"`
	ID                   string              `yaml:"id"`
	Branch               string              `yaml:"branch"`
	BranchAliases        []string            `yaml:"branch_aliases"`
	PreviousFingerprints []string            `yaml:"previous_fingerprints"`
	OptInFile            string              `yaml:"opt_in_file"`
	Commit               Commit              `yaml:"commit"`
	PR                   PR                  `yaml:"pr"`
	Limits               Limits              `yaml:"limits"`
	Memory               Memory              `yaml:"memory"`
	Security             Security            `yaml:"security"`
	Providers            []Provider          `yaml:"providers"`
	SensitivePaths       []string            `yaml:"sensitive_paths"`
	Packs                map[string]PackMeta `yaml:"packs"`

	// Single-provider shorthand: writer and sign at the top
	// level; platform and base_url accepted as synonyms of type and url.
	// Without platform, the type and URL come from the CI environment or the
	// hub's origin remote (ResolveProviders).
	Writer   string `yaml:"writer"`
	Sign     string `yaml:"sign"`
	Platform string `yaml:"platform"`
	BaseURL  string `yaml:"base_url"`

	Legacy bool `yaml:"-"`
}

type Commit struct {
	Message string `yaml:"message"`
	// Author is removed in v1 (the author is always the writer). It is still
	// accepted so old files parse; Validate warns and ignores it.
	Author *struct {
		Name  string `yaml:"name"`
		Email string `yaml:"email"`
	} `yaml:"author"`
}

type PR struct {
	Title     string   `yaml:"title"`
	Labels    []string `yaml:"labels"`
	Draft     bool     `yaml:"draft"`
	IntroFile string   `yaml:"intro_file"`
	LinkHub   string   `yaml:"link_hub"` // auto | always | never
}

type Limits struct {
	MaxNewPRsPerRun  int     `yaml:"max_new_prs_per_run"`
	MaxCloseFraction float64 `yaml:"max_close_fraction"`
}

type Memory struct {
	AutoCloseCooldown string `yaml:"auto_close_cooldown"` // e.g. "30d"
}

type Security struct {
	WriteIsolation            string `yaml:"write_isolation"` // platform | external | none
	Reason                    string `yaml:"reason"`
	PrivateTargetsInPublicHub string `yaml:"private_targets_in_public_hub"` // skip | deliver
}

type Provider struct {
	ID                 string         `yaml:"id"`
	Type               string         `yaml:"type"` // github | gitlab | gitea | forgejo
	URL                string         `yaml:"url"`
	APIURL             string         `yaml:"api_url"`
	CAFile             string         `yaml:"ca_file"`
	Writer             string         `yaml:"writer"`
	KnownAuthors       []string       `yaml:"known_authors"`
	AutomationAccounts []string       `yaml:"automation_accounts"`
	Sign               string         `yaml:"sign"` // auto | always
	Limits             ProviderLimits `yaml:"limits"`
}

// ProviderLimits override a provider's pacing; zero, or
// an absent min_interval, keeps the platform's default.
type ProviderLimits struct {
	WritesPerMinute int `yaml:"writes_per_minute"`
	WritesPerHour   int `yaml:"writes_per_hour"`
	// Reads is how many targets are inspected at once, GitReads how many
	// git fetches run at once, ReadsPerMinute the read budget.
	Reads             int `yaml:"reads"`
	GitReads          int `yaml:"git_reads"`
	ReadsPerMinute    int `yaml:"reads_per_minute"`
	CommentsPerMinute int `yaml:"comments_per_minute"`
	// MinInterval is the least time between two writes: a number of
	// milliseconds or seconds such as "250ms" or "1s" (Interval parses it).
	MinInterval string `yaml:"min_interval"`
}

// Interval returns MinInterval as a duration; zero when it is absent or
// does not parse (check rejects such a file).
func (l ProviderLimits) Interval() time.Duration {
	d, err := parseInterval(l.MinInterval)
	if err != nil {
		return 0
	}
	return d
}

// intervalRe is the form of limits.min_interval, as the schema has it.
var intervalRe = regexp.MustCompile(`^[1-9][0-9]{0,5}(ms|s)$`)

// parseInterval parses limits.min_interval ("" is zero).
func parseInterval(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if !intervalRe.MatchString(s) {
		return 0, fmt.Errorf("%q is not a number of milliseconds or seconds such as 250ms or 1s", s)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	return d, nil
}

type PackMeta struct {
	Description string   `yaml:"description"`
	Requires    []string `yaml:"requires"`
	Formerly    []string `yaml:"formerly"`
}

// Targets is targets.yml.
type Targets struct {
	Version  int      `yaml:"version"`
	Defaults Defaults `yaml:"defaults"`
	Targets  []Entry  `yaml:"targets"`
	Exclude  []string `yaml:"exclude"`
	// Repos is the pre-v1 format: a bare list of repository paths
	// without version. It is converted to Targets entries on load.
	Repos []string `yaml:"repos"`

	Legacy bool `yaml:"-"`
}

type Defaults struct {
	Provider string   `yaml:"provider"`
	Packs    []string `yaml:"packs"`
	// OptIn is the opt_in of every entry that sets none: OptInRequired
	// (the default when empty) or OptInAssumed.
	OptIn string `yaml:"opt_in"`
}

// Values of opt_in in targets.yml.
const (
	// OptInRequired: a target the entry selects gets nothing until it has
	// an opt-in file.
	OptInRequired = "required"
	// OptInAssumed: a target the entry selects counts as opted in without
	// an opt-in file, with the packs targets.yml gives it.
	OptInAssumed = "assumed"
)

// Entry is one item of targets.yml `targets:`. Exactly one of Repo, Org and
// Group is set. Org and Group are synonyms (a namespace).
//
// Repo, Org and Group may be web URLs as written in the file; ResolveURLs
// turns them into <provider>:<path>, which is what everything else reads.
type Entry struct {
	Repo      string   `yaml:"repo"`
	Org       string   `yaml:"org"`
	Group     string   `yaml:"group"`
	Provider  string   `yaml:"provider"`
	Topics    []string `yaml:"topics"`
	Subgroups *bool    `yaml:"subgroups"`
	Forks     bool     `yaml:"forks"`
	// Match keeps, of an org or group entry, the repositories whose full
	// path matches one of these glob patterns.
	Match []string `yaml:"match"`
	Packs []string `yaml:"packs"`
	// OptIn is OptInRequired, OptInAssumed, or "" for defaults.opt_in.
	OptIn string `yaml:"opt_in"`
}

// OptIn is the target's opt-in file. Its presence is the consent; it may be
// empty.
type OptIn struct {
	Version int      `yaml:"version"`
	Packs   []string `yaml:"packs"`
	Ignore  []string `yaml:"ignore"`
	// Enabled false opts the repository out, whatever targets.yml says;
	// absent means true.
	Enabled *bool `yaml:"enabled"`

	Legacy bool `yaml:"-"` // no version key
}

// Disabled reports whether the opt-in file says enabled: false. A nil
// opt-in is not disabled.
func (o *OptIn) Disabled() bool { return o != nil && o.Enabled != nil && !*o.Enabled }

// ParseHub decodes and validates hub.yml. data == nil means the file is
// absent (legacy hub).
//
// A key that is present is validated even when empty; a key that is absent
// takes its default: branch touchmark/<id>, the commit message and PR title
// "chore: sync engineering assets", the label engineering-assets, link_hub
// auto, max_new_prs_per_run 100, max_close_fraction 0.1, auto_close_cooldown
// 30d, write_isolation platform, private_targets_in_public_hub skip, and per
// provider sign auto and the public URL of github and gitlab; the hosts of
// previous_fingerprints are canonical (CanonicalFingerprint). The
// single-provider shorthand with platform (platform, base_url, writer, sign)
// is folded into Providers. Without platform, writer and sign stay at the
// top level and Providers stays empty: the provider's type and URL come from
// the CI environment or the hub's origin remote, and ResolveProviders gives
// it the top-level writer and sign. A base_url needs
// platform. The "change-me" placeholder id
// parses; Check rejects it. Strings that look like tokens or private keys
// anywhere in the file are errors, found before the file is decoded; when
// there is one, those are the only errors, since any other message could
// quote the secret.
//
// Messages of all three parsers are safe to print: control characters are
// escaped and anything that looks like a credential is cut to its prefix.
func ParseHub(data []byte) (*Hub, []Warning, error) {
	h, warns, err := parseHub(data)
	return h, cleanWarnings(warns), cleanErr(err)
}

// ParseTargets decodes and validates targets.yml. data == nil means absent:
// no targets, no defaults.
//
// A value of repo, org, group or exclude may be a web URL: its form is
// checked here, and ResolveURLs, which knows the providers, rewrites it as
// <provider>:<path>. Exclude entries are patterns that may hold the glob
// characters * and ? (glob.go), as are the patterns of an org or group
// entry's match; opt_in is required or assumed.
//
// The pre-v1 format, a bare `repos:` list without version, is converted
// to Targets entries with Legacy set and a warning.
func ParseTargets(data []byte) (*Targets, []Warning, error) {
	t, warns, err := parseTargets(data)
	return t, cleanWarnings(warns), cleanErr(err)
}

// ParseOptIn decodes and validates an opt-in file. An empty file is valid.
//
// Version is 1 after parsing; Legacy records that the file had no version
// key (the pre-v1 format, or an empty file). enabled: false opts the
// repository out (Disabled).
func ParseOptIn(data []byte) (*OptIn, []Warning, error) {
	o, warns, err := parseOptIn(data)
	return o, cleanWarnings(warns), cleanErr(err)
}

// Ref names one target repository: an optional provider id and a path such as
// "acme/billing" or "group/sub/project".
type Ref struct {
	Provider string
	Path     string
}

// ParseRef parses "path" or "provider:path".
//
// The path has at least two segments of letters, digits, '.', '-' and '_',
// none of them "." or "..", and no leading or trailing slash. Its case is
// kept; Select compares paths case-insensitively.
func ParseRef(s string) (Ref, error) { return parseRef(s, 2) }

func (r Ref) String() string {
	if r.Provider == "" {
		return r.Path
	}
	return r.Provider + ":" + r.Path
}

// Selection is the resolved pack list for one target.
type Selection struct {
	// Packs in layering order, requires expanded, duplicates removed.
	Packs []string
	// Complete is false when some targets.yml entries could not be evaluated
	// for this target without the platform API (org/group selectors in local
	// mode). Apply must refuse to run on an incomplete selection unless the
	// caller passed an explicit pack list.
	Complete bool
	// Unresolved lists the selectors that could not be evaluated.
	Unresolved []string
	// Sources explains where each pack came from: "defaults", "targets.yml",
	// the opt-in file name, "requires <pack>", or "--packs".
	Sources map[string][]string
}

// Select resolves the packs for target in local mode:
//  1. targets.Defaults.Packs;
//  2. Packs of every `repo:` entry that names target, in file order;
//  3. optIn.Packs.
//
// Duplicates keep their first position. Then every pack's requires (from
// hub.Packs, transitively) is inserted before it unless already earlier.
// Entries with org/group cannot be evaluated locally: they make the
// selection incomplete and are listed in Unresolved. An excluded target
// yields an empty, complete selection with a warning. Unknown packs and
// requires cycles are errors (known lists the packs the hub ships now).
//
// Only org/group entries that carry packs and whose namespace and match
// patterns could contain target count as unresolved; the others cannot
// change the result. A repo entry names target when the paths are equal
// ignoring case and the providers are equal; an exclude entry covers it
// when its pattern matches the path (an entry without glob characters is
// an equal path) and the providers are equal. Each side's provider is its
// own (entry provider, then ref prefix), else targets.Defaults.Provider,
// else the hub's only provider; when either side stays unknown, the path
// alone decides. targets must hold no URL: ResolveURLs rewrites them first.
// A pack's former name (formerly) selects the pack, with a warning, when
// that pack exists; a former name of a pack that is gone is unknown.
func Select(hub *Hub, targets *Targets, optIn *OptIn, target Ref, known map[string]bool) (Selection, []Warning, error) {
	return selectPacks(hub, targets, optIn, target, known)
}

// Explicit builds a complete selection from an explicit list (--packs),
// still expanding requires. A former pack name in the list is an error that
// names the current one; one in hub.yml's requires resolves silently (check
// warns about it).
func Explicit(hub *Hub, packs []string, known map[string]bool) (Selection, error) {
	return explicitPacks(hub, packs, known)
}

// Aliases returns pack name → former names from hub.Packs[*].Formerly.
func (h *Hub) Aliases() map[string][]string { return h.aliases() }

// KnownAliases is Aliases restricted to the renames Select honours: former
// names of packs in known that are not themselves current packs (in known
// or hub.Packs) and that no other pack claims. This is what decides whose
// history counts as a selected pack's; a name Check rejects never pulls in
// the history of a pack that still exists. Former names are sorted.
func (h *Hub) KnownAliases(known map[string]bool) map[string][]string { return h.knownAliases(known) }

// OptInName returns hub.OptInFile or the default.
func (h *Hub) OptInName() string {
	if h == nil || h.OptInFile == "" {
		return DefaultOptIn
	}
	return h.OptInFile
}

// Check performs the cross-file checks of `touchmark check` that need more
// than one file: packs referenced by defaults, entries and requires exist in
// known; requires has no cycles; formerly names do not collide with current
// packs or each other; provider references resolve; exclude entries parse.
//
// It also rejects the template's placeholder id, and requires every entry
// to name a provider (itself or through defaults.provider) when the hub has
// more than one. targets must hold no URL: ResolveURLs rewrites them first,
// and one left is an error. Former pack names in references, metadata for
// packs that do not exist, targets both listed and excluded (by name or by
// pattern), a match pattern no repository of its namespace can match, and
// a repo entry's opt_in: required that an org or group entry with opt_in:
// assumed overrides are warnings. An exclude pattern that covers no listed
// repository is fine: it is there for org and group entries.
func Check(hub *Hub, targets *Targets, known map[string]bool) ([]Warning, []error) {
	return check(hub, targets, known)
}
