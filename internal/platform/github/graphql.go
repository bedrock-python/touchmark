package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// GraphQL requests are fixed documents: data goes in variables only, never
// into the query text. The batched ones repeat one fixed field per alias
// (r0, r1, …), each with variables of its own ($o0, $n0, …).
//
// Every field below is in the schemas of github.com and GHES 3.19
// (docs.github.com/public/fpt/schema.docs.graphql and
// …/ghes-3.19/schema.docs-enterprise.graphql, 2026-09-29).

// gqlActorFields names an actor: a Bot's login comes without the "[bot]"
// suffix REST shows, and its databaseId is the REST user id (checked on
// github.com).
const gqlActorFields = `__typename login ... on User { databaseId } ... on Bot { databaseId } ... on Mannequin { databaseId }`

// gqlPRFields are the fields of a pull request the driver reads.
const gqlPRFields = `id databaseId number url state isDraft title body createdAt closedAt mergedAt
headRefName headRefOid baseRefName baseRef { name }
headRepository { databaseId }
repository { databaseId nameWithOwner }
author { ` + gqlActorFields + ` }
labels(first: 20) { nodes { name } }`

// queryOpenPRs lists the open pull requests of a repository from a head
// branch, from any repository: forks included (checked on github.com).
const queryOpenPRs = `query($owner: String!, $name: String!, $head: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(headRefName: $head, states: [OPEN], first: 50, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { ` + gqlPRFields + ` }
    }
  }
}`

// queryClosers names who closed (the actor of the last ClosedEvent) and
// who merged pull requests, by node id, up to 100 per request.
const queryClosers = `query($ids: [ID!]!) {
  nodes(ids: $ids) {
    ... on PullRequest {
      number repository { databaseId }
      mergedBy { ` + gqlActorFields + ` }
      timelineItems(itemTypes: [CLOSED_EVENT], last: 1) {
        nodes { ... on ClosedEvent { createdAt actor { ` + gqlActorFields + ` } } }
      }
    }
  }
}`

// querySweep lists the open pull requests from a head branch in
// repositories by node id, up to 50 repositories per request.
const querySweep = `query($ids: [ID!]!, $head: String!) {
  nodes(ids: $ids) {
    ... on Repository {
      databaseId
      pullRequests(headRefName: $head, states: [OPEN], first: 20) {
        pageInfo { hasNextPage endCursor }
        nodes { ` + gqlPRFields + ` }
      }
    }
  }
}`

// fileFields read one path at the default branch head of a repository:
// the entry's mode, id and size, and a text blob's content.
const fileFields = `databaseId
object(expression: "HEAD") { ... on Commit { oid file(path: $path) { mode type oid size object { ... on Blob { byteSize isBinary isTruncated oid text } } } } }`

// mutationUpdateRefs moves, creates and deletes refs atomically
// (UpdateRefsInput: repositoryId, refUpdates [{name, beforeOid, afterOid,
// force}]).
const mutationUpdateRefs = `mutation($input: UpdateRefsInput!) { updateRefs(input: $input) { clientMutationId } }`

// batchFilesQuery returns the query reading $path in n repositories,
// aliased r0 … r<n-1>, with variables $o<i> and $n<i>.
func batchFilesQuery(n int) string {
	var b strings.Builder
	b.WriteString("query($path: String!")
	for i := range n {
		fmt.Fprintf(&b, ", $o%d: String!, $n%d: String!", i, i)
	}
	b.WriteString(") {\n")
	for i := range n {
		fmt.Fprintf(&b, "  r%d: repository(owner: $o%d, name: $n%d) { %s }\n", i, i, i, fileFields)
	}
	b.WriteString("}")
	return b.String()
}

// gqlRequest is the body of a GraphQL request.
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// gqlResponse is a GraphQL answer: data and errors, both possibly present.
type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// gqlError is one error of a GraphQL answer. type is GitHub's
// (NOT_FOUND, FORBIDDEN, RATE_LIMITED, …); path names the field it is
// about, empty for the whole request.
type gqlError struct {
	Type    string `json:"type"`
	Path    []any  `json:"path"`
	Message string `json:"message"`
}

// at reports whether e is about the field at path (a prefix match of
// names).
func (e gqlError) at(path ...string) bool {
	if len(e.Path) < len(path) {
		return false
	}
	for i, p := range path {
		if s, ok := e.Path[i].(string); !ok || s != p {
			return false
		}
	}
	return true
}

// gqlErrors are the field errors of an answer.
type gqlErrors []gqlError

// at returns the first error about path, nil for none.
func (es gqlErrors) at(path ...string) *gqlError {
	for i := range es {
		if es[i].at(path...) {
			return &es[i]
		}
	}
	return nil
}

