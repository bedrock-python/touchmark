#!/usr/bin/env bash
# run.sh is the step of the touchmark GitHub Action (action.yml). It runs
# touchmark in the image of the Action's release, with docker on the runner,
# as the runner's own user, after checking where the image comes from:
#
#   1. it resolves TM_IMAGE:TM_VERSION, the release's version tag, to a
#      digest (docker buildx imagetools inspect);
#   2. it checks that this digest has a build provenance attestation made by
#      bedrock-python/touchmark's publish workflow on master, on a
#      GitHub-hosted runner (gh attestation verify, the attestation read
#      from the registry: --bundle-from-oci);
#   3. it pulls the image by that digest, checks that the image is labelled
#      with the version, and runs it by that digest.
# A version tag that names an image the workflow did not build fails step 2;
# one moved to an older release's image fails step 3. Nothing runs from a
# tag.
#
# Why not a container action (runs.using: docker): GitHub runs a container
# action as the image's user and documents that it must be root, or the
# action cannot write to GITHUB_WORKSPACE (docs.github.com/en/actions/
# reference/workflows-and-actions/dockerfile-support, "USER"). The image runs
# as a non-root user (Dockerfile), and touchmark writes its report files to
# the workspace and appends to the step summary. Run as the runner's uid, it
# owns the checkout, so git trusts it, and it can write both. This also
# keeps the docker socket, which a container action gets mounted, out of the
# container, and passes only the environment touchmark reads.
#
# The inputs arrive as TM_* variables, never inside the script's text, and
# each becomes one fixed flag: there is no free-form argument. Usage errors,
# and an image that cannot be resolved, verified or pulled, exit 2, like
# touchmark's own usage errors.
#
#   TM_IMAGE        the image without a tag, from action.yml
#   TM_VERSION      the release's version, X.Y.Z, from action.yml
#   TM_COMMAND      check, plan, distribute, doctor or version
#   TM_HUB          the hub checkout, relative to the workspace (--hub)
#   TM_STRICT, TM_ALL, TM_COMMENT, TM_ASSUME_OPT_IN, TM_DRY_RUN
#                   true or false: --strict, --all, --comment,
#                   --assume-opt-in, --dry-run
#   TM_ONLY         --only, comma-separated [PROVIDER:]PATH references
#   TM_DEADLINE     --deadline, a duration such as 50m
#   TM_FORMAT       --format: text, json or markdown
#   TM_REPORT       --report, a file relative to the workspace
#   GH_TOKEN        for gh, which will not start without one (action.yml
#                   passes the job's token); it never reaches the container
#
# The container gets CI, every GITHUB_* and TOUCHMARK_* variable of the step
# and the proxy settings of a self-hosted runner, by name (docker reads the
# values from its own environment, so no value reaches a command line),
# HOME=/tmp, the workspace, the event payload (read-only) and the step
# summary, mounted at the same paths. Nothing else: no docker socket, no
# RUNNER_TEMP, no GH_TOKEN, no capabilities.

set -euo pipefail

# Who builds the image: the attestation must name this repository and this
# workflow, run on this ref. Constants, not inputs.
signer_repo=bedrock-python/touchmark
signer_workflow=github.com/bedrock-python/touchmark/.github/workflows/publish.yml
signer_ref=refs/heads/master

die() {
	echo "::error title=touchmark action::$*"
	exit 2
}

image=${TM_IMAGE:-}
version=${TM_VERSION:-}
if ! [[ $image =~ ^[a-z0-9][a-z0-9.:/_-]*/[a-z0-9._-]+$ ]]; then
	die "action.yml names no image, without a tag or digest (\"$image\")"
fi
if [ "$version" = 0.0.0 ]; then
	die "this version of the action belongs to no release (version 0.0.0): use a release, such as bedrock-python/touchmark@<commit> # vX.Y.Z"
fi
if ! [[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.]+)?$ ]]; then
	die "action.yml names no release version (\"$version\")"
fi
if [ "${RUNNER_OS:-}" != Linux ]; then
	die "the action runs the touchmark image with docker, so it needs a Linux runner; on ${RUNNER_OS:-this runner} install touchmark from the release archives instead"
