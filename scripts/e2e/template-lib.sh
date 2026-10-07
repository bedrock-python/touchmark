# shellcheck shell=bash
# template-lib.sh is sourced by gitlab.sh and gitea.sh for --template DIR:
# the hub template's own CI files, run on the live forge by a runner that
# runs each job in the touchmark image, as a hub's runners do. It is not a
# script of its own. docs/project/e2e.md, "The hub template", explains the runs.
#
# The caller defines REPO, GO_IMAGE, die, log, host_path and rand_hex, and
# calls tpl_cleanup from its cleanup.
#
#   tpl_image SUFFIX
#       builds the image from the working tree's Dockerfile (or takes the
#       one --touchmark-image named), starts a registry in the Docker
#       host's network namespace on 127.0.0.1, pushes the image there and
#       sets tpl_image_ref to NAME:TAG@sha256:<digest>: the form the
#       template pins the release image in, which the tests write in place
#       of its placeholder. The Docker daemon pulls from it as any
#       registry on localhost, without TLS.
#   tpl_loopback SUFFIX CONTAINER NETWORK PORT
#       forwards 127.0.0.1:PORT and [::1]:PORT of the Docker host's network
#       namespace to CONTAINER's PORT on NETWORK (scripts/e2e/loopback.go).
#       The runner starts the job containers in that namespace (network
#       mode host), so they reach the forge at its external URL,
#       http://localhost[:PORT], the only plain-http host touchmark sends a
#       credential to. A job container cannot join the forge's network
#       namespace instead: GitLab's Docker executor gives every container a
#       hostname, which Docker refuses with --network container:<name>.
#   tpl_cleanup
#       removes the registry, the forwarder and the image tags tpl_image
#       made.
#
# The registry and the forwarder run in the Docker host's network namespace
# (on Docker Desktop, its Linux VM's, not the desktop's), which other
# containers of the machine share: the ports are taken for the run only.
#
# Images: the registry of Docker's distribution project, by digest (the one
# .github/workflows/ci.yml runs).

readonly TPL_REGISTRY_IMAGE=registry:3.1.2@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8

tpl_registry=
tpl_forwarder=
tpl_built=
tpl_pushed=
tpl_image_ref=
# tpl_source is the image under test: a reference from --touchmark-image,
# or empty to build one from the working tree.
tpl_source=${tpl_source:-}

# tpl_wait_log CONTAINER TEXT SECONDS waits until the container's log holds
# TEXT; it fails when the container stops or the time is up.
tpl_wait_log() {
	local c=$1 text=$2 deadline=$((SECONDS + $3)) state
	# The log is captured first: with pipefail, grep -q's early exit would
	# fail docker logs on a broken pipe, and the wait would never end.
	until grep -qF -- "$text" <<<"$(docker logs "$c" 2>&1)"; do
		if state=$(docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null) && [ "$state" != true ]; then
			docker logs --tail 50 "$c" >&2 || true
			die "$c stopped before it printed \"$text\""
		fi
		((SECONDS < deadline)) || {
			docker logs --tail 50 "$c" >&2 || true
			die "$c did not print \"$text\" in $3s"
		}
		sleep 1
	done
}

