package ghfake

import (
	"fmt"
	"slices"
	"strings"
)

// pendingPush is a push to the branch of a closed pull request: a
// violation unless a new pull request from that branch follows.
type pendingPush struct {
	repoID int64
	branch string
	text   string
}

// Violations returns what touchmark must never do and the fake saw since
// the last call; tests assert it is empty. Only writes through HTTP count
// (pushes, REST and GraphQL), by identities not marked Human; the setup
// methods are people. Each entry starts with its kind:
//   - "head-to-base <repo>#<n>: …" a write left an open pull request's head
//     equal to or behind its base (GitHub then closes it);
//   - "deleted-open-branch <repo>#<n>: …" a write deleted the head branch
//     of an open pull request (GitHub closes it);
//   - "foreign-branch <repo>#<n>: …" a write moved or deleted the head
//     branch of an open pull request by another author (SetKnownAuthors
//     names the authors a writer owns);
//   - "closed-branch-push <repo>#<n>: …" a write moved the branch of a
//     closed or merged pull request and no new pull request from that
//     branch followed before this call;
//   - "default-branch <repo>: …" a write moved or deleted the default
//     branch;
//   - "token-scope <identity>: …" an installation token minted for some
//     repositories was used on another one;
//   - "stale-token <identity>: …" a write with an expired, revoked or
//     suspended token.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.violations)
	for _, pp := range s.pending {
		out = append(out, pp.text)
	}
	s.violations, s.pending = nil, nil
	return out
}

// violate records a violation. Called with mu held.
func (s *Server) violate(format string, args ...any) {
	s.violations = append(s.violations, fmt.Sprintf(format, args...))
}

// addPending records a push to the branch of a closed pull request, once
// per branch. Called with mu held.
func (s *Server) addPending(repoID int64, branch, text string) {
	for i, pp := range s.pending {
		if pp.repoID == repoID && pp.branch == branch {
			s.pending[i].text = text
			return
		}
	}
	s.pending = append(s.pending, pendingPush{repoID: repoID, branch: branch, text: text})
}

// opened clears the pending pushes a pull request from branch of r
// justifies. Called with mu held.
func (s *Server) opened(r *repo, branch string) {
	if r == nil {
		return
	}
	s.pending = slices.DeleteFunc(s.pending, func(pp pendingPush) bool {
		return pp.repoID == r.id && pp.branch == branch
	})
}

// SetKnownAuthors declares whose pull requests count as writer's own
// (known_authors in hub.yml): moving or deleting the branch of such
// an open pull request is no violation. It replaces the previous list.
func (s *Server) SetKnownAuthors(writer string, authors ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.byLogin[strings.ToLower(writer)]
	if w == nil {
		return setupErr("SetKnownAuthors: unknown account %q", writer)
	}
	ids := map[int64]bool{}
	for _, login := range authors {
		a := s.byLogin[strings.ToLower(login)]
		if a == nil {
			return setupErr("SetKnownAuthors: unknown account %q", login)
		}
		ids[a.id] = true
	}
	s.known[w.id] = ids
	return nil
}

// Human marks an account as a person: its writes through HTTP (with a
// personal access token) are not judged by Violations.
func (s *Server) Human(login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(login)]
	if a == nil {
		return setupErr("Human: unknown account %q", login)
	}
	a.human = true
	return nil
}
