#!/usr/bin/env bash
# gitea.sh runs the live end-to-end tests of touchmark (package internal/e2e,
# build tag e2e) against Gitea or Forgejo in Docker: locally (Git Bash on
# Windows, Linux, macOS) and in CI. docs/project/e2e.md explains what they cover.
#
# Usage: scripts/e2e/gitea.sh [--keep] [--run REGEXP] [--require-signin]
#        [--template DIR [--touchmark-image REF]] IMAGE|all
#
#   IMAGE       a Gitea or Forgejo image, told apart by its name:
#               docker.gitea.com/gitea:1.27.3, codeberg.org/forgejo/forgejo:16.0.5, ...
#   all         the four supported images, one after the other
#   --keep      keep the forge, its network and an env file with the tokens
#               for debugging, and publish the forge on a loopback port; the
#               script prints how to reach and remove them
#   --run RE    run only the tests that match RE (go test -run)
#   --require-signin
#               start the forge with [service] REQUIRE_SIGNIN_VIEW = true,
#               as many company instances run; TestVisibility then expects
#               no repository to be public (the other tests assume the
#               default and may fail)
#   --template DIR
#               also run the hub template's .gitea workflows, from the
#               template's working tree DIR (TestTemplate; without it the
#               test skips), with Gitea's runner: Gitea images only. The
#               touchmark image is built from this working tree and pushed
#               to a local registry (scripts/e2e/template-lib.sh)
#   --touchmark-image REF
#               with --template: run the jobs in this image instead of one
#               built from the working tree, such as a release candidate
#
# For each image the script creates the network touchmark-e2e-<rand> and
# starts the forge in it as touchmark-e2e-<rand>-forge (network alias
# touchmark-e2e-forge), with SQLite on a tmpfs, and waits for /api/healthz
# (at most 180 s). It seeds four accounts through the forge's command line,
# each with an access token of the scopes that
# docs/getting-started/gitea-forgejo.md names: an admin, which only the
# tests' fixtures use, a reader bot, a writer bot and a person; then the
# organisation acme, where the team "readers" gives the reader read
# access, the team "writers" the writer write access, and the person is an
# owner. With --template it also creates the organisation hubs, of which
# the person is an owner, builds the touchmark image (or takes
# --touchmark-image), pushes it to a registry on 127.0.0.1, and starts
# Gitea's runner (RUNNER_IMAGE below, as touchmark-e2e-<rand>-actions,
# registered for the instance with a token from `gitea actions
# generate-runner-token`) with the Docker socket: it runs each job in the
# container the job names, in the Docker host's network namespace, where a
# forwarder on 127.0.0.1:3000 and [::1]:3000 leads to the forge, so that
# the jobs reach it at http://localhost:3000 too; jobs get no Docker socket
# and no cache server. The job containers are the runner's; the cleanup
# removes any it left. It checks the seeding through the API and runs
#
#   go test -tags e2e -count=1 -race -v ./internal/e2e ./internal/e2e/gitlab
#
# (internal/e2e/gitlab skips without its GitLab; internal/e2e/github needs
# no server and runs in the ordinary unit suite, so it is left out) in
# golang:1.27 inside the forge container's network namespace. touchmark
# sends a credential over plain http only to loopback
# (docs/project/threat-model.md, "Tokens and git"), so the tests reach the
# forge at http://localhost:3000, which is also its ROOT_URL: clone URLs in
# API answers are the same URL.
#
# Tokens never appear on a command line: curl reads its Authorization header
# from standard input, and the test container gets them through the
# environment of the docker command (-e NAME without a value). Only --keep
# writes them to a file, for running the tests again: created with umask 077
# and, under Git Bash or Cygwin, where the mode does not reach NTFS, cut down
# with icacls to the current user alone. Containers, the network and the
# file are removed on exit, also after a failure or Ctrl-C, unless --keep.

set -euo pipefail

# Git Bash on Windows rewrites arguments that look like POSIX paths (/src,
# /data, ...) for native programs such as docker.exe. Host paths are
# converted explicitly with host_path instead.
export MSYS_NO_PATHCONV=1

