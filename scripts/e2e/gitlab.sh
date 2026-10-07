#!/usr/bin/env bash
# gitlab.sh runs the live end-to-end tests of touchmark against GitLab
# (package internal/e2e/gitlab, build tag e2e) in Docker: locally (Git Bash
# on Windows, Linux, macOS) and in CI. docs/project/e2e.md explains what they cover.
#
# Usage: scripts/e2e/gitlab.sh [--keep] [--only-seed] [--run REGEXP]
#        [--template DIR [--touchmark-image REF]] IMAGE|all
#
#   IMAGE       a GitLab CE image: gitlab/gitlab-ce:18.11.12-ce.0, ...
#   all         the three supported images, one after the other
#   --keep      keep GitLab, the runner, the network and an env file with
#               the tokens for debugging, and publish GitLab on a loopback
#               port; the script prints how to reach and remove them
#   --only-seed start and seed GitLab and the runner, check the seeding and
#               stop there, without the tests (with --keep: a seeded
#               instance to work against)
#   --run RE    run only the tests that match RE (go test -run)
#   --template DIR
#               also run the hub template's .gitlab-ci.yml, from the
#               template's working tree DIR (TestTemplate; without it the
#               test skips): the touchmark image built from this working
#               tree, a local registry, and a second runner, with the
#               Docker executor, for the group the hubs live in
#               (scripts/e2e/template-lib.sh)
#   --touchmark-image REF
#               with --template: run the jobs in this image instead of one
#               built from the working tree, such as a release candidate
#
# Environment: TOUCHMARK_E2E_READY_TIMEOUT (seconds GitLab may take to boot,
# 900 by default; Docker Desktop on a busy Windows machine needs more),
# TOUCHMARK_E2E_TEST_TIMEOUT (go test -timeout, 45m), TOUCHMARK_E2E_GO_IMAGE.
#
# GitLab needs about 4 GB of memory: the script runs one GitLab container at
# a time and refuses to start while another one runs.
#
# For each image the script creates the network touchmark-e2e-<rand> and
# starts GitLab in it as touchmark-e2e-gitlab: external_url http://localhost,
# Puma in single mode, 10 Sidekiq threads, no Prometheus, registry, Pages or
# KAS, --shm-size 256m and a memory limit of 4.5 GB. It waits for
# /-/readiness (at most 15 minutes, printing progress) and seeds it:
#
#   - a personal access token of root, which only the fixtures use, through
#     one gitlab-rails runner script (Ruby, copied into the container; the
#     token comes back on its standard output);
#   - the group acme with the subgroup acme/sub;
#   - the reader and the writer of docs/getting-started/gitlab.md: instance
#     service accounts with personal access tokens, made members of acme as
#     Reporter and Developer, where the instance has the service accounts
#     API; else group access tokens of acme, the reader Reporter with
#     read_api and read_repository, the writer Developer with api and
#     write_repository.
#     Checked on the CE images: 18.11.12 answers 404 to GET
#     /service_accounts (the API is EE code there,
#     ee/lib/api/service_accounts.rb) and takes group access tokens; 19.4.1
#     has instance service accounts. The script logs which path it took,
#     and the tests get it in TOUCHMARK_E2E_GITLAB_ACCOUNTS;
#   - the person jdoe, an owner of acme, with a personal access token;
#   - an instance runner (gitlab/gitlab-runner of the same minor, Alpine,
#     shell executor), registered with a runner authentication token from
#     POST /user/runners, with a linux touchmark built from the working tree
#     at /opt/touchmark/touchmark.
#
# With --template DIR it also creates the group hubs, of which the person is
# an Owner, builds the touchmark image (or takes --touchmark-image), pushes
# it to a registry on 127.0.0.1 and starts the image runner
# (touchmark-e2e-gitlab-image-runner, the runner image above with the Docker
# executor, through the Docker socket) as a group runner of hubs: each job
# runs in the image the job names, as that image's user, in the Docker
# host's network namespace, where a forwarder on 127.0.0.1:80 and [::1]:80
# leads to GitLab, so that the jobs reach it at http://localhost too. The
# template's hub turns the instance runners off, so only this runner runs
# its jobs. The job containers and their volumes are the runner's (named
# runner-*); the cleanup removes any it left.
#
# It checks the seeding through the API (who each token acts as, its scopes,
# the roles in acme, the runner online) and runs
#
#   go test -tags e2e -count=1 -race -v ./internal/e2e/gitlab/...
#
# in golang:1.27. touchmark sends a credential over plain http only to
# loopback (docs/project/threat-model.md, "Tokens and git"), so the tests and
# the runner share GitLab's network namespace and reach it at
# http://localhost, its external_url: clone URLs in API answers and
# CI_SERVER_URL in jobs are the same URL.
#
# Secrets never appear on a command line or in a container's configuration.
# No root password is passed in: omnibus generates one on the first boot,
# which nothing here uses (root's token comes from a gitlab-rails runner
# script), and its file /etc/gitlab/initial_root_password is deleted once
# GitLab is ready. The tokens come back from GitLab on standard output,
# curl reads its PRIVATE-TOKEN header from standard input, and the test
# container gets them through a temporary env file, created with umask 077
# and, under Git Bash or Cygwin, where the mode does not reach NTFS, cut
# down with icacls to the current user alone. Containers with their
# anonymous volumes (the images declare /etc/gitlab, /var/log/gitlab,
# /var/opt/gitlab and the runner's /etc/gitlab-runner, /home/gitlab-runner),
# the network, the volume with the binary and the env files are removed on
# exit, also after a failure or Ctrl-C, unless --keep.

