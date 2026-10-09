package azuredevops

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Capabilities of Azure DevOps Services.
const (
	// maxBody bounds a description: 4 000 characters (Pull Requests -
	// Update: "Description (up to 4000 characters)"). The core counts
	// bytes, never fewer than characters, and the marker lives apart
	// (MarkerInProperties), so 4 000 bytes of description always fit.
	maxBody = 4000
	// markerProperty is the pull request property that holds the marker
	// line.
	markerProperty = "touchmark.marker"
)

// Checks the writer cannot make upfront: branch policies (a required
// reviewer, a build) refuse a direct push to the branch they protect
// (TF402455), and so do the repository's push policies; the writer may
// read neither before it pushes.
const (
	branchPolicies = "branch-policies"
	pushPolicies   = "push-policies"
)

// limits are the default pacing. Azure DevOps meters TSTUs, not requests:
// 200 within any sliding five minutes before it delays a user, which the
// X-RateLimit-* headers announce before it happens (internal/throttle
// pauses on them). Four targets and two git fetches at once, 120 reads and
// 30 writes a minute and half a second between two writes keep an
// ordinary run far from it; providers[].limits in hub.yml can raise them.
var limits = platform.Limits{Reads: 4, GitReads: 2, ReadsPerMinute: 120, WritesPerMinute: 30, MinInterval: 500 * time.Millisecond}

// guidRe matches an identity id.
var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// publicAccess is the identity connectionData names for an anonymous
// request (seen anonymously, 2026-10-09).
const publicAccess = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

// apiProperty is one value of a property bag: {"$type": …, "$value": …}.
type apiProperty struct {
	Value any `json:"$value"`
}

// text returns the property's value as a string, "" when it is no string.
func (p apiProperty) text() string {
	s, _ := p.Value.(string)
	return s
}

// apiIdentity is an identity of connectionData and of the Identities API.
type apiIdentity struct {
	ID                  string                 `json:"id"`
	Descriptor          string                 `json:"descriptor"`
	SubjectDescriptor   string                 `json:"subjectDescriptor"`
	ProviderDisplayName string                 `json:"providerDisplayName"`
	IsContainer         bool                   `json:"isContainer"`
	Properties          map[string]apiProperty `json:"properties"`
}

// apiIdentityRef is an identity as pull requests name it (IdentityRef).
type apiIdentityRef struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
	Descriptor  string `json:"descriptor"`
	IsContainer bool   `json:"isContainer"`
}

// connectionData is GET {org}/_apis/connectionData: the identity the
// credential acts as. It is not in the REST reference; Azure DevOps' own
// clients read it, and its shape is the one seen anonymously (2026-10-09).
type connectionData struct {
	AuthenticatedUser *apiIdentity `json:"authenticatedUser"`
}

// Probe reports the capabilities of Azure DevOps Services, which has one
// version: it asks the API nothing.
func (d *reader) Probe(context.Context) (platform.Caps, error) {
	return platform.Caps{
		Flavor:  "azure-devops",
		MaxBody: maxBody,
		Draft:   platform.DraftNative,
		// closedBy names who abandoned a pull request (read alone).
		CloserKnown: true,
		// An abandoned pull request is never changed again by touchmark:
		// whether its properties can still be written is not known, and
		// memory without writes needs none (platform.Caps.ClosedImmutable).
		ClosedImmutable: true,
		// Descriptions hold 4 000 characters: the marker lives in a
		// property.
		Marker:      platform.MarkerInProperties,
		RuntimeOnly: []string{branchPolicies, pushPolicies},
		Limits:      limits,
	}, nil
}

// Self is the identity the credential acts as: connectionData's
// authenticatedUser. Its email is the identity's Account property when it
// is an address (a user's sign-in name), for the commits of a writer.
func (d *reader) Self(ctx context.Context) (platform.Account, error) {
	return d.c.selfAccount(ctx)
}