readonly SUPPORTED_IMAGES=(
	docker.gitea.com/gitea:1.26.4
	docker.gitea.com/gitea:1.27.3
	codeberg.org/forgejo/forgejo:15.0.9
	codeberg.org/forgejo/forgejo:16.0.5
)
readonly GO_IMAGE=${TOUCHMARK_E2E_GO_IMAGE:-golang:1.27}
# Gitea's runner for --template (gitea.com/gitea/runner, formerly
# act_runner), pinned by digest.
readonly RUNNER_IMAGE=docker.gitea.com/runner:4.1.0@sha256:131ff842ec12453108f1c1a454031c325d3c95e4af459979a2e303e12d99c98c
readonly HUB_ORG=hubs
readonly TEST_TIMEOUT=${TOUCHMARK_E2E_TEST_TIMEOUT:-25m}
readonly HEALTH_TIMEOUT=180
readonly PORT=3000
readonly URL=http://localhost:$PORT
readonly ALIAS=touchmark-e2e-forge
readonly ORG=acme
readonly ADMIN=touchmark-admin
readonly READER=touchmark-reader
readonly WRITER=touchmark-writer
readonly PERSON=jdoe
# The reader's and the writer's scopes are those of
# docs/getting-started/gitea-forgejo.md. The person acts as a member of a
# target's team; the admin prepares fixtures only.
readonly READER_SCOPES=read:repository,read:issue,read:organization,read:user
readonly WRITER_SCOPES=write:repository,write:issue,read:organization,read:user
readonly PERSON_SCOPES=write:repository,write:issue,read:user
readonly ADMIN_SCOPES=all

SCRIPT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
readonly SCRIPT REPO

keep=0
run=
image=
signin=0
template=
tpl_source=

usage() {
	sed -n '/^# Usage:/,/^# For each image/p' "$SCRIPT" | sed '$d' | sed 's/^# \{0,1\}//'
}

die() {
	echo "gitea.sh: $*" >&2
	exit 1
}

log() {
	echo "gitea.sh: $*" >&2
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
	od -An -N"${1:-4}" -tx1 /dev/urandom | tr -d ' \n'
}

# shellcheck source=scripts/e2e/template-lib.sh
. "$(dirname "$SCRIPT")/template-lib.sh"

# The resources of the image under test, removed by cleanup.
net=
forge=
runner=
envfile=
actions_runner=
# volumes_before are the names of the volumes of Gitea's runner jobs that
# existed before this run's runner started; the cleanup leaves them.
volumes_before=

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	if [ "$keep" = 1 ] && [ -n "$forge" ]; then
		print_kept
	else
		# -v: the images declare volumes (the forge's /data), which would
		# be left behind as anonymous volumes.
		if [ -n "$runner" ]; then docker rm -f -v "$runner" >/dev/null 2>&1 || true; fi
		if [ -n "$actions_runner" ]; then
			docker rm -f -v "$actions_runner" >/dev/null 2>&1 || true
			remove_job_leftovers
		fi
		if [ -n "$forge" ]; then docker rm -f -v "$forge" >/dev/null 2>&1 || true; fi
		tpl_cleanup
		if [ -n "$net" ]; then docker network rm "$net" >/dev/null 2>&1 || true; fi
		if [ -n "$envfile" ]; then rm -f -- "$envfile"; fi
	fi
	exit "$status"
}

# job_volumes lists the volumes Gitea's runner makes for jobs.
job_volumes() {
	docker volume ls -q --filter name=GITEA-ACTIONS-TASK- 2>/dev/null | sort || true
}

# remove_job_leftovers removes what the runner's jobs left when they were
# cut short: the containers of the image under test, and the job volumes
# that appeared during the run.
remove_job_leftovers() {
	local ids v
	if [ -n "$tpl_image_ref" ]; then
		ids=$(docker ps -aq --filter "ancestor=$tpl_image_ref" 2>/dev/null || true)
		if [ -n "$ids" ]; then
			# shellcheck disable=SC2086 # one id per word
			docker rm -f -v $ids >/dev/null 2>&1 || true
		fi
	fi
	for v in $(job_volumes); do
		case " $volumes_before " in
		*" $v "*) ;;
		*) docker volume rm -f "$v" >/dev/null 2>&1 || true ;;
		esac
	done
}