set -euo pipefail

# Git Bash on Windows rewrites arguments that look like POSIX paths (/src,
# /out, ...) for native programs such as docker.exe. Host paths are converted
# explicitly with host_path instead.
export MSYS_NO_PATHCONV=1

readonly SUPPORTED_IMAGES=(
	gitlab/gitlab-ce:17.11.7-ce.0
	gitlab/gitlab-ce:18.11.12-ce.0
	gitlab/gitlab-ce:19.4.1-ce.0
)
readonly GO_IMAGE=${TOUCHMARK_E2E_GO_IMAGE:-golang:1.27}
readonly TEST_TIMEOUT=${TOUCHMARK_E2E_TEST_TIMEOUT:-45m}
readonly READY_TIMEOUT=${TOUCHMARK_E2E_READY_TIMEOUT:-900}
readonly URL=http://localhost
readonly GITLAB=touchmark-e2e-gitlab
readonly RUNNER=touchmark-e2e-gitlab-runner
readonly IMAGE_RUNNER=touchmark-e2e-gitlab-image-runner
readonly HUB_GROUP=hubs
readonly GROUP=acme
readonly SUBGROUP=sub
readonly ROOT=root
readonly READER=touchmark-reader
readonly WRITER=touchmark-writer
readonly PERSON=jdoe
readonly TOUCHMARK_BIN=/opt/touchmark/touchmark
# The roles and scopes of docs/getting-started/gitlab.md. GitLab's access
# levels: 20 Reporter, 30 Developer, 50 Owner.
readonly READER_LEVEL=20
readonly WRITER_LEVEL=30
readonly PERSON_LEVEL=50
readonly READER_SCOPES='["read_api","read_repository"]'
readonly WRITER_SCOPES='["api","write_repository"]'
readonly PERSON_SCOPES='["api","read_user","read_repository","write_repository"]'

SCRIPT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
readonly SCRIPT REPO

keep=0
only_seed=0
run=
image=
template=
tpl_source=

usage() {
	sed -n '/^# Usage:/,/^# GitLab needs/p' "$SCRIPT" | sed '$d' | sed 's/^# \{0,1\}//'
}

die() {
	echo "gitlab.sh: $*" >&2
	exit 1
}

log() {
	echo "gitlab.sh: $*" >&2
}

# host_path turns a path of this shell into one docker.exe understands.
host_path() {
	if command -v cygpath >/dev/null 2>&1; then
		cygpath -m -- "$1"
	else
		printf '%s\n' "$1"
	fi
}

rand_hex() {
	od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'
}

# shellcheck source=scripts/e2e/template-lib.sh
. "$(dirname "$SCRIPT")/template-lib.sh"

# private_file creates an empty temporary file only the current user can
# read: umask 077 on Linux and macOS; under Git Bash or Cygwin, where the
# mode does not reach NTFS and the file would inherit the ACL of the
# temporary directory, icacls replaces that with the current user alone.
private_file() {
	local old f
	old=$(umask)
	umask 077
	f=$(mktemp "${TMPDIR:-/tmp}/touchmark-e2e-XXXXXXXX")
	umask "$old"
	chmod 600 "$f"
	case $(uname -s) in
	MINGW* | MSYS* | CYGWIN*)
		MSYS2_ARG_CONV_EXCL='*' icacls "$(cygpath -w "$f")" /inheritance:r /grant:r "${USERNAME:?}:(R,W,D)" >/dev/null ||
			die "cannot restrict $f to $USERNAME"
		;;
	esac
	printf '%s' "$f"
}

# expiry prints the date 30 days from now: GitLab refuses tokens without an
# expiry date (at most 365 days ahead by default).
expiry() {
	date -u -d '+30 days' +%F 2>/dev/null || date -u -v+30d +%F
}

# The resources of the image under test, removed by cleanup.
net=
started_gitlab=0
started_runner=0
started_image_runner=0
image_runner_token=
volume=
gotest=
omnibus_env=
test_env=

