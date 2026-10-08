// Package ghfake is an HTTP fake of GitHub for tests: the REST API, the
// GraphQL API and git smart HTTP on one httptest server, backed by real
// bare git repositories served by `git http-backend`. The GitHub driver,
// the conformance suite and distribute run against it without github.com.
//
// It models only what touchmark uses, and each behavior is taken from
// GitHub's documentation, from read-only calls to public github.com data,
// or assumed where neither settles it; the table below names the source
// of every one. Assumed behaviors are what the live GitHub sandbox must
// confirm (docs/project/e2e.md, live sandbox scenarios); a test that
// depends on one of them proves only the fake.
//
// # Layout
//
// New starts the server on 127.0.0.1. REST is served under / (the
// github.com layout, where api.github.com is a host of its own) and under
// /api/v3 (GitHub Enterprise Server); GraphQL at /graphql and
// /api/graphql; git at /<owner>/<repo>.git. Every route under
// /repos/{owner}/{repo} is also served under /repositories/{id}, the form
// of GitHub's redirects and Link headers. Options.Flavor picks github.com
// (DotCom) or GitHub Enterprise Server (GHES) behaviors.
//
// People and admins act through the setup methods (AddOrg, CreateRepo,
// Commit, OpenPR, MergePR, AddRuleset, RegisterApp, Install, …); what
// arrives over HTTP is judged as touchmark's (see Violations), except for
// accounts marked Human. Tests inject failures with Fail, read the request
// log with Requests and the counters with Usage.
//
// # GraphQL contract
//
// The fake parses each request as GraphQL, validates the chosen operation
// against schemaSDL — a subset of GitHub's public schema, field for field
// — and executes it over its state. Any query or mutation within the
// subset works, whatever its operation name, aliases, variables and
// fragments; anything else fails validation with GitHub's messages
// and codes (undefinedField, argumentNotAccepted, …) and HTTP 200 without
// data, as on GitHub. Supported roots: repository, node, nodes, user,
// organization, viewer, rateLimit, and the mutation updateRefs. The
// operation name only labels errors; Fail addresses GraphQL requests as
// "POST /graphql" or "GRAPHQL <root field>". Validate checks a document
// the same way without a server: drivers' tests validate their fixed
// queries with it.
//
// # Behaviors and their sources
//
// Sources: "docs" (docs.github.com or the public schema, read 2026-09-24
// to 2026-09-29), "observed" (read-only calls to public repositories on
// 2026-09-29), "reports" (public issue reports, not documentation), and
// "assumed" (needs the live sandbox).
//
//	Apps, JWTs and tokens
//	  JWT RS256; iat not ahead, exp ahead and <= 10 min;        docs apps/creating-github-apps/authenticating-with-a-github-app/
//	    iss = client id or App id                                  generating-a-json-web-token-jwt-for-a-github-app
//	  JWT error messages ('Expiration time' claim …)            reports
//	  POST /app/installations/{id}/access_tokens: repositories,  docs rest/apps/apps#create-an-installation-access-token-for-an-app
//	    repository_ids (<= 500), permissions; 201 token,
//	    expires_at, permissions, repository_selection
//	  token lives 1 hour; cannot exceed the installation's       docs …/generating-an-installation-access-token-for-a-github-app
//	    permissions or repositories
//	  422 messages for those two refusals                        reports; assumed
//	  metadata:read always granted                               assumed
//	  stateless "ghs_" + JWT tokens (~520 chars) on github.com,  changelog 2026-05-15; length assumed
//	    40-char tokens on GHES
//	  DELETE /installation/token 204, token dead at once         docs rest/apps/installations#revoke-an-installation-access-token
//	  GET /installation/repositories, total_count, pagination    docs rest/apps/installations
//	  GET /app, /app/installations[/{id}], /orgs|users/…/         docs rest/apps/apps
//	    installation, /repos/{o}/{r}/installation (JWT only)
//	  401 "Bad credentials" for expired/revoked tokens           assumed
//	  GET /user with an installation token: 403                  docs
//	  bot "<slug>[bot]", type Bot, email <id>+<slug>[bot]@        observed (a public bot account; an App's commit)
//	    users.noreply.github.com
//	  noreply addresses on GHES: <id>+<login>@users.noreply.<host> assumed (as the driver assumes)
//	  2000 token requests an hour per App                        docs (OAuth tokens); that it covers installations: assumed
//	  git: Basic auth, any user name, token as password          docs (x-access-token); any user name assumed
//
//	Repositories
//	  repository fields incl. has_pull_requests,                 observed (a public repository)
//	    pull_request_creation_policy, topics, visibility
//	  301 for a renamed or transferred repository to             observed (a transferred repository)
//	    /repositories/{id}, body {message, url, documentation_url}
//	  307 for other methods on the old path                      docs (temporary redirects); assumed for renames
//	  git on the old path redirects                              assumed
//	  old login of a renamed account: 404                        assumed
//	  old paths under a renamed account: 404 in the API,         docs (renaming an organization: "API requests
//	    git still redirects                                        that use the old organization's name will return a 404")
//	  GET /users/{u}/repos: public repositories only             docs rest/repos/repos#list-repositories-for-a-user
//	  GET /user/repos: affiliation, visibility, type (422 with   docs rest/repos/repos#list-repositories-for-the-
//	    the other two); organization membership not modeled        authenticated-user
//	  error body {message, documentation_url, status}            observed (status as a string)
//	  GET /orgs/{org}/repos: type, sort=created desc default,    docs rest/repos/repos#list-organization-repositories
//	    full_name asc; per_page <= 100
//	  Link header: prev, next, last, first; /repositories|        observed
//	    /organizations/{id} URLs, query order kept
//	  installation token lists the org's other public repos     assumed (reports: a community discussion)
//	  GET /repos/{o}/{r}/hash-algorithm sha1|sha256; absent on   docs; observed (sha1); GHES: rest-api-description ghes-3.19
//	    GHES
//	  forks carry parent and source                              docs
//	  drafts refused in private repositories of free owners:     docs (plans); message assumed
//	    422 "Draft pull requests are not supported …"
//	  rulesets not enforced in private repositories of free      docs (plans); rules/branches then [] assumed
//	    owners
//
//	Contents and git data
//	  contents: base64 wrapped at 60 characters; raw and object  docs rest/repos/contents; wrapping observed
//	    media types; > 1 MB: 403 too_large, object gives
//	    encoding "none"; directories <= 1000 entries
//	  symlink to a regular file: the target's size and content   observed (a public repository)
//	    under the link's name, path and sha
//	  other symlinks: type "symlink" with target                 docs
//	  submodule: type "submodule", submodule_git_url             observed (a public repository)
//	  "No commit found for the ref <ref>", "Branch not found"    observed
//	  empty repository: contents 404 "This repository is empty." assumed (message); trees 409 docs
//	    trees 409 "Git Repository is empty."
//	  trees: mode, type, sha, size (blobs), url (not for         observed (a public repository)
//	    submodules), truncated
//	  "HEAD" names the default branch in git/trees/HEAD,          observed (git/commits/HEAD is 404)
//	    contents?ref=HEAD and commits/HEAD
//	  POST /git/commits by an installation token without author, docs (signature verification for bots); observed
//	    committer, signature: author the bot, committer            (an App's API commit: committer GitHub, verified)
//	    "GitHub <noreply@github.com>", verified "valid"
//	  API commits unsigned on GHES unless web commit signing     docs (GHES web commit signing)
//	  POST /git/commits with a PAT: unsigned                     reports; assumed
//	  verification reasons (valid, unsigned, unknown_key,        docs rest/git/commits (names); when each applies: assumed
//	    bad_email, invalid, malformed_signature, …)
//	  SSH signatures verify with a registered signing key and    docs (SSH signature verification); rules assumed
//	    the owner's email
//	  git refs: create 422 "Reference already exists", update    docs rest/git/refs (codes); messages reports
//	    422 "Update is not a fast forward", delete 422 for the
//	    default branch, 409 on an empty repository
//	  Workflows needed for ref writes changing .github/workflows docs (permissions for Workflows); 403 shape assumed
//
//	Pull requests
//	  list: state, base, head "owner:branch"; a head without      docs rest/pulls/pulls; observed (bare head ignored,
//	    "owner:" is ignored; newest first; found after the         deleted head branch still found)
//	    head branch is gone
//	  create: 422 exists / no commits / invalid head or base     docs (422); messages reports
//	  body <= 65 536 characters                                  reports ("Body is too long"); message assumed
//	  PATCH title, body, state, base applied all or nothing      assumed
//	  reopen refused when the head branch is gone or was         reports; messages assumed
//	    force-pushed or recreated since the close
//	  labels: POST issues/{n}/labels creates missing labels;     reports; POST labels docs
//	    POST labels 422 already_exists
//	  a reader of a public repository may open a pull request    docs (pull_request_creation_policy); assumed for the API
//	    from a fork
//	  merge: merge, squash, rebase; 405 not mergeable, 409 sha   docs rest/pulls/pulls#merge-a-pull-request
//	  merge commits by GitHub, signed                            observed (web-flow committer, verified)
//	  deleting the head branch closes the pull request           docs (creating-and-deleting-branches)
//	  head moved to its base's tip closes the pull request       reports, undocumented
//	  head moved behind its base closes it                       assumed
//	  head reachable from a moved base: merged ("indirect")      docs (about-pull-request-merges#indirect-merges)
//	  base branch deleted: retargeted to the base of a merged    docs changelog 2020-05-19 (retargeting)
//	    pull request whose head it was, closed otherwise           closing: reports
//	  closer: actor of the last ClosedEvent; a merge adds        observed (GraphQL timeline of a public repository)
//	    MergedEvent then ClosedEvent
//	  actor of a close by branch deletion or push: the pusher    assumed
//	  force push adds HeadRefForcePushedEvent                    docs (schema); assumed when
//	  refs/pull/<n>/head follows the head, also from forks       docs (pull request refs); timing assumed
//
//	Pushes and rules
//	  push without write access: 403 "Permission to <repo>.git   reports; assumed
//	    denied to <login>."; archived: 403
//	  what one cannot see: 404 "Repository not found."           observed (anonymous, missing repository); private assumed
//	  Workflows refusal "refusing to allow a GitHub App to       reports (quoted by gitx); PAT wording assumed
//	    create or update workflow `<path>` without `workflows`
//	    permission", judged by the diff from the old tip (from the
//	    default branch for a new ref), also for hidden refs
//	    and moves across others' workflow changes                 assumed (public reports)
//	  GH013 refusal: "push declined due to repository rule       reports (GH013 issues); exact line layout assumed
//	    violations" with remote lines, "Cannot force-push to this
//	    branch", "Commits must have verified signatures." and
//	    "Found N violations:"
//	  other rule messages (creation, update, deletion, linear     assumed
//	    history, pull_request)
//	  signatures checked on the commits an update brings: old..  assumed
//	    new, or those no other branch has for a new branch
//	  rules/branches/{b}: active rules, also for a branch that    docs rest/repos/rules; observed (shape: type,
//	    does not exist, bypass ignored                             parameters, ruleset_source_type, source, id)
//	  ~DEFAULT_BRANCH, ~ALL, fnmatch patterns with **            docs (rulesets)
//	  rulesets/{id} current_user_can_bypass                      docs
//	  refs/pull/* refused: "deny updating a hidden ref"          assumed (git's receive.hideRefs wording)
//	  other refs (refs/touchmark/…) accepted, nothing follows     assumed
//	  pushes are all or nothing when one ref is refused          assumed
//
//	GraphQL
//	  schema subset field for field (TestSchemaSubset)           docs (public schemas fpt and ghes-3.19, 2026-09-29)
//	  GHES 3.19 lacks Repository.hasPullRequestsEnabled and some docs (ghes-3.19 schema)
//	    enum values: the GHES flavor refuses them
//	  NOT_FOUND "Could not resolve to a Repository with the      observed (type, path by alias, locations, data null)
//	    name 'o/n'." with HTTP 200 and the other aliases' data
//	  validation errors: path from "query [Name]", extensions   observed (undefinedField)
//	    code, no data
//	  Int is a signed 32-bit integer: a literal or variable      docs (GraphQL spec, Int)
//	    out of range fails validation or coercion
//	  a declared variable not used: variableNotUsed, "Variable   observed
//	    $v is declared by anonymous query but not used"
//	  object(expression): Blob for files and symlinks (the link  observed (a public repository)
//	    text), Tree for directories, null for submodules and
//	    missing paths; TreeEntry.mode as a decimal int
//	  Commit.file(path:): the entry of any type (submodule:       observed (a public repository)
//	    type commit, object null); a missing path, or one through
//	    a symlink, is null with NOT_FOUND "Could not resolve file
//	    for path '<path>'." on the field
//	  Bot.login without "[bot]"                                  observed (a public bot account)
//	  headRepository null once the fork is gone                  observed
//	  first/last required, <= 100                                docs (node limits); error types and messages assumed
//	  updateRefs: atomic, beforeOid zeros = must not exist,      docs (schema description of updateRefs)
//	    afterOid zeros deletes, force for non-fast-forwards
//	  updateRefs refusals: STALE_DATA for a moved ref,          assumed (modeled on createCommitOnBranch reports)
//	    UNPROCESSABLE, FORBIDDEN for Workflows
//	  viewer with an installation token: FORBIDDEN               assumed
//	  primary limit: HTTP 200, errors type RATE_LIMITED,         docs (200 and remaining 0); type and message reports
//	    x-ratelimit-remaining 0
//
//	Limits
//	  primary: 5000/h per installation and user, 60/h anonymous, docs rest/using-the-rest-api/rate-limits-for-the-rest-api
//	    x-ratelimit-limit/remaining/used/reset/resource;
//	    exhausted: 403 with remaining 0
//	  "API rate limit exceeded for installation ID <n>."         reports
//	  secondary: 900 REST points a minute (GET 1, writes 5),    docs; message reports
//	    2000 GraphQL points (query 1, mutation 5); 403 with
//	    retry-after
//	  content creation: 80 a minute, 500 an hour; 403 "…         docs (limits); message reports
//	    temporarily blocked from content creation …"
//	  which writes count as content: pull requests, comments,    docs for pull requests and comments; Git Data (commits,
//	    labels, API commits, API ref writes; not pushes            refs) and labels assumed
//	  GHES: limits off, no x-ratelimit headers                   docs (off by default); headers assumed
//	  X-GitHub-Api-Version 2022-11-28 everywhere, 2026-03-10 on   docs (API versions); 400 for others assumed
//	    github.com only
//
//	Settings of a repository (settings.go; touchmark setup, doctor --hub-token)
//	  environments: GET list and one (Actions read; anyone for   docs rest/deployments/environments; anonymous read
//	    a public repository), PUT (Administration write)           observed 2026-09-29
//	  PUT clears the protection rules it is not sent             assumed
//	  custom deployment branch policies refused (422) in a       docs (plans: environments in private repositories
//	    private repository of a free owner                          need Pro, Team or Enterprise); status assumed
//	  deployment-branch-policies: list, POST (404 unless the     docs rest/deployments/branch-policies; 422 for a
//	    environment selects custom policies), DELETE                duplicate assumed (the docs say 303)
//	  secrets: names only, of the repository, its environments,  docs rest/actions/secrets, rest/dependabot/secrets;
//	    Dependabot and the organization's shared with it;           values arrive as libsodium sealed boxes: the fake
//	    SetSecret and SetOrgSecret stand for gh secret set          takes none
//	  variables of the repository and its environments: GET,     docs rest/actions/variables (409 "already exists")
//	    POST 201 (409 when it exists), PATCH 204
//	  POST /repos/{o}/{r}/rulesets (Administration write); a     docs rest/repos/rules; the 403 message reports
//	    private repository of a free owner: 403
//	  the Admin role: a classic token with repo, a fine-grained  docs (permissions of fine-grained tokens)
//	    one with the permission, of an admin of the repository
//	  App manifest flow: POST /organizations/{org}/settings/     docs apps/sharing-github-apps/registering-a-
//	    apps/new and /settings/apps/new take the form field        github-app-from-a-manifest; the confirmation page
//	    manifest and redirect to its redirect_url with code and    is skipped, a loopback redirect_url accepted
//	    state; POST /app-manifests/{code}/conversions once per     (assumed)
//	    code within an hour: 201 with id, slug, client_id,
//	    client_secret, webhook_secret, pem; the App gets
//	    default_permissions
//
//	Instance
//	  GET /meta: github.com without installed_version; GHES      observed (github.com); docs (GHES meta)
//	    installed_version (Options.Version, 3.19.0)
//	  GHES: X-GitHub-Enterprise-Version on every REST answer     docs (GHES REST API)
//
// # Violations
//
// Violations returns what touchmark must never do: a head moved to its
// base, the branch of an open pull request deleted, a push to the branch
// of a closed pull request not followed by a new one, a move of a branch
// carrying a foreign open pull request, a move of the default branch, a
// narrowed installation token used on another repository, and a write
// with an expired, revoked or suspended token. Tests assert it is empty.
package ghfake