print_kept() {
	local published mount=""
	published=$(docker port "$forge" "$PORT/tcp" 2>/dev/null | head -n 1 || true)
	if [ -n "$template" ]; then
		mount=" -v '$(host_path "$template")':/template:ro"
	fi
	cat >&2 <<EOF
gitea.sh: kept for debugging (--keep):
  network    $net
  forge      $forge${published:+, published at http://$published}${template:+
  template   the runner $actions_runner, the registry $tpl_registry, the forwarder $tpl_forwarder}
  env file   $envfile (the tokens; readable by you alone)
  run the tests again:
    docker run --rm --network container:$forge --env-file '$(host_path "$envfile")' \\
      -v '$(host_path "$REPO")':/src:ro -w /src$mount $GO_IMAGE \\
      sh -c 'git config --global --add safe.directory /src && go test -tags e2e -count=1 -race -v ./internal/e2e ./internal/e2e/gitlab'
  remove everything:
    docker rm -f -v $forge${template:+ $actions_runner $tpl_registry $tpl_forwarder} && docker network rm $net && rm -f '$envfile'
EOF
}

# forge_cli runs the forge's own command line as the git user.
forge_cli() {
	docker exec -u git "$forge" "$bin" "$@"
}

# api TOKEN METHOD PATH [JSON] sends a request to the forge's API with curl
# inside the forge container, prints the response body and fails on a
# status other than 2xx. The token reaches curl on its standard input.
api() {
	local token=$1 method=$2 path=$3 body=${4-} out status
	local -a data=()
	if [ -n "$body" ]; then
		data=(--data-binary "$body")
	fi
	out=$(printf 'Authorization: token %s\nContent-Type: application/json\nAccept: application/json\n' "$token" |
		docker exec -i "$forge" curl -sS -H @- -X "$method" ${data[@]+"${data[@]}"} -w '\n%{http_code}' "$URL/api/v1$path") ||
		die "$method $path: curl failed"
	status=${out##*$'\n'}
	out=${out%$'\n'*}
	case $status in
	2??) printf '%s' "$out" ;;
	*)
		echo "gitea.sh: $method $path: HTTP $status: $out" >&2
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
	printf 'Authorization: token %s\nContent-Type: application/json\n' "$token" |
		docker exec -i "$forge" curl -sS -H @- -X "$method" ${data[@]+"${data[@]}"} -o /dev/null -w '%{http_code}' "$URL/api/v1$path"
}

# json_id prints the id of a JSON object whose first field is "id", as both
# forges encode their API structs (Go's encoding/json keeps field order).
json_id() {
	sed -n 's/^{"id":\([0-9][0-9]*\),.*/\1/p'
}

create_user() {
	local login=$1 kind=$2
	local -a args=(admin user create --username "$login" --email "$login@example.com")
	case $kind in
	admin) args+=(--admin --random-password --must-change-password=false) ;;
	bot)
		# Gitea has a bot user type (no password, no web login; 1.27 refuses
		# a password for it); Forgejo has none, so its bots are ordinary
		# users.
		if [ "$flavor" = gitea ]; then
			args+=(--user-type bot)
		else
			args+=(--random-password --must-change-password=false)
		fi
		;;
	person) args+=(--random-password --must-change-password=false) ;;
	esac
	# Standard output carries the random password: dropped.
	if ! forge_cli "${args[@]}" >/dev/null; then
		# An older Gitea may want a password for a bot too. die runs when
		# there is no retry and when the retry fails, both meant.
		# shellcheck disable=SC2015
		[ "$kind" = bot ] && [ "$flavor" = gitea ] && forge_cli "${args[@]}" --random-password >/dev/null ||
			die "cannot create the user $login"
	fi
	# Gitea 1.26 makes a new bot change its password first, and the API
	# then answers 403 to its token: unset the flag for every account.
	forge_cli admin user must-change-password --unset "$login" >/dev/null ||
		die "cannot unset must-change-password for $login"
}

mint_token() {
	local login=$1 scopes=$2 token
	token=$(forge_cli admin user generate-access-token --username "$login" --token-name touchmark-e2e --scopes "$scopes" --raw) ||
		die "cannot mint a token for $login"
	token=$(printf '%s' "$token" | tr -d '\r\n')
	[[ $token =~ ^[0-9a-f]{40}$ ]] || die "the token minted for $login is not 40 hex digits"
	printf '%s' "$token"
}