# volumes_of CONTAINER prints the names of the volumes mounted in it.
volumes_of() {
	docker container inspect -f '{{range .Mounts}}{{if .Name}}{{.Name}} {{end}}{{end}}' "$1" 2>/dev/null || true
}

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	if [ -n "$omnibus_env" ]; then rm -f -- "$omnibus_env"; fi
	if [ "$keep" = 1 ] && [ "$started_gitlab" = 1 ]; then
		print_kept
	else
		# docker rm -v removes the anonymous volumes of the images' VOLUME
		# lines with the containers (about 0.5 GB of GitLab's data per
		# run); any volume of theirs still there afterwards is removed too.
		local left="" v
		if [ "$started_runner" = 1 ]; then left+=" $(volumes_of "$RUNNER")"; fi
		if [ "$started_image_runner" = 1 ]; then left+=" $(volumes_of "$IMAGE_RUNNER")"; fi
		if [ "$started_gitlab" = 1 ]; then left+=" $(volumes_of "$GITLAB")"; fi
		if [ -n "$gotest" ]; then docker rm -f -v "$gotest" >/dev/null 2>&1 || true; fi
		if [ "$started_image_runner" = 1 ]; then
			docker rm -f -v "$IMAGE_RUNNER" >/dev/null 2>&1 || true
			remove_job_leftovers
		fi
		if [ "$started_runner" = 1 ]; then docker rm -f -v "$RUNNER" >/dev/null 2>&1 || true; fi
		if [ "$started_gitlab" = 1 ]; then docker rm -f -v "$GITLAB" >/dev/null 2>&1 || true; fi
		if [ -n "$volume" ]; then docker volume rm -f "$volume" >/dev/null 2>&1 || true; fi
		for v in $left; do
			if [ "$v" != "$volume" ] && docker volume inspect "$v" >/dev/null 2>&1; then
				docker volume rm -f "$v" >/dev/null 2>&1 || echo "gitlab.sh: cannot remove the volume $v" >&2
			fi
		done
		tpl_cleanup
		if [ -n "$net" ]; then docker network rm "$net" >/dev/null 2>&1 || true; fi
		if [ -n "$test_env" ]; then rm -f -- "$test_env"; fi
	fi
	exit "$status"
}