// selfAccount returns the credential's identity, looked up once.
func (c *client) selfAccount(ctx context.Context) (platform.Account, error) {
	const op = "get account"
	c.mu.Lock()
	if c.self != nil {
		a := *c.self
		c.mu.Unlock()
		return a, nil
	}
	c.mu.Unlock()
	if c.token == "" {
		return platform.Account{}, &platform.Error{Op: op, Class: platform.ClassAuth, Err: errors.New("no credential: the reader is anonymous")}
	}
	var cd connectionData
	if _, err := c.get(ctx, op, c.apis("connectionData"), nil, &cd); err != nil {
		return platform.Account{}, err
	}
	u := cd.AuthenticatedUser
	switch {
	case u == nil || !guidRe.MatchString(u.ID):
		return platform.Account{}, shapeError(op, "connectionData names no authenticated identity")
	case strings.EqualFold(u.ID, publicAccess) || strings.HasPrefix(u.Descriptor, "System:PublicAccess"):
		return platform.Account{}, &platform.Error{Op: op, Class: platform.ClassAuth,
			Err: errors.New("the token is not accepted: Azure DevOps took the request for an anonymous one")}
	}
	a := identityAccount(u)
	if email := u.Properties["Account"].text(); validEmail(email) {
		a.Email = email
	}
	c.mu.Lock()
	c.self = &a
	c.mu.Unlock()
	return a, nil
}

// validEmail reports whether s can be a commit author's address: one "@"
// between non-empty parts, and nothing git would refuse or misread.
func validEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@") &&
		!strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == '<' || r == '>' || r == 0x7f })
}

// Lookup resolves a login, an identity id (a GUID), to its account: GET
// https://vssps.dev.azure.com/{org}/_apis/identities?identityIds={id}.
// Azure DevOps names identities by display names that are neither unique
// nor stable, so anything else is ClassInvalid; an id the organization does
// not know is ErrNotFound.
func (d *reader) Lookup(ctx context.Context, login string) (platform.Account, error) {
	const op = "look up account"
	if !guidRe.MatchString(login) {
		return platform.Account{}, invalid(op, "%q: Azure DevOps logins are identity ids (GUIDs), such as 3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d, "+
			"as connectionData shows them (a display name or an email names no identity reliably)", login)
	}
	var found list[*apiIdentity]
	q := url.Values{"identityIds": {login}, "queryMembership": {"None"}}
	if _, err := d.c.get(ctx, op, endpoint(d.c.vssps, "_apis", "identities"), q, &found); err != nil {
		return platform.Account{}, err
	}
	for _, id := range found.Value {
		if id != nil && strings.EqualFold(id.ID, login) {
			return identityAccount(id), nil
		}
	}
	return platform.Account{}, notFound(op, "identity %s", login)
}

// identityAccount converts an identity: its id, lowercased, is both its id
// and its login.
func identityAccount(u *apiIdentity) platform.Account {
	return platform.Account{
		ID:    strings.ToLower(u.ID),
		Login: strings.ToLower(u.ID),
		Kind:  kindOf(u.SubjectDescriptor, u.Descriptor, u.IsContainer),
	}
}

// refAccount converts an identity reference of a pull request; the zero
// Account when it names none.
func refAccount(r *apiIdentityRef) platform.Account {
	if r == nil || !guidRe.MatchString(r.ID) {
		return platform.Account{}
	}
	id := strings.ToLower(r.ID)
	return platform.Account{ID: id, Login: id, Kind: kindOf(r.Descriptor, "", r.IsContainer)}
}

// kindOf tells people from automation by an identity's descriptors: a
// subject descriptor "svc." is a service identity (a project's build
// service, an app), a bot; "aadsp." a service principal of Microsoft Entra
// ID; "aad." and "msa." users; the identity descriptor's type tells the same
// for the Identities API (Microsoft.TeamFoundation.ServiceIdentity,
// Microsoft.VisualStudio.Services.Claims.AadServicePrincipal). A group, or
// anything else, is unknown.
func kindOf(subject, descriptor string, container bool) platform.AccountKind {
	switch {
	case container:
		return platform.KindUnknown
	case strings.HasPrefix(subject, "svc."), strings.HasPrefix(descriptor, "Microsoft.TeamFoundation.ServiceIdentity;"):
		return platform.KindBot
	case strings.HasPrefix(subject, "aadsp."), strings.HasPrefix(descriptor, "Microsoft.VisualStudio.Services.Claims.AadServicePrincipal;"):
		return platform.KindServiceAccount
	case strings.HasPrefix(subject, "aad."), strings.HasPrefix(subject, "msa."),
		strings.HasPrefix(descriptor, "Microsoft.IdentityModel.Claims.ClaimsIdentity;"):
		return platform.KindUser
	}
	return platform.KindUnknown
}
