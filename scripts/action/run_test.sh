#!/usr/bin/env bash
# run_test.sh tests scripts/action/run.sh, the step of the GitHub Action,
# with a fake docker and a fake gh that record their arguments: the inputs
# it accepts and the flags they become, the inputs it refuses (exit 2, no
# docker call), the image it runs (the version tag resolved to a digest,
# that digest's attestation verified, its version label checked, then run
# by digest), what stops it at each of those steps, and the environment and
# mounts the container gets. CI runs it (job release-snapshot in
# .github/workflows/ci.yml); it also runs under Git Bash.
#
# Usage: scripts/action/run_test.sh

set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
run=$here/run.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/bin" "$tmp/ws/hub"
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
# The fake docker: records each call, one argument per line, and in a file
# of its own the values of the variables the call passes with --env NAME.
# imagetools inspect prints $FAKE/digest (or fails when it has none), image
# inspect prints $FAKE/label.
{
	echo "== docker"
	printf '%s\n' "$@"
} >>"$FAKE_LOG"
prev=
for a in "$@"; do
	if [ "$prev" = --env ] && [[ $a != *=* ]]; then
		echo "$a=${!a-<unset>}" >>"$FAKE_LOG.env"
	fi
	prev=$a
done
case "$1 $2 $3" in
"buildx imagetools inspect")
	[ -f "$FAKE/digest" ] || { echo "ERROR: $4: not found" >&2; exit 1; }
	printf '%s' "$(cat "$FAKE/digest")"
	;;
"image inspect "*) cat "$FAKE/label" ;;
esac
exit 0
EOF
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
# The fake gh: records each call, whether it had a token and a host, and
# exits with $FAKE/gh-exit.
{
	echo "== gh GH_TOKEN=${GH_TOKEN:+set} GH_HOST=${GH_HOST-<unset>}"
	printf '%s\n' "$@"
} >>"$FAKE_LOG"
code=$(cat "$FAKE/gh-exit")
[ "$code" = 0 ] || echo "Error: verifying with issuer \"sigstore.dev\"" >&2
exit "$code"
EOF
chmod +x "$tmp/bin/docker" "$tmp/bin/gh"
: >"$tmp/event.json"
: >"$tmp/summary.md"

image=ghcr.io/bedrock-python/touchmark
digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
failures=0

# step [VAR=VALUE...]: runs run.sh in the fake workspace with a clean set of
# TM_* inputs plus VAR=VALUE, against a good release 0.1.0 unless the fakes
# are changed first; leaves the log in $log and the code in $code.
step() {
	log=$tmp/calls.log
	: >"$log"
	: >"$log.env"
	code=0
	(
		cd "$tmp/ws"
		for v in $(compgen -e); do
			case $v in TM_* | TOUCHMARK_* | GITHUB_* | GH_* | CI | [Hh][Tt][Tt][Pp]*_[Pp][Rr][Oo][Xx][Yy] | [Nn][Oo]_[Pp][Rr][Oo][Xx][Yy]) unset "$v" ;; esac
		done
		export PATH="$tmp/bin:$PATH" FAKE=$tmp/fake FAKE_LOG=$log RUNNER_OS=Linux
		export GITHUB_WORKSPACE=$tmp/ws GITHUB_EVENT_PATH=$tmp/event.json GITHUB_STEP_SUMMARY=$tmp/summary.md
		export GITHUB_ACTIONS=true CI=true TM_IMAGE=$image TM_VERSION=0.1.0 GH_TOKEN=gh-token-canary
		for kv in "$@"; do
			export "${kv%%=*}=${kv#*=}"
		done
		bash "$run"
	) >"$tmp/out" 2>&1 || code=$?
}

# good puts the fakes back to a good release: the tag resolves to $digest,
# its attestation verifies, its label is v0.1.0.
good() {
	rm -rf "$tmp/fake"
	mkdir -p "$tmp/fake"
	printf '%s\n' "$digest" >"$tmp/fake/digest"
	echo v0.1.0 >"$tmp/fake/label"
	echo 0 >"$tmp/fake/gh-exit"
}

fail() {
	echo "FAIL: $*"
	sed 's/^/    /' "$tmp/out" "$log"
	failures=$((failures + 1))
}

# run_args prints the arguments of the docker run call after the image.
run_args() {
	awk -v ref="$image@$digest" '
		/^== / { block = $0; first = 1; inrun = 0; seen = 0; next }
		first { first = 0; inrun = (block == "== docker" && $0 == "run"); next }
		inrun && seen { print }
		inrun && !seen && $0 == ref { seen = 1 }
	' "$log"
}

# calls prints the calls, one line each: the program and its first words.
calls() {
	awk '/^== / { if (line != "") print line; line = $2; n = 0; next } n < 3 { line = line " " $0; n++ } END { if (line != "") print line }' "$log"
}