# remove_job_leftovers removes the containers and volumes the image runner's
# Docker executor made and did not remove (a job cut short): the runner
# labels them with its short token.
remove_job_leftovers() {
	[ -n "$image_runner_token" ] || return 0
	local short=${image_runner_token#glrt-} ids
	short=${short:0:8}
	ids=$(docker ps -aq --filter "label=com.gitlab.gitlab-runner.runner.id=$short" 2>/dev/null || true)
	if [ -n "$ids" ]; then
		# shellcheck disable=SC2086 # one id per word
		docker rm -f -v $ids >/dev/null 2>&1 || true
	fi
	ids=$(docker volume ls -q --filter "label=com.gitlab.gitlab-runner.runner.id=$short" 2>/dev/null || true)
	if [ -n "$ids" ]; then
		# shellcheck disable=SC2086 # one name per word
		docker volume rm -f $ids >/dev/null 2>&1 || true
	fi
}

print_kept() {
	local published mount=""
	published=$(docker port "$GITLAB" 80/tcp 2>/dev/null | head -n 1 || true)
	if [ -n "$template" ]; then
		mount=" -v '$(host_path "$template")':/template:ro"
	fi
	cat >&2 <<EOF
gitlab.sh: kept for debugging (--keep):
  network    $net
  gitlab     $GITLAB${published:+, published at http://$published (sign in with a token from the env file)}
  runner     $RUNNER${template:+
  template   the image runner $IMAGE_RUNNER, the registry $tpl_registry, the forwarder $tpl_forwarder}
  volume     $volume (the linux touchmark the runner runs)
  env file   ${test_env:-none} (the tokens; readable by you alone)
  run the tests again:
    docker run --rm --network container:$GITLAB --env-file '$(host_path "${test_env:-ENV_FILE}")' \\
      -v '$(host_path "$REPO")':/src:ro -w /src$mount $GO_IMAGE \\
      sh -c 'git config --global --add safe.directory /src && go test -tags e2e -count=1 -race -v ./internal/e2e/gitlab/...'
  remove everything (-v: with the containers' anonymous volumes):
    docker rm -f -v $RUNNER $GITLAB${template:+ $IMAGE_RUNNER $tpl_registry $tpl_forwarder} && docker volume rm $volume && docker network rm $net && rm -f '${test_env:-ENV_FILE}'
EOF
}

# curl_bin is curl inside the GitLab container.
curl_bin=curl

# api TOKEN METHOD PATH [JSON] sends a request to GitLab's REST API (under
# /api/v4) with curl inside the GitLab container, prints the response body
# and fails on a status other than 2xx. The token reaches curl on its
# standard input, as a PRIVATE-TOKEN header.
api() {
	local token=$1 method=$2 path=$3 body=${4-} out status
	local -a data=()
	if [ -n "$body" ]; then
		data=(--data-binary "$body")
	fi
	out=$(printf 'PRIVATE-TOKEN: %s\nContent-Type: application/json\nAccept: application/json\n' "$token" |
		docker exec -i "$GITLAB" "$curl_bin" -sS -H @- -X "$method" ${data[@]+"${data[@]}"} -w '\n%{http_code}' "$URL/api/v4$path") ||
		die "$method $path: curl failed"
	status=${out##*$'\n'}
	out=${out%$'\n'*}
	case $status in
	2??) printf '%s' "$out" ;;
	*)
		echo "gitlab.sh: $method $path: HTTP $status: ${out:0:500}" >&2
		return 1
		;;
	esac
}

# api_status TOKEN METHOD PATH [JSON] prints only the status of a request.
api_status() {
	local token=$1 method=$2 path=$3 body=${4-}
	local -a data=()
	if [ -n "$body" ]; then
		data=(--data-binary "$body")
	fi
	printf 'PRIVATE-TOKEN: %s\nContent-Type: application/json\n' "$token" |
		docker exec -i "$GITLAB" "$curl_bin" -sS -H @- -X "$method" ${data[@]+"${data[@]}"} -o /dev/null -w '%{http_code}' "$URL/api/v4$path"
}

# json_num NAME prints the first number field NAME of the JSON on standard
# input; json_str NAME the first string field.
json_num() {
	{ grep -o "\"$1\":[0-9][0-9]*" || true; } | head -n 1 | sed 's/.*://'
}

json_str() {
	{ grep -o "\"$1\":\"[^\"]*\"" || true; } | head -n 1 | sed 's/^[^:]*:"//; s/"$//'
}

wait_ready() {
	local started=$SECONDS deadline=$((SECONDS + READY_TIMEOUT)) next=$((SECONDS + 30)) state
	until docker exec "$GITLAB" "$curl_bin" -fsS -o /dev/null "$URL/-/readiness?all=1" >/dev/null 2>&1 &&
		[ "$(docker exec "$GITLAB" "$curl_bin" -sS -o /dev/null -w '%{http_code}' "$URL/api/v4/version" 2>/dev/null)" = 401 ]; do
		# Only an answer of the daemon counts: on a loaded machine docker
		# inspect itself may fail now and then.
		if state=$(docker inspect -f '{{.State.Running}}' "$GITLAB" 2>/dev/null) && [ "$state" != true ]; then
			docker logs --tail 100 "$GITLAB" >&2 || true
			die "GitLab stopped before it became ready"
		fi
		if ((SECONDS >= deadline)); then
			docker logs --tail 100 "$GITLAB" >&2 || true
			die "GitLab is not ready after ${READY_TIMEOUT}s"
		fi
		if ((SECONDS >= next)); then
			log "waiting for GitLab: $((SECONDS - started))s, $(docker stats --no-stream --format '{{.MemUsage}}' "$GITLAB" 2>/dev/null || echo '?')"
			next=$((SECONDS + 30))
		fi
		sleep 3
	done
}

# The Ruby script that mints root's token. It prints the token as
# TOUCHMARK_ROOT_TOKEN=<token> on standard output; nothing else of it is
# read. Organizations exist on 17.x and later; a token needs one from the
# version that added the column. Single quotes: Ruby, not the shell,
# reads it.
# shellcheck disable=SC2016
readonly SEED_RUBY='
root = User.find_by_username!("root")
token = root.personal_access_tokens.build(
  name: "touchmark-e2e",
  scopes: %w[api read_user read_repository write_repository],
  expires_at: Date.today + 30
)
if token.respond_to?(:organization_id=) && token.organization_id.nil?
  org = defined?(Organizations::Organization) ? Organizations::Organization.order(:id).first : nil
  token.organization_id = org.id if org
end
token.save!
$stdout.puts "TOUCHMARK_ROOT_TOKEN=#{token.token}"
$stdout.flush
'

seed_root_token() {
	# gitlab-rails runner runs as the user git: the script (no secret in it)
	# must be readable by that user.
	printf '%s' "$SEED_RUBY" | docker exec -i "$GITLAB" sh -c 'cat > /tmp/touchmark-seed.rb && chmod 644 /tmp/touchmark-seed.rb'
	local out status=0
	out=$(docker exec "$GITLAB" gitlab-rails runner /tmp/touchmark-seed.rb 2>&1) || status=$?
	docker exec "$GITLAB" rm -f /tmp/touchmark-seed.rb
	root_token=$(printf '%s\n' "$out" | sed -n 's/^TOUCHMARK_ROOT_TOKEN=//p' | tr -d '\r' | head -n 1)
	if [ "$status" != 0 ] || ! [[ $root_token =~ ^glpat-[A-Za-z0-9_.-]{20,}$ ]]; then
		printf '%s\n' "$out" | grep -v '^TOUCHMARK_ROOT_TOKEN=' | tail -n 20 >&2
		die "gitlab-rails runner printed no root token (exit $status)"
	fi
}

# create_group NAME [PARENT_ID] creates a private group and prints its id.
create_group() {
	local body="{\"name\":\"$1\",\"path\":\"$1\",\"visibility\":\"private\""
	if [ -n "${2-}" ]; then body+=",\"parent_id\":$2"; fi
	api "$root_token" POST /groups "$body}" | json_num id
}

add_member() {
	api "$root_token" POST "/groups/$1/members" "{\"user_id\":$2,\"access_level\":$3}" >/dev/null
}

# user_token USER_ID SCOPES mints a personal access token for a user as an
# admin (POST /users/:id/personal_access_tokens).
user_token() {
	api "$root_token" POST "/users/$1/personal_access_tokens" \
		"{\"name\":\"touchmark-e2e\",\"scopes\":$2,\"expires_at\":\"$exp\"}" | json_str token
}

# service_account LOGIN LEVEL SCOPES creates an instance service account
# with a token and makes it a member of acme. It fails (status 1) when the
# instance has no service accounts API (the CE images before 19.x, where it
# is EE code); the caller then takes group access tokens.
service_account() {
	local login=$1 level=$2 scopes=$3 status out id token
	status=$(api_status "$root_token" GET "/service_accounts?per_page=1")
	if [ "$status" != 200 ]; then
		log "GET /service_accounts: HTTP $status: no service accounts here; group access tokens instead"
		return 1
	fi
	out=$(api "$root_token" POST /service_accounts "{\"name\":\"$login\",\"username\":\"$login\"}") || return 1
	id=$(printf '%s' "$out" | json_num id)
	[ -n "$id" ] || die "POST /service_accounts answered without an id: ${out:0:300}"
	token=$(user_token "$id" "$scopes") || token=
	if [ -z "$token" ]; then
		# The service account API's own token endpoint (group service
		# accounts have one; instance ones may, depending on the version).
		token=$(api "$root_token" POST "/service_accounts/$id/personal_access_tokens" \
			"{\"name\":\"touchmark-e2e\",\"scopes\":$scopes,\"expires_at\":\"$exp\"}" | json_str token)
	fi
	[ -n "$token" ] || die "cannot mint a token for the service account $login"
	add_member "$group_id" "$id" "$level"
	printf '%s %s %s' "$id" "$login" "$token"
}

# group_token NAME LEVEL SCOPES creates a group access token of acme and
# prints the id and username of its bot user and the token.
group_token() {
	local name=$1 level=$2 scopes=$3 out id token login
	out=$(api "$root_token" POST "/groups/$group_id/access_tokens" \
		"{\"name\":\"$name\",\"scopes\":$scopes,\"access_level\":$level,\"expires_at\":\"$exp\"}") ||
		die "cannot create the group access token $name"
	id=$(printf '%s' "$out" | json_num user_id)
	token=$(printf '%s' "$out" | json_str token)
	[ -n "$id" ] && [ -n "$token" ] || die "the group access token $name came without its user or token"
	login=$(api "$root_token" GET "/users/$id" | json_str username)
	[ -n "$login" ] || die "cannot read the bot user of $name"
	printf '%s %s %s' "$id" "$login" "$token"
}

seed() {
	exp=$(expiry)
	seed_root_token
	group_id=$(create_group "$GROUP")
	[ -n "$group_id" ] || die "cannot create the group $GROUP"
	subgroup_id=$(create_group "$SUBGROUP" "$group_id")
	[ -n "$subgroup_id" ] || die "cannot create the group $GROUP/$SUBGROUP"

	local out
	out=$(api "$root_token" POST /users "{\"username\":\"$PERSON\",\"name\":\"Jane Doe\",\"email\":\"$PERSON@example.com\",\"password\":\"Pw-$(rand_hex 16)\",\"skip_confirmation\":true}")
	person_id=$(printf '%s' "$out" | json_num id)
	[ -n "$person_id" ] || die "cannot create the user $PERSON"
	person_token=$(user_token "$person_id" "$PERSON_SCOPES")
	[ -n "$person_token" ] || die "cannot mint a token for $PERSON"
	add_member "$group_id" "$person_id" "$PERSON_LEVEL"

	local reader writer
	if reader=$(service_account "$READER" "$READER_LEVEL" "$READER_SCOPES"); then
		writer=$(service_account "$WRITER" "$WRITER_LEVEL" "$WRITER_SCOPES") ||
			die "the reader became a service account, the writer cannot"
		accounts=service-account
	else
		reader=$(group_token "$READER" "$READER_LEVEL" "$READER_SCOPES") || exit 1
		writer=$(group_token "$WRITER" "$WRITER_LEVEL" "$WRITER_SCOPES") || exit 1
		accounts=group-access-token
	fi
	read -r reader_id reader_login reader_token <<<"$reader"
	read -r writer_id writer_login writer_token <<<"$writer"

	out=$(api "$root_token" POST /user/runners '{"runner_type":"instance_type","run_untagged":true,"description":"touchmark-e2e"}') ||
		die "cannot create a runner"
	runner_id=$(printf '%s' "$out" | json_num id)
	runner_token=$(printf '%s' "$out" | json_str token)
	[ -n "$runner_id" ] && [ -n "$runner_token" ] || die "POST /user/runners answered without an id or token"
}

# check_user LOGIN TOKEN checks that TOKEN acts as LOGIN and is no admin.
check_user() {
	local login=$1 token=$2 user
	user=$(api "$token" GET /user) || die "the token of $login does not work"
	[ "$(printf '%s' "$user" | json_str username)" = "$login" ] || die "the token of $login acts as someone else"
	case $user in *'"is_admin":true'*) die "$login is an admin" ;; esac
}

# check_scopes LOGIN TOKEN SCOPES checks the scopes of a token as
# GET /personal_access_tokens/self reports them.
check_scopes() {
	local login=$1 token=$2 want=$3 got
	got=$(api "$token" GET /personal_access_tokens/self | { grep -o '"scopes":\[[^]]*\]' || true; } | head -n 1)
	[ "$got" = "\"scopes\":$want" ] || die "the token of $login has $got, want $want"
}

# check_role USER_ID LOGIN LEVEL checks a member of acme and of its
# subgroup (inherited), as the reader sees it: the check that plan --strict
# does on targets, through members/all.
check_role() {
	local id=$1 login=$2 level=$3 got g
	for g in "$group_id" "$subgroup_id"; do
		got=$(api "$reader_token" GET "/groups/$g/members/all/$id" | json_num access_level)
		[ "$got" = "$level" ] || die "$login has access level ${got:-none} in group $g, want $level"
	done
}

verify_seed() {
	local user status login
	user=$(api "$root_token" GET /user) || die "root's token does not work"
	case $user in *'"is_admin":true'*) ;; *) die "root's token is not an admin's" ;; esac
	check_user "$reader_login" "$reader_token"
	check_user "$writer_login" "$writer_token"
	check_user "$PERSON" "$person_token"
	check_scopes "$reader_login" "$reader_token" "$READER_SCOPES"
	check_scopes "$writer_login" "$writer_token" "$WRITER_SCOPES"
	check_role "$reader_id" "$reader_login" "$READER_LEVEL"
	check_role "$writer_id" "$writer_login" "$WRITER_LEVEL"
	check_role "$person_id" "$PERSON" "$PERSON_LEVEL"
	# The reader reads but never writes: a project in acme is refused.
	status=$(api_status "$reader_token" POST /projects "{\"name\":\"touchmark-e2e-forbidden\",\"namespace_id\":$group_id}")
	[ "$status" = 403 ] || die "the reader may create a project in $GROUP (HTTP $status, want 403)"
	if [ "$accounts" = service-account ]; then
		for login in "$reader_login" "$writer_login"; do
			user=$(api "$root_token" GET "/users?username=$login")
			case $user in *'"bot":true'*) ;; *) log "note: $login is not a bot in /users: ${user:0:300}" ;; esac
		done
	fi
}

