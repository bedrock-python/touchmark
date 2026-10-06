package ghfake

import (
	"fmt"
	"strings"
	"sync"
)

// schemaSDL is the part of GitHub's GraphQL schema the fake serves: every
// type, field, argument and enum value below is copied from
// https://docs.github.com/public/fpt/schema.docs.graphql (downloaded
// 2026-09-29), with only the members the fake resolves. A query that
// selects anything else fails validation the way GitHub reports an
// unknown field ("undefinedField"), so a driver cannot come to depend on
// something the fake would only pretend to know. TestSchemaSubset checks
// this text against a downloaded schema when GHFAKE_SCHEMA names one.
const schemaSDL = `
scalar DateTime
scalar GitObjectID
scalar GitRefname
scalar URI

type Query {
  node(id: ID!): Node
  nodes(ids: [ID!]!): [Node]!
  organization(login: String!): Organization
  rateLimit(dryRun: Boolean = false): RateLimit
  repository(followRenames: Boolean = true, name: String!, owner: String!): Repository
  user(login: String!): User
  viewer: User!
}

type Mutation {
  updateRefs(input: UpdateRefsInput!): UpdateRefsPayload
}

interface Node {
  id: ID!
}

interface Actor {
  avatarUrl(size: Int): URI!
  login: String!
  resourcePath: URI!
  url: URI!
}

interface RepositoryOwner {
  avatarUrl(size: Int): URI!
  id: ID!
  login: String!
  repository(followRenames: Boolean = true, name: String!): Repository
  resourcePath: URI!
  url: URI!
}

interface GitObject {
  abbreviatedOid: String!
  commitResourcePath: URI!
  commitUrl: URI!
  id: ID!
  oid: GitObjectID!
  repository: Repository!
}

type User implements Actor & Node & RepositoryOwner {
  avatarUrl(size: Int): URI!
  databaseId: Int
  email: String!
  id: ID!
  login: String!
  name: String
  repository(followRenames: Boolean = true, name: String!): Repository
  resourcePath: URI!
  url: URI!
}

type Mannequin implements Actor & Node {
  avatarUrl(size: Int): URI!
  databaseId: Int
  email: String
  id: ID!
  login: String!
  resourcePath: URI!
  url: URI!
}

type Bot implements Actor & Node {
  avatarUrl(size: Int): URI!
  databaseId: Int
  id: ID!
  login: String!
  resourcePath: URI!
  url: URI!
}

type Organization implements Actor & Node & RepositoryOwner {
  avatarUrl(size: Int): URI!
  databaseId: Int
  id: ID!
  login: String!
  name: String
  repository(followRenames: Boolean = true, name: String!): Repository
  resourcePath: URI!
  url: URI!
}

type Repository implements Node {
  databaseId: Int
  defaultBranchRef: Ref
  hasPullRequestsEnabled: Boolean!
  id: ID!
  isArchived: Boolean!
  isDisabled: Boolean!
  isEmpty: Boolean!
  isFork: Boolean!
  isLocked: Boolean!
  isMirror: Boolean!
  isPrivate: Boolean!
  isTemplate: Boolean!
  name: String!
  nameWithOwner: String!
  object(expression: String, oid: GitObjectID): GitObject
  owner: RepositoryOwner!
  parent: Repository
  pullRequests(after: String, baseRefName: String, before: String, first: Int, headRefName: String, labels: [String!], last: Int, orderBy: IssueOrder, states: [PullRequestState!]): PullRequestConnection!
  ref(qualifiedName: String!): Ref
  repositoryTopics(after: String, before: String, first: Int, last: Int): RepositoryTopicConnection!
  url: URI!
  viewerPermission: RepositoryPermission
  visibility: RepositoryVisibility!
}

type Ref implements Node {
  id: ID!
  name: String!
  prefix: String!
  target: GitObject
}

type Commit implements GitObject & Node {
  abbreviatedOid: String!
  commitResourcePath: URI!
  commitUrl: URI!
  committedDate: DateTime!
  file(path: String!): TreeEntry
  id: ID!
  message: String!
  oid: GitObjectID!
  repository: Repository!
  tree: Tree!
}

type Tree implements GitObject & Node {
  abbreviatedOid: String!
  commitResourcePath: URI!
  commitUrl: URI!
  entries: [TreeEntry!]
  id: ID!
  oid: GitObjectID!
  repository: Repository!
}

type Blob implements GitObject & Node {
  abbreviatedOid: String!
  byteSize: Int!
  commitResourcePath: URI!
  commitUrl: URI!
  id: ID!
  isBinary: Boolean
  isTruncated: Boolean!
  oid: GitObjectID!
  repository: Repository!
  text: String
}

type TreeEntry {
  mode: Int!
  name: String!
  object: GitObject
  oid: GitObjectID!
  path: String
  repository: Repository!
  size: Int!
  type: String!
}

type PullRequest implements Node {
  author: Actor
  baseRef: Ref
  baseRefName: String!
  baseRefOid: GitObjectID!
  baseRepository: Repository
  body: String!
  closed: Boolean!
  closedAt: DateTime
  createdAt: DateTime!
  databaseId: Int
  headRefName: String!
  headRefOid: GitObjectID!
  headRepository: Repository
  headRepositoryOwner: RepositoryOwner
  id: ID!
  isCrossRepository: Boolean!
  isDraft: Boolean!
  labels(after: String, before: String, first: Int, last: Int): LabelConnection
  merged: Boolean!
  mergedAt: DateTime
  mergedBy: Actor
  number: Int!
  repository: Repository!
  state: PullRequestState!
  timelineItems(after: String, before: String, first: Int, itemTypes: [PullRequestTimelineItemsItemType!], last: Int, since: DateTime, skip: Int): PullRequestTimelineItemsConnection!
  title: String!
  updatedAt: DateTime!
  url: URI!
}

type PageInfo {
  endCursor: String
  hasNextPage: Boolean!
  hasPreviousPage: Boolean!
  startCursor: String
}

type PullRequestConnection {
  edges: [PullRequestEdge]
  nodes: [PullRequest]
  pageInfo: PageInfo!
  totalCount: Int!
}

type PullRequestEdge {
  cursor: String!
  node: PullRequest
}

type LabelConnection {
  nodes: [Label]
  pageInfo: PageInfo!
  totalCount: Int!
}

type Label implements Node {
  color: String!
  description: String
  id: ID!
  name: String!
}

type RepositoryTopicConnection {
  nodes: [RepositoryTopic]
  pageInfo: PageInfo!
  totalCount: Int!
}

type RepositoryTopic implements Node {
  id: ID!
  topic: Topic!
}

type Topic implements Node {
  id: ID!
  name: String!
}

type PullRequestTimelineItemsConnection {
  filteredCount: Int!
  nodes: [PullRequestTimelineItems]
  pageCount: Int!
  pageInfo: PageInfo!
  totalCount: Int!
}

union PullRequestTimelineItems = BaseRefChangedEvent | ClosedEvent | HeadRefDeletedEvent | HeadRefForcePushedEvent | MergedEvent | ReopenedEvent

type ClosedEvent implements Node {
  actor: Actor
  createdAt: DateTime!
  id: ID!
  stateReason: IssueStateReason
}

type ReopenedEvent implements Node {
  actor: Actor
  createdAt: DateTime!
  id: ID!
  stateReason: IssueStateReason
}

type MergedEvent implements Node {
  actor: Actor
  createdAt: DateTime!
  id: ID!
}

type HeadRefDeletedEvent implements Node {
  actor: Actor
  createdAt: DateTime!
  headRefName: String!
  id: ID!
}

type HeadRefForcePushedEvent implements Node {
  actor: Actor
  createdAt: DateTime!
  id: ID!
}

type BaseRefChangedEvent implements Node {
  actor: Actor
  createdAt: DateTime!
  currentRefName: String!
  id: ID!
  previousRefName: String!
}

type RateLimit {
  cost: Int!
  limit: Int!
  nodeCount: Int!
  remaining: Int!
  resetAt: DateTime!
  used: Int!
}

type UpdateRefsPayload {
  clientMutationId: String
}

input UpdateRefsInput {
  clientMutationId: String
  refUpdates: [RefUpdate!]!
  repositoryId: ID!
}

input RefUpdate {
  afterOid: GitObjectID!
  beforeOid: GitObjectID
  force: Boolean = false
  name: GitRefname!
}

input IssueOrder {
  direction: OrderDirection!
  field: IssueOrderField!
}

enum IssueOrderField {
  COMMENTS
  CREATED_AT
  UPDATED_AT
}

enum OrderDirection {
  ASC
  DESC
}

enum PullRequestState {
  CLOSED
  MERGED
  OPEN
}

enum IssueStateReason {
  COMPLETED
  DUPLICATE
  NOT_PLANNED
  REOPENED
}

enum RepositoryVisibility {
  INTERNAL
  PRIVATE
  PUBLIC
}

enum RepositoryPermission {
  ADMIN
  MAINTAIN
  READ
  TRIAGE
  TRIAGE_PLUS
  WRITE
}

enum PullRequestTimelineItemsItemType {
  ADDED_TO_MERGE_QUEUE_EVENT ADDED_TO_PROJECT_EVENT ADDED_TO_PROJECT_V2_EVENT ADDED_TO_STACK_EVENT
  ARCHIVED_EVENT ASSIGNED_EVENT AUTOMATIC_BASE_CHANGE_FAILED_EVENT AUTOMATIC_BASE_CHANGE_SUCCEEDED_EVENT
  AUTO_MERGE_DISABLED_EVENT AUTO_MERGE_ENABLED_EVENT AUTO_REBASE_ENABLED_EVENT AUTO_SQUASH_ENABLED_EVENT
  BASE_REF_CHANGED_EVENT BASE_REF_DELETED_EVENT BASE_REF_FORCE_PUSHED_EVENT BLOCKED_BY_ADDED_EVENT
  BLOCKED_BY_REMOVED_EVENT BLOCKING_ADDED_EVENT BLOCKING_REMOVED_EVENT CLOSED_EVENT COMMENT_DELETED_EVENT
  CONNECTED_EVENT CONVERTED_FROM_DRAFT_EVENT CONVERTED_NOTE_TO_ISSUE_EVENT CONVERTED_TO_DISCUSSION_EVENT
  CONVERT_TO_DRAFT_EVENT CROSS_REFERENCED_EVENT DEMILESTONED_EVENT DEPLOYED_EVENT
  DEPLOYMENT_ENVIRONMENT_CHANGED_EVENT DISCONNECTED_EVENT HEAD_REF_DELETED_EVENT HEAD_REF_FORCE_PUSHED_EVENT
  HEAD_REF_RESTORED_EVENT ISSUE_COMMENT ISSUE_COMMENT_PINNED_EVENT ISSUE_COMMENT_UNPINNED_EVENT
  ISSUE_FIELD_ADDED_EVENT ISSUE_FIELD_CHANGED_EVENT ISSUE_FIELD_REMOVED_EVENT ISSUE_TYPE_ADDED_EVENT
  ISSUE_TYPE_CHANGED_EVENT ISSUE_TYPE_REMOVED_EVENT LABELED_EVENT LOCKED_EVENT MARKED_AS_DUPLICATE_EVENT
  MENTIONED_EVENT MERGED_EVENT MILESTONED_EVENT MOVED_COLUMNS_IN_PROJECT_EVENT PARENT_ISSUE_ADDED_EVENT
  PARENT_ISSUE_REMOVED_EVENT PINNED_EVENT PROJECT_V2_ITEM_STATUS_CHANGED_EVENT PULL_REQUEST_COMMIT
  PULL_REQUEST_COMMIT_COMMENT_THREAD PULL_REQUEST_REVIEW PULL_REQUEST_REVIEW_THREAD
  PULL_REQUEST_REVISION_MARKER READY_FOR_REVIEW_EVENT REFERENCED_EVENT REMOVED_FROM_MERGE_QUEUE_EVENT
  REMOVED_FROM_PROJECT_EVENT REMOVED_FROM_PROJECT_V2_EVENT REMOVED_FROM_STACK_EVENT RENAMED_TITLE_EVENT
  REOPENED_EVENT REVIEW_DISMISSED_EVENT REVIEW_REQUESTED_EVENT REVIEW_REQUEST_REMOVED_EVENT SUBSCRIBED_EVENT
  SUB_ISSUE_ADDED_EVENT SUB_ISSUE_REMOVED_EVENT TRANSFERRED_EVENT UNARCHIVED_EVENT UNASSIGNED_EVENT
  UNLABELED_EVENT UNLOCKED_EVENT UNMARKED_AS_DUPLICATE_EVENT UNPINNED_EVENT UNSUBSCRIBED_EVENT
  USER_BLOCKED_EVENT
}
`