fi
command -v docker >/dev/null 2>&1 || die "docker is not available on this runner"
command -v gh >/dev/null 2>&1 || die "gh (GitHub CLI) is not available on this runner: the action needs it to verify the image"
workspace=${GITHUB_WORKSPACE:-}
[ -n "$workspace" ] && [ -d "$workspace" ] || die "GITHUB_WORKSPACE is not a directory"
case $PWD/ in
"$workspace"/*) ;;
*) die "the step runs in $PWD, outside the workspace $workspace" ;;
esac

cmd=${TM_COMMAND:-}
case $cmd in
check | plan | distribute | doctor | version) ;;
*) die "input command must be check, plan, distribute, doctor or version, not \"$cmd\"" ;;
esac

# applies NAME COMMAND... fails unless the command is one of COMMAND...
applies() {
	local input=$1 c
	shift
	for c in "$@"; do
		[ "$c" = "$cmd" ] && return 0
	done
	die "input $input does not apply to $cmd (only to $*)"
}

# flag NAME VALUE COMMAND... reports whether the boolean input NAME is set,
# after checking it applies to the command.
flag() {
	local input=$1 value=$2
	shift 2
	case $value in
	false | '') return 1 ;;
	true) applies "$input" "$@" ;;
	*) die "input $input must be true or false, not \"$value\"" ;;
	esac
}

# relpath NAME VALUE checks a path relative to the workspace.
relpath() {
	local input=$1 value=$2
	if ! [[ $value =~ ^[A-Za-z0-9._/-]+$ ]] || [[ $value == /* ]] || [[ /$value/ == */../* ]]; then
		die "input $input must be a path inside the workspace, not \"$value\""
	fi
}

args=("$cmd")
if [ "$cmd" != version ]; then
	hub=${TM_HUB:-.}
	relpath hub "$hub"
	args+=(--hub "$hub")
fi
if flag strict "${TM_STRICT:-}" plan distribute doctor; then args+=(--strict); fi
if flag all "${TM_ALL:-}" plan; then args+=(--all); fi
if flag comment "${TM_COMMENT:-}" plan; then args+=(--comment); fi
if flag assume-opt-in "${TM_ASSUME_OPT_IN:-}" plan; then args+=(--assume-opt-in); fi
if flag dry-run "${TM_DRY_RUN:-}" distribute; then args+=(--dry-run); fi
if [ -n "${TM_ONLY:-}" ]; then
	applies only plan distribute doctor
	[[ $TM_ONLY =~ ^[A-Za-z0-9._/:,@+-]+$ ]] || die "input only must be comma-separated [PROVIDER:]PATH references, not \"$TM_ONLY\""
	args+=(--only "$TM_ONLY")
fi
if [ -n "${TM_DEADLINE:-}" ]; then
	applies deadline distribute
	[[ $TM_DEADLINE =~ ^[0-9][0-9a-z.]*$ ]] || die "input deadline must be a duration such as 50m, not \"$TM_DEADLINE\""
	args+=(--deadline "$TM_DEADLINE")
fi
if [ -n "${TM_FORMAT:-}" ]; then
	applies format check plan distribute doctor
	case $TM_FORMAT in
	text | json | markdown) ;;
	*) die "input format must be text, json or markdown, not \"$TM_FORMAT\"" ;;
	esac
	args+=(--format "$TM_FORMAT")
fi
if [ -n "${TM_REPORT:-}" ]; then
	applies report distribute doctor
	relpath report "$TM_REPORT"
	args+=(--report "$TM_REPORT")
fi

opts=(--rm --init --user "$(id -u):$(id -g)" --cap-drop ALL --security-opt no-new-privileges
	--env HOME=/tmp --volume "$workspace:$workspace" --workdir "$PWD")
if [ -f "${GITHUB_EVENT_PATH:-}" ]; then
	opts+=(--volume "$GITHUB_EVENT_PATH:$GITHUB_EVENT_PATH:ro")
fi
if [ -f "${GITHUB_STEP_SUMMARY:-}" ]; then
	opts+=(--volume "$GITHUB_STEP_SUMMARY:$GITHUB_STEP_SUMMARY")
fi
while IFS= read -r name; do
	case $name in
	TM_*) ;;
	CI | GITHUB_* | TOUCHMARK_* | HTTP_PROXY | HTTPS_PROXY | NO_PROXY | http_proxy | https_proxy | no_proxy) opts+=(--env "$name") ;;
	esac
done < <(compgen -e | LC_ALL=C sort)

# 1. The version tag, resolved once: everything after this uses the digest.
# One retry for each network step.
tagged=$image:$version
resolve() {
	docker buildx imagetools inspect "$tagged" --format '{{ .Manifest.Digest }}' 2>&1
}
out=$(resolve) || out=$(resolve) || die "cannot resolve $tagged to a digest: ${out//$'\n'/ }"
# The digest is the one line of the output that is one; a warning on
# another line is not.
digest=$(printf '%s\n' "$out" | tr -d '\r' | grep -xE 'sha256:[0-9a-f]{64}' || true)
[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || die "cannot resolve $tagged to a digest: ${out//$'\n'/ }"
ref=$image@$digest

# 2. Its build provenance, from the registry. GH_HOST is dropped so that a
# job's own setting cannot point gh at another host.
verify() {
	env -u GH_HOST gh attestation verify "oci://$ref" --bundle-from-oci \
		--repo "$signer_repo" --signer-workflow "$signer_workflow" --source-ref "$signer_ref" \
		--deny-self-hosted-runners 2>&1
}
out=$(verify) || out=$(verify) ||
	die "$ref ($tagged) has no build provenance attestation from $signer_workflow on $signer_ref: ${out//$'\n'/ }"

# 3. The image itself, by digest, labelled with the version it was tagged as.
docker pull --quiet "$ref" >/dev/null || docker pull --quiet "$ref" >/dev/null || die "cannot pull $ref"
label=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$ref") ||
	die "cannot inspect $ref"
[ "$label" = "v$version" ] || die "$ref ($tagged) is the image of \"$label\", not of v$version"
echo "touchmark action: $tagged is $ref, built by $signer_workflow"
exec docker run "${opts[@]}" "$ref" "${args[@]}"