start_gitlab() {
	local others
	others=$(docker ps --format '{{.Names}} {{.Image}}' | { grep -E ' gitlab/gitlab-(ce|ee)' || true; })
	[ -z "$others" ] || die "another GitLab container runs ($others): run one at a time, GitLab needs about 4 GB"
	if docker container inspect "$GITLAB" >/dev/null 2>&1; then
		die "a container $GITLAB exists (kept by --keep?): remove it with docker rm -f $GITLAB $RUNNER"
	fi
	omnibus_env=$(private_file)
	# One line: docker's env files do not continue lines. Every key is valid
	# in 17.11 to 19.4; an unknown key would fail reconfigure. No
	# initial_root_password: the configuration stays in the container's
	# environment (docker inspect) for its life, so it holds no secret.
	{
		printf 'GITLAB_OMNIBUS_CONFIG='
		printf "external_url '%s'; " "$URL"
		printf "gitlab_rails['usage_ping_enabled'] = false; "
		printf "puma['worker_processes'] = 0; "
		printf "sidekiq['concurrency'] = 10; "
		printf "prometheus_monitoring['enable'] = false; "
		printf "registry['enable'] = false; "
		printf "gitlab_pages['enable'] = false; "
		printf "gitlab_kas['enable'] = false; "
		printf "package['modify_kernel_parameters'] = false; "
		printf "postgresql['shared_buffers'] = '256MB'\n"
	} >"$omnibus_env"
	local -a publish=()
	if [ "$keep" = 1 ]; then
		publish=(-p 127.0.0.1::80)
	fi
	started_gitlab=1
	docker run -d --name "$GITLAB" --network "$net" --shm-size 256m --memory 4608m \
		${publish[@]+"${publish[@]}"} --env-file "$(host_path "$omnibus_env")" "$image" >/dev/null
	rm -f -- "$omnibus_env"
	omnibus_env=
	if ! docker exec "$GITLAB" sh -c 'command -v curl' >/dev/null 2>&1; then
		curl_bin=/opt/gitlab/embedded/bin/curl
	fi
}

