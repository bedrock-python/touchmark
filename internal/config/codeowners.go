package config

import (
	"bytes"
	"strings"
)

// PlaceholderOwner is the owner the hub template's CODEOWNERS files name
// until a hub replaces it with its own team (the template's README, "Set up
// your hub").
const PlaceholderOwner = "@acme/hub-maintainers"

// CodeownersFiles are where GitHub, GitLab, Gitea and Forgejo look for a
// repository's CODEOWNERS file.
var CodeownersFiles = []string{"CODEOWNERS", ".github/CODEOWNERS", ".gitlab/CODEOWNERS", ".gitea/CODEOWNERS", "docs/CODEOWNERS"}

// CheckCodeowners warns about each CODEOWNERS file of a hub that still
// names PlaceholderOwner on a rule line. The platforms treat an owner they
// do not know as no owner, so with the placeholder "require review from
// code owners" requires nobody for packs/, hub.yml and the workflows, and
// says nothing. files maps the paths of CodeownersFiles to their content.
// The template itself, whose id is still PlaceholderID, is not warned
// about: check fails on the id already.
func CheckCodeowners(h *Hub, files map[string][]byte) []Warning {
	if h == nil || h.Legacy || h.ID == PlaceholderID {
		return nil
	}
	var out []Warning
	for _, name := range CodeownersFiles {
		data, ok := files[name]
		if !ok {
			continue
		}
		for line := range bytes.Lines(data) {
			s := strings.TrimSpace(string(line))
			if s == "" || strings.HasPrefix(s, "#") {
				continue
			}
			if rule, _, _ := strings.Cut(s, " #"); containsOwner(rule, PlaceholderOwner) {
				out = append(out, Warning{File: name, Message: "names " + PlaceholderOwner + ", the template's placeholder: " +
					"the platform knows no such owner, so code owner review requires nobody; replace it with your team"})
				break
			}
		}
	}
	return out
}

// containsOwner reports whether owner is one of the fields of rule,
// ignoring case as the platforms do.
func containsOwner(rule, owner string) bool {
	for _, f := range strings.Fields(rule) {
		if strings.EqualFold(f, owner) {
			return true
		}
	}
	return false
}