# expect_args WANT VAR=VALUE...: the step succeeds and runs the image with
# exactly WANT (space-separated) as touchmark's arguments.
expect_args() {
	local want=$1
	shift
	good
	step "$@"
	local got
	got=$(run_args | tr '\n' ' ' | sed 's/ $//')
	if [ "$code" != 0 ] || [ "$got" != "$want" ]; then
		fail "$* -> exit $code, args \"$got\", want \"$want\""
	fi
}

# expect_refused PATTERN VAR=VALUE...: the step exits 2, prints PATTERN in
# an ::error command, and calls neither docker nor gh.
expect_refused() {
	local pattern=$1
	shift
	good
	step "$@"
	if [ "$code" != 2 ] || ! grep -q "^::error title=touchmark action::.*$pattern" "$tmp/out" || [ -s "$log" ]; then
		fail "$* -> exit $code, want 2 with \"$pattern\" and no docker or gh call"
	fi
}

# expect_stopped PATTERN LAST: the step, with the fakes as the caller set
# them, exits 2 with PATTERN, and its last call is LAST (as calls prints
# it): nothing runs.
expect_stopped() {
	local pattern=$1 last=$2
	step TM_COMMAND=plan
	local got
	got=$(calls | tail -n 1)
	if [ "$code" != 2 ] || ! grep -q "^::error title=touchmark action::.*$pattern" "$tmp/out" ||
		[ "$got" != "$last" ] || run_args | grep -q .; then
		fail "stopped at \"$last\" -> exit $code, last call \"$got\", want 2 with \"$pattern\" and no run"
	fi
}

expect_args "version" TM_COMMAND=version
expect_args "check --hub ." TM_COMMAND=check
expect_args "check --hub hub --format json" TM_COMMAND=check TM_HUB=hub TM_FORMAT=json
expect_args "plan --hub . --strict --all --comment --assume-opt-in --only gh:acme/api,acme/web --format markdown" \
	TM_COMMAND=plan TM_STRICT=true TM_ALL=true TM_COMMENT=true TM_ASSUME_OPT_IN=true \
	TM_ONLY=gh:acme/api,acme/web TM_FORMAT=markdown
expect_args "plan --hub ." TM_COMMAND=plan TM_STRICT=false TM_ALL=false TM_COMMENT= TM_DRY_RUN=false
expect_args "distribute --hub . --strict --dry-run --deadline 50m --report out/report.json" \
	TM_COMMAND=distribute TM_STRICT=true TM_DRY_RUN=true TM_DEADLINE=50m TM_REPORT=out/report.json
expect_args "doctor --hub ./hub --strict --only corp:platform/api --report doctor.json" \
	TM_COMMAND=doctor TM_HUB=./hub TM_STRICT=true TM_ONLY=corp:platform/api TM_REPORT=doctor.json

expect_refused "belongs to no release" TM_COMMAND=plan TM_VERSION=0.0.0
expect_refused "names no release version" TM_COMMAND=plan TM_VERSION=
expect_refused "names no release version" TM_COMMAND=plan TM_VERSION=latest
expect_refused "names no release version" TM_COMMAND=plan TM_VERSION=v0.1.0
expect_refused "names no release version" TM_COMMAND=plan TM_VERSION=0.1
expect_refused "names no release version" TM_COMMAND=plan "TM_VERSION=0.1.0 --all"
expect_refused "names no image" TM_COMMAND=plan TM_IMAGE=ghcr.io/bedrock-python/touchmark:0.1.0
expect_refused "names no image" TM_COMMAND=plan "TM_IMAGE=ghcr.io/bedrock-python/touchmark@$digest"
expect_refused "names no image" TM_COMMAND=plan TM_IMAGE=touchmark
expect_refused "needs a Linux runner" TM_COMMAND=plan RUNNER_OS=Windows
expect_refused "input command must be" TM_COMMAND=apply
expect_refused "input command must be" "TM_COMMAND=plan --all"
expect_refused "input command must be" TM_COMMAND=
expect_refused "input all does not apply to distribute" TM_COMMAND=distribute TM_ALL=true
expect_refused "input dry-run does not apply to plan" TM_COMMAND=plan TM_DRY_RUN=true
expect_refused "input deadline does not apply to plan" TM_COMMAND=plan TM_DEADLINE=5m
expect_refused "input report does not apply to plan" TM_COMMAND=plan TM_REPORT=r.json
expect_refused "input strict must be true or false" TM_COMMAND=plan TM_STRICT=yes
expect_refused "input only must be" TM_COMMAND=plan "TM_ONLY=acme/api --recreate x"
expect_refused "input deadline must be" TM_COMMAND=distribute "TM_DEADLINE=5m --allow-stale"
expect_refused "input format must be" TM_COMMAND=plan TM_FORMAT=yaml
expect_refused "input hub must be a path inside the workspace" TM_COMMAND=plan TM_HUB=/etc
expect_refused "input hub must be a path inside the workspace" TM_COMMAND=plan TM_HUB=../other
expect_refused "input hub must be a path inside the workspace" TM_COMMAND=plan "TM_HUB=a b"
expect_refused "input report must be a path inside the workspace" TM_COMMAND=doctor TM_REPORT=a/../../x