# build_touchmark builds a linux touchmark from the working tree into the
# volume the runner mounts.
build_touchmark() {
	volume=touchmark-e2e-$suffix-bin
	docker volume create "$volume" >/dev/null
	docker run --rm --name "touchmark-e2e-$suffix-build" -v "$(host_path "$REPO"):/src:ro" -v "$volume:/out" -w /src \
		-e CGO_ENABLED=0 -e GOTOOLCHAIN=local -e GOFLAGS=-mod=readonly \
		"$GO_IMAGE" go build -buildvcs=false -trimpath -o /out/touchmark ./cmd/touchmark
}

# runner_image is the runner of the minor of the GitLab image; a runner
# works with GitLab of its own minor and older. The Alpine flavour: its git
# (2.47 to 2.54) runs plan, which needs 2.45; the Ubuntu one has 2.43.
runner_image() {
	case $image in
	*:17.11.*) echo gitlab/gitlab-runner:alpine-v17.11.4 ;;
	*:18.11.*) echo gitlab/gitlab-runner:alpine-v18.11.4 ;;
	*) echo gitlab/gitlab-runner:alpine-v19.4.1 ;;
	esac
}

start_runner() {
	local img
	img=$(runner_image)
	if ! docker image inspect "$img" >/dev/null 2>&1; then
		docker pull -q "$img" >/dev/null
	fi
	started_runner=1
	docker run -d --name "$RUNNER" --network "container:$GITLAB" -v "$volume:/opt/touchmark:ro" "$img" >/dev/null
	# The token reaches gitlab-runner register through the environment of
	# docker exec, named without a value on its command line.
	CI_SERVER_TOKEN=$runner_token docker exec -e CI_SERVER_TOKEN "$RUNNER" \
		gitlab-runner register --non-interactive --url "$URL" --executor shell --name touchmark-e2e >/dev/null 2>&1 ||
		die "gitlab-runner register failed"
	docker exec "$RUNNER" sed -i 's/^concurrent = .*/concurrent = 4/' /etc/gitlab-runner/config.toml
	docker exec "$RUNNER" "$TOUCHMARK_BIN" version >/dev/null || die "the runner cannot run $TOUCHMARK_BIN"
	local deadline=$((SECONDS + 180)) status
	while :; do
		status=$(api "$root_token" GET "/runners/$runner_id" | json_str status)
		[ "$status" = online ] && break
		((SECONDS < deadline)) || die "the runner is $status after 180s, want online"
		sleep 3
	done
	log "runner $runner_id online: $img, shell executor, $TOUCHMARK_BIN"
}