wait_healthy() {
	local deadline=$((SECONDS + HEALTH_TIMEOUT))
	until docker exec "$forge" curl -fsS -o /dev/null "$URL/api/healthz" >/dev/null 2>&1; do
		if [ "$(docker inspect -f '{{.State.Running}}' "$forge" 2>/dev/null)" != true ]; then
			docker logs --tail 100 "$forge" >&2 || true
			die "the forge stopped before it became healthy"
		fi
		if ((SECONDS >= deadline)); then
			docker logs --tail 100 "$forge" >&2 || true
			die "the forge is not healthy after ${HEALTH_TIMEOUT}s"
		fi
		sleep 1
	done
}

seed() {
	create_user "$ADMIN" admin
	create_user "$READER" bot
	create_user "$WRITER" bot
	create_user "$PERSON" person
	admin_token=$(mint_token "$ADMIN" "$ADMIN_SCOPES")
	reader_token=$(mint_token "$READER" "$READER_SCOPES")
	writer_token=$(mint_token "$WRITER" "$WRITER_SCOPES")
	person_token=$(mint_token "$PERSON" "$PERSON_SCOPES")

	api "$admin_token" POST /orgs \
		"{\"username\":\"$ORG\",\"visibility\":\"private\",\"repo_admin_change_team_access\":false}" >/dev/null
	local units='"units":["repo.code","repo.issues","repo.pulls"]'
	readers_id=$(api "$admin_token" POST "/orgs/$ORG/teams" \
		"{\"name\":\"readers\",\"permission\":\"read\",\"includes_all_repositories\":true,\"can_create_org_repo\":false,$units,\"units_map\":{\"repo.code\":\"read\",\"repo.issues\":\"read\",\"repo.pulls\":\"read\"}}" |
		json_id)
	writers_id=$(api "$admin_token" POST "/orgs/$ORG/teams" \
		"{\"name\":\"writers\",\"permission\":\"write\",\"includes_all_repositories\":true,\"can_create_org_repo\":false,$units,\"units_map\":{\"repo.code\":\"write\",\"repo.issues\":\"write\",\"repo.pulls\":\"write\"}}" |
		json_id)
	owners_id=$(api "$admin_token" GET "/orgs/$ORG/teams" | { grep -o '"id":[0-9]*,"name":"Owners"' || true; } | sed 's/[^0-9]//g' | head -n 1)
	[ -n "$readers_id" ] && [ -n "$writers_id" ] && [ -n "$owners_id" ] || die "cannot read the ids of the teams of $ORG"
	api "$admin_token" PUT "/teams/$readers_id/members/$READER" >/dev/null
	api "$admin_token" PUT "/teams/$writers_id/members/$WRITER" >/dev/null
	api "$admin_token" PUT "/teams/$owners_id/members/$PERSON" >/dev/null
}

# verify_seed checks through the API what the tests rely on: each token
# acts as its account, only the admin is an admin, the teams hold the right
# members with the right access, and the reader's and the writer's tokens
# cannot create repositories.
verify_seed() {
	local pair login token user
	for pair in "$ADMIN:$admin_token" "$READER:$reader_token" "$WRITER:$writer_token" "$PERSON:$person_token"; do
		login=${pair%%:*}
		token=${pair#*:}
		user=$(api "$token" GET /user) || die "the token of $login does not work"
		case $user in *"\"login\":\"$login\""*) ;; *) die "the token of $login acts as someone else" ;; esac
		case $login:$user in
		"$ADMIN":*'"is_admin":true'*) ;;
		"$ADMIN":*) die "$ADMIN is not an admin" ;;
		*:*'"is_admin":true'*) die "$login is an admin" ;;
		esac
	done
	local members
	members=$(api "$admin_token" GET "/teams/$readers_id/members")
	case $members in *"\"login\":\"$READER\""*) ;; *) die "$READER is not in the team readers" ;; esac
	members=$(api "$admin_token" GET "/teams/$writers_id/members")
	case $members in *"\"login\":\"$WRITER\""*) ;; *) die "$WRITER is not in the team writers" ;; esac
	members=$(api "$admin_token" GET "/teams/$owners_id/members")
	case $members in *"\"login\":\"$PERSON\""*) ;; *) die "$PERSON is not an owner of $ORG" ;; esac
	local team
	team=$(api "$admin_token" GET "/teams/$readers_id")
	case $team in *'"repo.code":"read"'*) ;; *) die "the team readers does not read code: $team" ;; esac
	team=$(api "$admin_token" GET "/teams/$writers_id")
	case $team in *'"repo.pulls":"write"'*) ;; *) die "the team writers does not write pull requests: $team" ;; esac
	api "$reader_token" GET "/orgs/$ORG/repos" >/dev/null || die "the reader cannot list the repositories of $ORG"
	local status
	for pair in "$READER:$reader_token" "$WRITER:$writer_token"; do
		status=$(api_status "${pair#*:}" POST "/orgs/$ORG/repos" '{"name":"touchmark-e2e-forbidden"}')
		[ "$status" = 403 ] || die "${pair%%:*} may create a repository in $ORG (HTTP $status, want 403)"
	done
}