# The image: the version tag resolved once, the digest verified, pulled,
# its label read and run, in that order, each by the digest; gh with the
# signer's policy, a token and no GH_HOST, even when the job sets one.
good
step TM_COMMAND=plan GH_HOST=ghes.example.invalid
want="docker buildx imagetools inspect
gh attestation verify oci://$image@$digest
docker pull --quiet $image@$digest
docker image inspect --format
docker run --rm --init"
if [ "$code" != 0 ] || [ "$(calls)" != "$want" ]; then
	fail "the calls of a good release -> exit $code:
$(calls)"
fi
grep -qx "$image:0.1.0" "$log" || fail "imagetools inspect does not resolve $image:0.1.0"
policy="--bundle-from-oci --repo bedrock-python/touchmark --signer-workflow github.com/bedrock-python/touchmark/.github/workflows/publish.yml --source-ref refs/heads/master --deny-self-hosted-runners"
got=$(awk '/^== gh/ { ingh = 1; n = 0; next } /^== / { ingh = 0 } ingh && ++n > 3 { printf "%s ", $0 }' "$log" | sed 's/ $//')
[ "$got" = "$policy" ] || fail "gh attestation verify policy: \"$got\", want \"$policy\""
grep -qx "== gh GH_TOKEN=set GH_HOST=<unset>" "$log" || fail "gh runs without a token, or with GH_HOST"
[ "$(grep -c "^$image@$digest\$" "$log")" = 3 ] || fail "pull, image inspect and run do not all name the digest"
grep -q "^::error" "$tmp/out" && fail "a good release prints an error"

# A warning next to the digest changes nothing.
good
printf 'WARNING: a warning of buildx\n%s\n' "$digest" >"$tmp/fake/digest"
step TM_COMMAND=plan
if [ "$code" != 0 ] || ! run_args | grep -q .; then
	fail "a warning next to the digest -> exit $code"
fi

# What stops it, and where.
good
rm "$tmp/fake/digest"
expect_stopped "cannot resolve $image:0.1.0 to a digest" "docker buildx imagetools inspect"
good
echo "sha256:abc" >"$tmp/fake/digest"
expect_stopped "cannot resolve $image:0.1.0 to a digest" "docker buildx imagetools inspect"
good
echo 1 >"$tmp/fake/gh-exit"
expect_stopped "$image@$digest ($image:0.1.0) has no build provenance attestation from github.com/bedrock-python/touchmark/.github/workflows/publish.yml on refs/heads/master" \
	"gh attestation verify oci://$image@$digest"
[ "$(grep -c '^== gh' "$log")" = 2 ] || fail "a failed verification is not tried twice"
good
echo v0.0.9 >"$tmp/fake/label"
expect_stopped "is the image of \"v0.0.9\", not of v0.1.0" "docker image inspect --format"

# The environment: CI, GITHUB_*, TOUCHMARK_* and the proxy settings by name,
# never a value on the command line, never the action's own TM_*, GH_TOKEN
# or anything else.
good
step TM_COMMAND=distribute TOUCHMARK_WRITE_APP_KEY=secret-key-canary TOUCHMARK_KEY_EXPOSED=false \
	GITHUB_TOKEN=hub-token-canary OTHER_SECRET=other-canary HTTPS_PROXY=http://proxy.example.invalid:3128
if [ "$code" != 0 ]; then
	fail "distribute with credentials -> exit $code"
fi
for want in "TOUCHMARK_WRITE_APP_KEY=secret-key-canary" "TOUCHMARK_KEY_EXPOSED=false" \
	"GITHUB_TOKEN=hub-token-canary" "CI=true" "GITHUB_ACTIONS=true" "HTTPS_PROXY=http://proxy.example.invalid:3128"; do
	grep -qxF "$want" "$log.env" || fail "the container does not get $want"
done
if grep -q 'OTHER_SECRET\|TM_\|GH_TOKEN' "$log.env" || grep -qxE 'OTHER_SECRET|TM_[A-Z_]+|GH_TOKEN' "$log"; then
	fail "the container gets a variable it should not"
fi
if grep -q 'canary' "$log"; then
	fail "a credential's value reaches a command line"
fi
# The run options: the runner's uid, no capabilities, the workspace, the
# event (read-only) and the summary at their own paths.
uid="$(id -u):$(id -g)"
for want in "--user" "$uid" "--cap-drop" "ALL" "no-new-privileges" "HOME=/tmp" \
	"$tmp/ws:$tmp/ws" "$tmp/event.json:$tmp/event.json:ro" "$tmp/summary.md:$tmp/summary.md" "--rm" "--init"; do
	grep -qxF -- "$want" "$log" || fail "docker run lacks $want"
done
if grep -q 'docker.sock' "$log"; then
	fail "docker run mounts the docker socket"
fi

if [ "$failures" -gt 0 ]; then
	echo "run_test.sh: $failures failure(s)"
	exit 1
fi
echo "run_test.sh: ok"