# start_template sets up what --template needs: the group of hubs with the
# person as an Owner, the touchmark image in a local registry, the
# forwarder to GitLab and the image runner, a group runner of the hubs
# with the Docker executor.
start_template() {
	hub_group_id=$(create_group "$HUB_GROUP")
	[ -n "$hub_group_id" ] || die "cannot create the group $HUB_GROUP"
	add_member "$hub_group_id" "$person_id" "$PERSON_LEVEL"
	tpl_image "$suffix"
	tpl_loopback "$suffix" "$GITLAB" "$net" 80

	local out id img
	out=$(api "$root_token" POST /user/runners "{\"runner_type\":\"group_type\",\"group_id\":$hub_group_id,\"run_untagged\":true,\"description\":\"touchmark-e2e-image\"}") ||
		die "cannot create the image runner"
	id=$(printf '%s' "$out" | json_num id)
	image_runner_token=$(printf '%s' "$out" | json_str token)
	[ -n "$id" ] && [ -n "$image_runner_token" ] || die "POST /user/runners answered without an id or token"
	img=$(runner_image)
	started_image_runner=1
	# The Docker socket: the Docker executor starts the job containers on
	# this machine's daemon.
	docker run -d --name "$IMAGE_RUNNER" --network "container:$GITLAB" \
		-v /var/run/docker.sock:/var/run/docker.sock "$img" >/dev/null
	# Network mode host: the jobs reach GitLab at http://localhost through
	# the forwarder. No cache volumes, so a job leaves nothing behind.
	CI_SERVER_TOKEN=$image_runner_token docker exec -e CI_SERVER_TOKEN "$IMAGE_RUNNER" \
		gitlab-runner register --non-interactive --url "$URL" --executor docker --name touchmark-e2e-image \
		--docker-image "$tpl_image_ref" --docker-network-mode host --docker-disable-cache \
		--docker-pull-policy always >/dev/null 2>&1 ||
		die "gitlab-runner register (Docker executor) failed"
	docker exec "$IMAGE_RUNNER" sed -i 's/^concurrent = .*/concurrent = 3/' /etc/gitlab-runner/config.toml
	local deadline=$((SECONDS + 180)) status
	while :; do
		status=$(api "$root_token" GET "/runners/$id" | json_str status)
		[ "$status" = online ] && break
		((SECONDS < deadline)) || die "the image runner is $status after 180s, want online"
		sleep 3
	done
	log "image runner $id online: group $HUB_GROUP, Docker executor, network mode host, default image $tpl_image_ref"
}

# test_vars prints the variables the tests read as NAME=value lines,
# the tokens included.
test_vars() {
	printf 'TOUCHMARK_E2E_GITLAB_URL=%s\n' "$URL"
	printf 'TOUCHMARK_E2E_GITLAB_IMAGE=%s\n' "$image"
	printf 'TOUCHMARK_E2E_GITLAB_GROUP=%s\n' "$GROUP"
	printf 'TOUCHMARK_E2E_GITLAB_SUBGROUP=%s\n' "$GROUP/$SUBGROUP"
	printf 'TOUCHMARK_E2E_GITLAB_ACCOUNTS=%s\n' "$accounts"
	printf 'TOUCHMARK_E2E_GITLAB_RUNNER_BIN=%s\n' "$TOUCHMARK_BIN"
	if [ -n "$template" ]; then
		printf 'TOUCHMARK_E2E_GITLAB_TEMPLATE=/template\n'
		printf 'TOUCHMARK_E2E_GITLAB_HUB_GROUP=%s\n' "$HUB_GROUP"
		printf 'TOUCHMARK_E2E_GITLAB_TOUCHMARK_IMAGE=%s\n' "$tpl_image_ref"
	fi
	printf 'TOUCHMARK_E2E_GITLAB_REQUIRED=1\n'
	printf 'TOUCHMARK_E2E_GITLAB_ROOT_LOGIN=%s\n' "$ROOT"
	printf 'TOUCHMARK_E2E_GITLAB_READER_LOGIN=%s\n' "$reader_login"
	printf 'TOUCHMARK_E2E_GITLAB_WRITER_LOGIN=%s\n' "$writer_login"
	printf 'TOUCHMARK_E2E_GITLAB_PERSON_LOGIN=%s\n' "$PERSON"
	printf 'TOUCHMARK_E2E_GITLAB_ROOT_TOKEN=%s\n' "$root_token"
	printf 'TOUCHMARK_E2E_GITLAB_READER_TOKEN=%s\n' "$reader_token"
	printf 'TOUCHMARK_E2E_GITLAB_WRITER_TOKEN=%s\n' "$writer_token"
	printf 'TOUCHMARK_E2E_GITLAB_PERSON_TOKEN=%s\n' "$person_token"
}