# start_template sets up what --template needs: the organisation of hubs
# with the person as an owner, the touchmark image in a local registry, the
# forwarder to the forge and Gitea's runner.
start_template() {
	api "$admin_token" POST /orgs "{\"username\":\"$HUB_ORG\",\"visibility\":\"private\",\"repo_admin_change_team_access\":false}" >/dev/null
	local owners
	owners=$(api "$admin_token" GET "/orgs/$HUB_ORG/teams" | { grep -o '"id":[0-9]*,"name":"Owners"' || true; } | sed 's/[^0-9]//g' | head -n 1)
	[ -n "$owners" ] || die "cannot read the Owners team of $HUB_ORG"
	api "$admin_token" PUT "/teams/$owners/members/$PERSON" >/dev/null
	tpl_image "$suffix"
	tpl_loopback "$suffix" "$forge" "$net" "$PORT"

	if ! docker image inspect "$RUNNER_IMAGE" >/dev/null 2>&1; then
		docker pull -q "$RUNNER_IMAGE" >/dev/null
	fi
	local token config
	token=$(forge_cli actions generate-runner-token | tr -d '\r\n')
	[ -n "$token" ] || die "gitea actions generate-runner-token printed no token"
	volumes_before=$(job_volumes | tr '\n' ' ')
	actions_runner=touchmark-e2e-$suffix-actions
	# The runner's configuration (no secret in it): jobs in the Docker
	# host's network namespace, without the Docker socket and without the
	# cache server; a job without a container runs in the image under test.
	config=$(mktemp "${TMPDIR:-/tmp}/touchmark-e2e-runner-XXXXXXXX")
	cat >"$config" <<EOF
log:
  level: info
runner:
  file: /data/.runner
  capacity: 2
  timeout: 1h
  labels:
    - "ubuntu-latest:docker://$tpl_image_ref"
cache:
  enabled: false
container:
  network: host
  docker_host: "-"
  valid_volumes: []
EOF
	# The token reaches the runner's registration through the environment
	# of docker create, named without a value on its command line.
	GITEA_RUNNER_REGISTRATION_TOKEN=$token docker create --name "$actions_runner" --network "container:$forge" \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-e GITEA_INSTANCE_URL="$URL" -e GITEA_RUNNER_REGISTRATION_TOKEN -e GITEA_RUNNER_NAME=touchmark-e2e \
		-e CONFIG_FILE=/etc/touchmark-e2e-runner.yaml -e GITEA_MAX_REG_ATTEMPTS=10 \
		"$RUNNER_IMAGE" >/dev/null
	docker cp "$(host_path "$config")" "$actions_runner:/etc/touchmark-e2e-runner.yaml" >/dev/null
	rm -f -- "$config"
	docker start "$actions_runner" >/dev/null
	tpl_wait_log "$actions_runner" "Runner registered successfully" 120
	tpl_wait_log "$actions_runner" "declare successfully" 60
	log "Gitea's runner online: $RUNNER_IMAGE, network host, jobs without a container in $tpl_image_ref"
}