tpl_image() {
	local suffix=$1 image port digest
	if [ -n "$tpl_source" ]; then
		image=$tpl_source
		if ! docker image inspect "$image" >/dev/null 2>&1; then
			docker pull -q "$image" >/dev/null || die "cannot pull $image"
		fi
	else
		image=touchmark-e2e-image:$suffix
		log "building the touchmark image from $REPO (Dockerfile, BINARY=source)"
		tpl_built=$image
		docker buildx build --load --quiet -t "$image" "$(host_path "$REPO")" >/dev/null ||
			die "cannot build the touchmark image"
	fi
	if ! docker image inspect "$TPL_REGISTRY_IMAGE" >/dev/null 2>&1; then
		docker pull -q "$TPL_REGISTRY_IMAGE" >/dev/null
	fi
	port=$((20000 + 0x$(rand_hex 2) % 10000))
	tpl_registry=touchmark-e2e-$suffix-registry
	docker run -d --name "$tpl_registry" --network host -e "REGISTRY_HTTP_ADDR=127.0.0.1:$port" \
		-e OTEL_TRACES_EXPORTER=none "$TPL_REGISTRY_IMAGE" >/dev/null
	local deadline=$((SECONDS + 60))
	until docker exec "$tpl_registry" wget -q -O /dev/null "http://127.0.0.1:$port/v2/" 2>/dev/null; do
		if [ "$(docker inspect -f '{{.State.Running}}' "$tpl_registry" 2>/dev/null)" != true ] || ((SECONDS >= deadline)); then
			docker logs --tail 30 "$tpl_registry" >&2 || true
			die "the registry on 127.0.0.1:$port does not answer"
		fi
		sleep 1
	done
	tpl_pushed=localhost:$port/touchmark:0.0.0-e2e
	docker tag "$image" "$tpl_pushed"
	docker push -q "$tpl_pushed" >/dev/null || die "cannot push to the registry on localhost:$port"
	# The digest is the one the registry serves for the tag, not the one
	# RepoDigests names: with the containerd image store, a multi-platform
	# image pulled for one platform is pushed as that platform's manifest
	# alone, while RepoDigests keeps the index's digest, which the registry
	# then lacks (a runner that always pulls fails on it).
	digest=$(docker exec "$tpl_registry" wget -S -q -O /dev/null \
		--header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
		"http://127.0.0.1:$port/v2/touchmark/manifests/0.0.0-e2e" 2>&1 |
		sed -n 's/^ *[Dd]ocker-[Cc]ontent-[Dd]igest: *\(sha256:[0-9a-f]\{64\}\).*$/\1/p' | head -n 1)
	[ -n "$digest" ] || die "the registry on localhost:$port serves no digest for $tpl_pushed"
	tpl_image_ref=$tpl_pushed@$digest
	log "touchmark image: $image, as $tpl_image_ref ($(docker run --rm --entrypoint touchmark "$tpl_image_ref" version 2>&1 | head -n 1))"
}

tpl_loopback() {
	local suffix=$1 container=$2 network=$3 port=$4 ip
	ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$container")
	[ -n "$ip" ] || die "$container has no address on $network"
	if ! docker image inspect "$GO_IMAGE" >/dev/null 2>&1; then
		docker pull -q "$GO_IMAGE" >/dev/null
	fi
	tpl_forwarder=touchmark-e2e-$suffix-loopback
	docker run -d --name "$tpl_forwarder" --network host -v "$(host_path "$REPO"):/src:ro" -w /src \
		-e GOTOOLCHAIN=local -e GOFLAGS=-mod=readonly -e CGO_ENABLED=0 "$GO_IMAGE" \
		sh -c "go build -o /tmp/loopback ./scripts/e2e/loopback.go && exec /tmp/loopback -to '$ip:$port' '127.0.0.1:$port' '[::1]:$port'" >/dev/null
	tpl_wait_log "$tpl_forwarder" "loopback: ready" 180
	docker exec "$tpl_forwarder" curl -sS -o /dev/null "http://localhost:$port/" ||
		die "http://localhost:$port does not reach $container through the forwarder"
	log "forwarder: 127.0.0.1:$port and [::1]:$port of the Docker host's network namespace to $container ($ip:$port)"
}

tpl_cleanup() {
	if [ -n "$tpl_forwarder" ]; then docker rm -f -v "$tpl_forwarder" >/dev/null 2>&1 || true; fi
	if [ -n "$tpl_registry" ]; then docker rm -f -v "$tpl_registry" >/dev/null 2>&1 || true; fi
	if [ -n "$tpl_pushed" ]; then docker image rm "$tpl_pushed" >/dev/null 2>&1 || true; fi
	if [ -n "$tpl_built" ]; then docker image rm "$tpl_built" >/dev/null 2>&1 || true; fi
}