// graphql sends one GraphQL request of op with auth and decodes data into
// out. It returns the errors about single fields (with a path) for the
// caller to judge; an error of the whole request is returned as a
// classified error (GitHub answers a primary rate limit with HTTP 200 and
// an error of type RATE_LIMITED, a secondary one with 200 or 403 and a
// message about it):
//   - RATE_LIMITED, or a message about a secondary rate limit, anywhere:
//     ClassRateLimited, waiting for the headers' reset;
//   - an error without a path, or with data null: NOT_FOUND ClassNotFound,
//     FORBIDDEN ClassPermission, a timeout or INTERNAL ClassTransient, else
//     ClassInvalid (a query the schema refuses: MAX_NODE_LIMIT_EXCEEDED,
//     parse errors).
func (c *client) graphql(ctx context.Context, op string, a *httpx.Auth, query string, vars map[string]any, out any) (gqlErrors, error) {
	if a == nil {
		return nil, &platform.Error{Op: op, Class: platform.ClassUnsupported, Err: errors.New("GraphQL needs a credential")}
	}
	if !isMutation(query) {
		// A query is a read, though it is a POST: only mutations spend the
		// write budget.
		ctx = throttle.AsRead(ctx)
	}
	var resp gqlResponse
	hr, err := c.http.JSON(ctx, http.MethodPost, c.graphqlURL, a, gqlRequest{Query: query, Variables: vars}, &resp)
	if err != nil {
		return nil, c.apiError(op, err)
	}
	for _, e := range resp.Errors {
		if e.Type == "RATE_LIMITED" || limitMessage(strings.ToLower(e.Message)) {
			return nil, &platform.Error{Op: op, Class: platform.ClassRateLimited, Status: hr.Status,
				RetryAfter: retryAfter(hr.Header, c.now()), Err: errors.New(c.mask("GraphQL: " + oneLine(e.Message)))}
		}
	}
	null := len(resp.Data) == 0 || string(resp.Data) == "null"
	var field gqlErrors
	for _, e := range resp.Errors {
		if len(e.Path) > 0 && !null {
			field = append(field, e)
			continue
		}
		return nil, &platform.Error{Op: op, Class: gqlClass(e), Status: hr.Status,
			Err: errors.New(c.mask("GraphQL: " + oneLine(e.Type+" "+e.Message)))}
	}
	if null {
		return nil, shapeError(op, "GraphQL answered without data")
	}
	if out != nil {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return nil, shapeError(op, "GraphQL data does not decode: %v", err)
		}
	}
	return field, nil
}

// isMutation reports whether query is a GraphQL mutation (it writes), not a
// query: its first word, after comments and blanks, is "mutation".
func isMutation(query string) bool {
	for {
		query = strings.TrimLeft(query, " \t\r\n,")
		if !strings.HasPrefix(query, "#") {
			break
		}
		_, query, _ = strings.Cut(query, "\n")
	}
	word := strings.FieldsFunc(query, func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') })
	return len(word) > 0 && word[0] == "mutation"
}

// gqlClass classifies an error of a whole GraphQL request, or of a field.
func gqlClass(e gqlError) platform.Class {
	lower := strings.ToLower(e.Message)
	switch {
	case e.Type == "NOT_FOUND":
		return platform.ClassNotFound
	case e.Type == "FORBIDDEN":
		return platform.ClassPermission
	case e.Type == "RATE_LIMITED", limitMessage(lower):
		return platform.ClassRateLimited
	case e.Type == "INTERNAL", e.Type == "SERVICE_UNAVAILABLE", strings.Contains(lower, "timeout"),
		strings.Contains(lower, "something went wrong"):
		return platform.ClassTransient
	}
	if _, class, ok := ruleOf(lower); ok {
		return class
	}
	return platform.ClassInvalid
}

// fieldError is the error of op for a GraphQL field error: its class, the
// rule its message names, and ErrNotFound under NOT_FOUND.
func (c *client) fieldError(op string, e *gqlError) error {
	msg := c.mask("GraphQL: " + oneLine(e.Message))
	pe := &platform.Error{Op: op, Class: gqlClass(*e), Err: errors.New(msg)}
	if rule, class, ok := ruleOf(strings.ToLower(e.Message)); ok {
		pe.Class, pe.Rule = class, rule
	}
	if pe.Class == platform.ClassNotFound {
		pe.Err = fmt.Errorf("%s: %w", msg, platform.ErrNotFound)
	}
	return pe
}

// gqlActor is an actor of GraphQL.
type gqlActor struct {
	Typename   string `json:"__typename"`
	Login      string `json:"login"`
	DatabaseID int64  `json:"databaseId"`
}

// account converts an actor: a Bot's login gets the "[bot]" suffix REST
// uses; types other than User and Bot are unknown.
func (a *gqlActor) account() platform.Account {
	if a == nil {
		return platform.Account{Kind: platform.KindUnknown}
	}
	acc := platform.Account{Login: a.Login, Kind: platform.KindUnknown}
	if a.DatabaseID > 0 {
		acc.ID = fmt.Sprint(a.DatabaseID)
	}
	switch a.Typename {
	case "User":
		acc.Kind = platform.KindUser
	case "Bot":
		acc.Kind = platform.KindBot
		if a.Login != "" && !strings.HasSuffix(a.Login, "[bot]") {
			acc.Login = a.Login + "[bot]"
		}
	}
	return acc
}
