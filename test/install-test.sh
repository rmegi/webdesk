#!/bin/sh
# Runs install.sh in fresh distro containers, Docker included, and checks the
# page answers inside each. Needs privileged containers (Docker in Docker).
#
#   test/install-test.sh                       ubuntu, debian, fedora
#   test/install-test.sh linuxmint/mint22-amd64 rockylinux:9
#
# Installs the committed state of the current branch, from this checkout.
# Each run downloads packages and builds the images from scratch: expect
# several minutes per distro. Logs go to test/install-logs/.

set -u

cd "$(dirname "$0")/.." || exit
branch=$(git rev-parse --abbrev-ref HEAD)
[ $# -gt 0 ] || set -- ubuntu:24.04 debian:12 fedora:42
mkdir -p test/install-logs
failed=

for image in "$@"; do
  log=test/install-logs/$(echo "$image" | tr '/:' '__').log
  printf '%-32s ' "$image"
  # safe.directory: the checkout is owned by a user the container doesn't have.
  if docker run --rm --privileged -v /var/lib/docker -v "$PWD:/src:ro" \
    -e WEBDESK_REPO=/src -e WEBDESK_BRANCH="$branch" \
    -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0='*' \
    "$image" sh -c 'sh /src/install.sh && sh /src/install.sh' >"$log" 2>&1; then
    echo ok
  else
    echo "FAILED (see $log)"
    failed="$failed $image"
  fi
done

[ -z "$failed" ] || { echo "failed:$failed"; exit 1; }