# test_vars prints the variables the tests read, but the tokens, as
# NAME=value lines.
test_vars() {
	printf 'TOUCHMARK_E2E_URL=%s\n' "$URL"
	if [ -n "$template" ]; then
		printf 'TOUCHMARK_E2E_TEMPLATE=/template\n'
		printf 'TOUCHMARK_E2E_HUB_ORG=%s\n' "$HUB_ORG"
		printf 'TOUCHMARK_E2E_TOUCHMARK_IMAGE=%s\n' "$tpl_image_ref"
	fi
	printf 'TOUCHMARK_E2E_FLAVOR=%s\n' "$flavor"
	printf 'TOUCHMARK_E2E_IMAGE=%s\n' "$image"
	printf 'TOUCHMARK_E2E_ORG=%s\n' "$ORG"
	printf 'TOUCHMARK_E2E_REQUIRE_SIGNIN=%s\n' "$signin"
	printf 'TOUCHMARK_E2E_ADMIN_LOGIN=%s\n' "$ADMIN"
	printf 'TOUCHMARK_E2E_READER_LOGIN=%s\n' "$READER"
	printf 'TOUCHMARK_E2E_WRITER_LOGIN=%s\n' "$WRITER"
	printf 'TOUCHMARK_E2E_PERSON_LOGIN=%s\n' "$PERSON"
}

# write_envfile writes the variables and the tokens for --keep. umask 077
# makes the file the owner's alone on Linux and macOS; under Git Bash or
# Cygwin the mode does not reach NTFS, where the file would inherit the
# ACL of the temporary directory, so icacls replaces that with the current
# user alone.
write_envfile() {
	local old
	old=$(umask)
	umask 077
	envfile=$(mktemp "${TMPDIR:-/tmp}/touchmark-e2e-XXXXXXXX")
	umask "$old"
	chmod 600 "$envfile"
	case $(uname -s) in
	MINGW* | MSYS* | CYGWIN*)
		MSYS2_ARG_CONV_EXCL='*' icacls "$(cygpath -w "$envfile")" /inheritance:r /grant:r "${USERNAME:?}:(R,W,D)" >/dev/null ||
			die "cannot restrict the env file to $USERNAME"
		;;
	esac
	{
		test_vars
		printf 'TOUCHMARK_E2E_ADMIN_TOKEN=%s\n' "$admin_token"
		printf 'TOUCHMARK_E2E_READER_TOKEN=%s\n' "$reader_token"
		printf 'TOUCHMARK_E2E_WRITER_TOKEN=%s\n' "$writer_token"
		printf 'TOUCHMARK_E2E_PERSON_TOKEN=%s\n' "$person_token"
	} >"$envfile"
}

run_tests() {
	local -a proxy=()
	local name
	for name in GOPROXY GONOSUMDB GOPRIVATE HTTPS_PROXY HTTP_PROXY NO_PROXY; do
		if [ -n "${!name-}" ]; then
			proxy+=(-e "$name")
		fi
	done
	local -a vars=()
	local line
	while IFS= read -r line; do
		vars+=(-e "$line")
	done < <(test_vars)
	local -a mounts=(-v "$(host_path "$REPO"):/src:ro")
	if [ -n "$template" ]; then
		mounts+=(-v "$(host_path "$template"):/template:ro")
	fi
	runner=touchmark-e2e-$suffix-go
	# The tokens reach docker through its environment, named without a
	# value on its command line. TOUCHMARK_E2E_REQUIRED makes the tests
	# fail instead of skipping when they do not find the forge.
	TOUCHMARK_E2E_ADMIN_TOKEN=$admin_token TOUCHMARK_E2E_READER_TOKEN=$reader_token \
		TOUCHMARK_E2E_WRITER_TOKEN=$writer_token TOUCHMARK_E2E_PERSON_TOKEN=$person_token \
		docker run --rm --name "$runner" --network "container:$forge" \
		"${vars[@]}" -e TOUCHMARK_E2E_REQUIRED=1 \
		-e TOUCHMARK_E2E_ADMIN_TOKEN -e TOUCHMARK_E2E_READER_TOKEN \
		-e TOUCHMARK_E2E_WRITER_TOKEN -e TOUCHMARK_E2E_PERSON_TOKEN \
		-e GOTOOLCHAIN=local -e GOFLAGS=-mod=readonly \
		-e "TOUCHMARK_E2E_RUN=$run" -e "TOUCHMARK_E2E_TEST_TIMEOUT=$TEST_TIMEOUT" \
		${proxy[@]+"${proxy[@]}"} \
		"${mounts[@]}" -w /src \
		"$GO_IMAGE" sh -c 'git config --global --add safe.directory /src &&
			exec go test -tags e2e -count=1 -race -v -timeout "$TOUCHMARK_E2E_TEST_TIMEOUT" \
				${TOUCHMARK_E2E_RUN:+-run "$TOUCHMARK_E2E_RUN"} ./internal/e2e ./internal/e2e/gitlab'
}