// Type kinds of the schema.
const (
	kindScalar    = "SCALAR"
	kindObject    = "OBJECT"
	kindInterface = "INTERFACE"
	kindUnion     = "UNION"
	kindEnum      = "ENUM"
	kindInput     = "INPUT_OBJECT"
)

// gqlType is a type of the schema.
type gqlType struct {
	kind    string
	name    string
	fields  map[string]*gqlField // objects and interfaces
	ifaces  []string             // objects: the interfaces they implement
	members []string             // unions
	values  map[string]bool      // enums
	inputs  map[string]*gqlInput // input objects
	order   []string             // input field order
}

// gqlField is a field with its arguments.
type gqlField struct {
	name string
	args map[string]*gqlInput
	typ  *gqlTypeRef
}

// gqlInput is an argument or input field.
type gqlInput struct {
	name string
	typ  *gqlTypeRef
	def  *gqlValue
}

// gqlSchema is a parsed schema.
type gqlSchema struct {
	types map[string]*gqlType
}

var (
	schemaOnce sync.Once
	schemaDot  *gqlSchema
	schemaGHES *gqlSchema
	schemaErr  error
	notOnGHES  = []string{"Repository.hasPullRequestsEnabled"}
	valuesGHES = map[string][]string{
		"RepositoryPermission": {"TRIAGE_PLUS"},
		"PullRequestTimelineItemsItemType": {"ADDED_TO_PROJECT_V2_EVENT", "ADDED_TO_STACK_EVENT", "ARCHIVED_EVENT",
			"CONVERTED_FROM_DRAFT_EVENT", "ISSUE_COMMENT_PINNED_EVENT", "ISSUE_COMMENT_UNPINNED_EVENT",
			"ISSUE_FIELD_ADDED_EVENT", "ISSUE_FIELD_CHANGED_EVENT", "ISSUE_FIELD_REMOVED_EVENT",
			"PROJECT_V2_ITEM_STATUS_CHANGED_EVENT", "REMOVED_FROM_PROJECT_V2_EVENT", "REMOVED_FROM_STACK_EVENT",
			"UNARCHIVED_EVENT"},
	}
)