write_test_env() {
	test_env=$(private_file)
	test_vars >"$test_env"
	{
		printf 'TOUCHMARK_E2E_GITLAB_RUN=%s\n' "$run"
		printf 'TOUCHMARK_E2E_TEST_TIMEOUT=%s\n' "$TEST_TIMEOUT"
		printf 'GOTOOLCHAIN=local\nGOFLAGS=-mod=readonly\n'
	} >>"$test_env"
}

run_tests() {
	local -a proxy=()
	local name
	for name in GOPROXY GONOSUMDB GOPRIVATE HTTPS_PROXY HTTP_PROXY NO_PROXY; do
		if [ -n "${!name-}" ]; then
			proxy+=(-e "$name")
		fi
	done
	local -a mounts=(-v "$(host_path "$REPO"):/src:ro")
	if [ -n "$template" ]; then
		mounts+=(-v "$(host_path "$template"):/template:ro")
	fi
	gotest=touchmark-e2e-$suffix-go
	docker run --rm --name "$gotest" --network "container:$GITLAB" --env-file "$(host_path "$test_env")" \
		${proxy[@]+"${proxy[@]}"} \
		"${mounts[@]}" -w /src \
		"$GO_IMAGE" sh -c 'git config --global --add safe.directory /src &&
			exec go test -tags e2e -count=1 -race -v -timeout "$TOUCHMARK_E2E_TEST_TIMEOUT" \
				${TOUCHMARK_E2E_GITLAB_RUN:+-run "$TOUCHMARK_E2E_GITLAB_RUN"} ./internal/e2e/gitlab/...'
}

# run_image runs the tests against one image; its trap removes what it
# created.
run_image() {
	case $image in
	gitlab/gitlab-ce:* | */gitlab/gitlab-ce:*) ;;
	*) die "not a GitLab CE image: $image" ;;
	esac
	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	suffix=$(rand_hex 4)
	for img in "$image" "$GO_IMAGE"; do
		if ! docker image inspect "$img" >/dev/null 2>&1; then
			docker pull -q "$img" >/dev/null
		fi
	done
	net=touchmark-e2e-$suffix
	docker network create "$net" >/dev/null
	log "$image: network $net"
	local started=$SECONDS
	start_gitlab
	build_touchmark
	wait_ready
	log "ready after $((SECONDS - started))s"
	# The password omnibus generated for root: nothing uses it.
	docker exec "$GITLAB" rm -f /etc/gitlab/initial_root_password
	seed
	verify_seed
	log "version: $(api "$root_token" GET /version)"
	log "seeded ($accounts): $reader_login (Reporter, read_api read_repository), $writer_login (Developer, api write_repository), $PERSON (Owner) in $GROUP and $GROUP/$SUBGROUP"
	start_runner
	if [ -n "$template" ]; then
		start_template
	fi
	write_test_env
	if [ "$only_seed" = 1 ]; then
		log "--only-seed: seeded and checked; no tests"
		return
	fi
	run_tests
}

parse_args() {
	while [ $# -gt 0 ]; do
		case $1 in
		--keep) keep=1 ;;
		--only-seed) only_seed=1 ;;
		--run)
			[ $# -ge 2 ] || die "--run needs a regular expression"
			run=$2
			shift
			;;
		--run=*) run=${1#--run=} ;;
		--template)
			[ $# -ge 2 ] || die "--template needs the template's directory"
			template=$2
			shift
			;;
		--template=*) template=${1#--template=} ;;
		--touchmark-image)
			[ $# -ge 2 ] || die "--touchmark-image needs an image"
			tpl_source=$2
			shift
			;;
		--touchmark-image=*) tpl_source=${1#--touchmark-image=} ;;
		-h | --help)
			usage
			exit 0
			;;
		-*) die "unknown flag $1 (see --help)" ;;
		*)
			[ -z "$image" ] || die "one IMAGE or all, not several"
			image=$1
			;;
		esac
		shift
	done
	[ -n "$image" ] || {
		usage >&2
		exit 2
	}
	if [ -n "$tpl_source" ] && [ -z "$template" ]; then
		die "--touchmark-image works with --template only"
	fi
	if [ -n "$template" ]; then
		[ -f "$template/.gitlab-ci.yml" ] && [ -f "$template/hub.yml" ] ||
			die "--template $template: not a hub template (no .gitlab-ci.yml and hub.yml)"
		template=$(cd "$template" && pwd)
	fi
}

main() {
	parse_args "$@"
	command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
	docker info >/dev/null 2>&1 || die "the Docker daemon does not answer"
	if [ "$image" != all ]; then
		run_image
		return
	fi
	[ "$keep" = 0 ] || die "--keep keeps one GitLab: name one IMAGE, not all"
	local -a flags=() failed=()
	local img
	if [ "$only_seed" = 1 ]; then flags+=(--only-seed); fi
	if [ -n "$run" ]; then flags+=(--run "$run"); fi
	if [ -n "$template" ]; then flags+=(--template "$template"); fi
	if [ -n "$tpl_source" ]; then flags+=(--touchmark-image "$tpl_source"); fi
	for img in "${SUPPORTED_IMAGES[@]}"; do
		if ! "${BASH:-bash}" "$SCRIPT" ${flags[@]+"${flags[@]}"} "$img"; then
			failed+=("$img")
		fi
	done
	if [ ${#failed[@]} -gt 0 ]; then
		die "failed: ${failed[*]}"
	fi
	log "passed: ${SUPPORTED_IMAGES[*]}"
}

main "$@"