# run_image runs the tests against one image; its trap removes what it
# created.
run_image() {
	local lower
	lower=$(printf '%s' "$image" | tr '[:upper:]' '[:lower:]')
	case $lower in
	*forgejo*) flavor=forgejo bin=forgejo prefix=FORGEJO ;;
	*gitea*) flavor=gitea bin=gitea prefix=GITEA ;;
	*) die "cannot tell Gitea from Forgejo by the image name $image" ;;
	esac
	if [ -n "$template" ] && [ "$flavor" != gitea ]; then
		log "$image: --template runs Gitea's runner, which this script sets up for Gitea only; TestTemplate skips here"
		template=
	fi
	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	suffix=$(rand_hex)
	log "$image ($flavor): network touchmark-e2e-$suffix"
	if ! docker image inspect "$image" >/dev/null 2>&1; then
		docker pull -q "$image" >/dev/null
	fi
	if ! docker image inspect "$GO_IMAGE" >/dev/null 2>&1; then
		docker pull -q "$GO_IMAGE" >/dev/null
	fi
	net=touchmark-e2e-$suffix
	docker network create "$net" >/dev/null
	forge=touchmark-e2e-$suffix-forge
	local -a publish=() settings=()
	if [ "$keep" = 1 ]; then
		publish=(-p "127.0.0.1::$PORT")
	fi
	if [ "$signin" = 1 ]; then
		settings=(-e "${prefix}__service__REQUIRE_SIGNIN_VIEW=true")
	fi
	local started=$SECONDS
	# The tmpfs keeps SQLite and the repositories in memory; exec lets git
	# run the forge's hooks from it.
	docker run -d --name "$forge" --network "$net" --network-alias "$ALIAS" \
		${publish[@]+"${publish[@]}"} \
		--tmpfs /data:rw,exec,mode=0755 \
		-e "${prefix}__security__INSTALL_LOCK=true" \
		-e "${prefix}__database__DB_TYPE=sqlite3" \
		-e "${prefix}__server__ROOT_URL=$URL/" \
		-e "${prefix}__server__HTTP_PORT=$PORT" \
		-e "${prefix}__server__DISABLE_SSH=true" \
		-e "${prefix}__server__OFFLINE_MODE=true" \
		-e "${prefix}__service__DISABLE_REGISTRATION=true" \
		-e "${prefix}__log__LEVEL=Warn" \
		${settings[@]+"${settings[@]}"} \
		"$image" >/dev/null
	wait_healthy
	log "healthy after $((SECONDS - started))s"
	seed
	verify_seed
	log "seeded $ADMIN (admin), $READER and $WRITER (bots), $PERSON (person); $ORG: readers (read) has $READER, writers (write) has $WRITER, Owners has $PERSON"
	# Asked with a token: with --require-signin the forge answers nobody
	# else.
	log "version: $(api "$admin_token" GET /version)"
	if [ "$flavor" = forgejo ]; then
		log "Forgejo: $(printf 'Authorization: token %s\n' "$admin_token" |
			docker exec -i "$forge" curl -sS -H @- "$URL/api/forgejo/v1/version")"
	fi
	if [ -n "$template" ]; then
		start_template
	fi
	if [ "$keep" = 1 ]; then
		write_envfile
	fi
	run_tests
}

parse_args() {
	while [ $# -gt 0 ]; do
		case $1 in
		--keep) keep=1 ;;
		--run)
			[ $# -ge 2 ] || die "--run needs a regular expression"
			run=$2
			shift
			;;
		--run=*) run=${1#--run=} ;;
		--require-signin) signin=1 ;;
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
		[ -d "$template/.gitea/workflows" ] && [ -f "$template/hub.yml" ] ||
			die "--template $template: not a hub template (no .gitea/workflows and hub.yml)"
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
	local -a flags=() failed=()
	local img
	if [ "$keep" = 1 ]; then flags+=(--keep); fi
	if [ -n "$run" ]; then flags+=(--run "$run"); fi
	if [ "$signin" = 1 ]; then flags+=(--require-signin); fi
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