// fakeSchema returns the fake's schema for a flavor, parsed once. The GHES
// variant lacks what GitHub Enterprise Server 3.19's public schema lacks
// (https://docs.github.com/public/ghes-3.19/schema.docs-enterprise.graphql,
// read 2026-09-29): Repository.hasPullRequestsEnabled and some enum values.
func fakeSchema(f Flavor) (*gqlSchema, error) {
	schemaOnce.Do(func() {
		if schemaDot, schemaErr = parseSchema(schemaSDL); schemaErr != nil {
			return
		}
		if schemaGHES, schemaErr = parseSchema(schemaSDL); schemaErr != nil {
			return
		}
		for _, f := range notOnGHES {
			typ, name, _ := strings.Cut(f, ".")
			delete(schemaGHES.types[typ].fields, name)
		}
		for typ, values := range valuesGHES {
			for _, v := range values {
				delete(schemaGHES.types[typ].values, v)
			}
		}
	})
	if f == GHES {
		return schemaGHES, schemaErr
	}
	return schemaDot, schemaErr
}

// parseSchema parses the schema definition language subset: scalar,
// type, interface, union, enum and input definitions, with descriptions
// and directives skipped.
func parseSchema(src string) (*gqlSchema, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	sc := &gqlSchema{types: map[string]*gqlType{}}
	for _, builtin := range []string{"ID", "String", "Int", "Float", "Boolean"} {
		sc.types[builtin] = &gqlType{kind: kindScalar, name: builtin}
	}
	for p.tok.kind != tokEOF {
		if p.tok.kind == tokString {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		kw, err := p.name()
		if err != nil {
			return nil, err
		}
		switch kw {
		case "schema":
			if err := p.skipBlock(); err != nil {
				return nil, err
			}
			continue
		case "directive":
			if err := p.skipDirectiveDef(); err != nil {
				return nil, err
			}
			continue
		}
		name, err := p.name()
		if err != nil {
			return nil, err
		}
		t := &gqlType{name: name}
		if kw != "type" && kw != "interface" {
			if _, err := p.directives(); err != nil {
				return nil, err
			}
		}
		switch kw {
		case "scalar":
			t.kind = kindScalar
		case "type", "interface":
			t.kind = kindObject
			if kw == "interface" {
				t.kind = kindInterface
			}
			if p.tok.kind == tokName && p.tok.val == "implements" {
				if err := p.advance(); err != nil {
					return nil, err
				}
				for p.tok.kind == tokName || p.is("&") {
					if p.is("&") {
						if err := p.advance(); err != nil {
							return nil, err
						}
						continue
					}
					t.ifaces = append(t.ifaces, p.tok.val)
					if err := p.advance(); err != nil {
						return nil, err
					}
				}
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			if t.fields, err = p.fieldDefs(); err != nil {
				return nil, err
			}
		case "union":
			t.kind = kindUnion
			if err := p.expect("="); err != nil {
				return nil, err
			}
			for p.tok.kind == tokName || p.is("|") {
				if p.is("|") {
					if err := p.advance(); err != nil {
						return nil, err
					}
					continue
				}
				// A name followed by a keyword ends the union.
				if isKeyword(p.tok.val) && len(t.members) > 0 {
					break
				}
				t.members = append(t.members, p.tok.val)
				if err := p.advance(); err != nil {
					return nil, err
				}
			}
		case "enum":
			t.kind, t.values = kindEnum, map[string]bool{}
			if err := p.expect("{"); err != nil {
				return nil, err
			}
			for !p.is("}") {
				if p.tok.kind == tokString {
					if err := p.advance(); err != nil {
						return nil, err
					}
					continue
				}
				v, err := p.name()
				if err != nil {
					return nil, err
				}
				if _, err := p.directives(); err != nil {
					return nil, err
				}
				t.values[v] = true
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		case "input":
			t.kind = kindInput
			if err := p.expect("{"); err != nil {
				return nil, err
			}
			t.inputs = map[string]*gqlInput{}
			for !p.is("}") {
				if p.tok.kind == tokString {
					if err := p.advance(); err != nil {
						return nil, err
					}
					continue
				}
				in, err := p.inputDef()
				if err != nil {
					return nil, err
				}
				t.inputs[in.name] = in
				t.order = append(t.order, in.name)
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("schema: unknown definition %q", kw)
		}
		sc.types[name] = t
	}
	return sc, sc.check()
}

// skipBlock skips a "{ … }" block.
func (p *parser) skipBlock() error {
	if err := p.expect("{"); err != nil {
		return err
	}
	for depth := 1; depth > 0; {
		switch {
		case p.tok.kind == tokEOF:
			return p.fail()
		case p.is("{"):
			depth++
		case p.is("}"):
			depth--
		}
		if err := p.advance(); err != nil {
			return err
		}
	}
	return nil
}

// skipDirectiveDef skips "@name(args) repeatable on A | B".
func (p *parser) skipDirectiveDef() error {
	if err := p.expect("@"); err != nil {
		return err
	}
	if _, err := p.name(); err != nil {
		return err
	}
	if p.is("(") {
		if err := p.advance(); err != nil {
			return err
		}
		for !p.is(")") {
			if p.tok.kind == tokString {
				if err := p.advance(); err != nil {
					return err
				}
				continue
			}
			if _, err := p.inputDef(); err != nil {
				return err
			}
		}
		if err := p.advance(); err != nil {
			return err
		}
	}
	for p.tok.kind == tokName || p.is("|") {
		if p.tok.kind == tokName && isKeyword(p.tok.val) {
			return nil
		}
		if err := p.advance(); err != nil {
			return err
		}
	}
	return nil
}

// isKeyword reports whether a name starts a definition.
func isKeyword(v string) bool {
	switch v {
	case "type", "interface", "union", "enum", "input", "scalar", "directive", "schema":
		return true
	}
	return false
}

// fieldDefs parses "{ name(args): Type … }".
func (p *parser) fieldDefs() (map[string]*gqlField, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	out := map[string]*gqlField{}
	for !p.is("}") {
		if p.tok.kind == tokString {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		f := &gqlField{name: n, args: map[string]*gqlInput{}}
		if p.is("(") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			for !p.is(")") {
				in, err := p.inputDef()
				if err != nil {
					return nil, err
				}
				f.args[in.name] = in
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		if f.typ, err = p.typeRef(); err != nil {
			return nil, err
		}
		if _, err := p.directives(); err != nil {
			return nil, err
		}
		out[n] = f
	}
	return out, p.advance()
}

// inputDef parses "name: Type = default".
func (p *parser) inputDef() (*gqlInput, error) {
	for p.tok.kind == tokString {
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	n, err := p.name()
	if err != nil {
		return nil, err
	}
	if err := p.expect(":"); err != nil {
		return nil, err
	}
	in := &gqlInput{name: n}
	if in.typ, err = p.typeRef(); err != nil {
		return nil, err
	}
	if p.is("=") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		v, err := p.value(true)
		if err != nil {
			return nil, err
		}
		in.def = &v
	}
	if _, err := p.directives(); err != nil {
		return nil, err
	}
	return in, nil
}

// check verifies that every type the schema names is defined.
func (sc *gqlSchema) check() error {
	known := func(t *gqlTypeRef) error {
		if sc.types[t.named()] == nil {
			return fmt.Errorf("schema: unknown type %s", t.named())
		}
		return nil
	}
	for _, t := range sc.types {
		for _, f := range t.fields {
			if err := known(f.typ); err != nil {
				return err
			}
			for _, a := range f.args {
				if err := known(a.typ); err != nil {
					return err
				}
			}
		}
		for _, in := range t.inputs {
			if err := known(in.typ); err != nil {
				return err
			}
		}
		for _, m := range append(append([]string(nil), t.members...), t.ifaces...) {
			if sc.types[m] == nil {
				return fmt.Errorf("schema: unknown type %s", m)
			}
		}
	}
	return nil
}

// possible reports whether an object type name can be a value of type t.
func (sc *gqlSchema) possible(t *gqlType, object string) bool {
	switch t.kind {
	case kindObject:
		return t.name == object
	case kindUnion:
		for _, m := range t.members {
			if m == object {
				return true
			}
		}
	case kindInterface:
		if o := sc.types[object]; o != nil {
			for _, i := range o.ifaces {
				if i == t.name {
					return true
				}
			}
		}
	}
	return false
}

// overlaps reports whether a fragment on cond can apply within parent.
func (sc *gqlSchema) overlaps(parent, cond *gqlType) bool {
	if parent.name == cond.name {
		return true
	}
	for name, t := range sc.types {
		if t.kind == kindObject && sc.possible(parent, name) && sc.possible(cond, name) {
			return true
		}
	}
	return false
}
