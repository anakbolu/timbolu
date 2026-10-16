#!/usr/bin/env bash
#
# Remove the virtual-computer sandboxes a stack left behind: the containers the control
# plane stopped (or created and never got running), and — with VOLUMES=1 — the home volumes
# that go with them. With --running it takes the sandboxes that are still up as well, which
# is the mode a teardown needs.
#
# Why this exists next to `POST /v1/admin/cleanup`: that route does the same thing over
# HTTP, with the same label and the same stack scope, but it needs the control plane to be
# up and it needs the token. An operator who has neither — the stack is already down, the
# token went away with the terminal it was generated in — has only the docker CLI left, and
# this is that path. It is the CLI half of the pair, not a second mechanism.
#
# Why --running exists at all: a sandbox is created by the control plane rather than
# declared in the compose file, so `docker compose down` does not know about it, and the
# default mode deliberately leaves a running one alone — a running sandbox is a session
# somebody is using, and a "cleanup" that killed it would be the opposite of what the word
# means. The control plane reaps its own sandboxes on SIGTERM (docs/adr/0003-virtual-computer.md
# §3.9), so a graceful `down` needs nothing here. This mode is for the teardown that had no
# grace: a control plane that was SIGKILLed, a docker host where the stack was removed out
# from under it, or a `down` whose stop grace expired first. Those sandboxes hold their
# volumes and the stack's internal network, and without this the leftovers have to be found
# by hand — which is what #245 was.
#
# It never touches the control plane or the fixture container: neither carries the
# `vcomputer.bot` label (the runtime puts that label on the containers and volumes it
# creates for a bot, control/internal/sandbox/docker.go), so the label filter is what stops
# a cleanup from taking the running stack down with it. The name filter is what keeps two
# stacks on one host from taking each other's sandboxes: a sandbox is named
# <stack>-<bot id>, so the stack is the scope.
#
# Usage, from the repository root:
#
#   apps/virtual-computer/scripts/cleanup.sh              # stopped sandboxes only
#   VOLUMES=1 apps/virtual-computer/scripts/cleanup.sh    # ... and their volumes
#   apps/virtual-computer/scripts/cleanup.sh --volumes
#   apps/virtual-computer/scripts/cleanup.sh --running --volumes   # after a teardown
#   STACK=vc2 DOCKER="sudo docker" apps/virtual-computer/scripts/cleanup.sh
#
# Environment:
#   DOCKER   the docker CLI. Default `docker`. "sudo docker" is expected to work, which is
#            why every use of it below is unquoted on purpose (shellcheck SC2086 is
#            silenced where it fires, with that reason).
#   STACK    the stack whose leftovers are removed. Default vcomputer — the same default as
#            scripts/e2e.sh and docker-compose.poc.yml.
#   VOLUMES  set to 1 to remove the volumes as well. Off by default: a volume is a bot's
#            home directory, and deleting one is a data decision rather than a cleanup
#            step. The label is what makes it findable, not what makes it disposable.
#   RUNNING  set to 1 to include the sandboxes that are still running, and to remove their
#            volumes in the same pass. This is the destructive mode: it ends sessions that
#            nobody asked it to end, so it is off by default and it is never what
#            `docker compose down` needs while the control plane is shutting down
#            gracefully.

set -euo pipefail

DOCKER=${DOCKER:-docker}
STACK=${STACK:-vcomputer}
VOLUMES=${VOLUMES:-0}
RUNNING=${RUNNING:-0}

for arg in "$@"; do
    case "$arg" in
        --volumes)
            VOLUMES=1
            ;;
        --running)
            RUNNING=1
            ;;
        -h | --help)
            printf 'usage: [STACK=%s] [VOLUMES=1] [RUNNING=1] [DOCKER="sudo docker"] %s [--volumes] [--running]\n' "$STACK" "$0"
            exit 0
            ;;
        *)
            printf 'cleanup.sh: unknown argument %s\n' "$arg" >&2
            exit 2
            ;;
    esac
done

removed=0
failed=0

# The scope is the same in both modes — the label every sandbox carries and the name
# prefix that ties it to this stack — and the status filters are what make the default "not
# running". RUNNING=1 drops them, and that is the whole difference between the modes: the
# removal below forces either way, so a running container and a stopped one need no
# different command.
container_filters=(
    --filter label=vcomputer.bot
    --filter "name=^${STACK}-"
)
if [ "$RUNNING" != "1" ]; then
    # Stopped sandboxes, in every state that means "not running": `exited` is the one the
    # control plane leaves behind, `created` is a container that never came up, and `dead`
    # is one the daemon gave up on. The status filters are OR'ed, so all three are one
    # query.
    container_filters+=(
        --filter status=exited
        --filter status=created
        --filter status=dead
    )
fi

# shellcheck disable=SC2086  # DOCKER may be "sudo docker": the split is the point
containers=$($DOCKER ps -aq "${container_filters[@]}")

# shellcheck disable=SC2086  # the list is newline-separated container ids on purpose
for id in $containers; do
    # The name rather than the id goes into the log: an operator looking at this output is
    # looking for a sandbox they recognise.
    # shellcheck disable=SC2086  # DOCKER may be "sudo docker"
    name=$($DOCKER inspect --format '{{.Name}}' "$id" | sed 's#^/##')
    # shellcheck disable=SC2086  # DOCKER may be "sudo docker"
    if $DOCKER rm -f "$id" >/dev/null; then
        printf 'removed container %s\n' "$name"
        removed=$((removed + 1))
    else
        printf 'cleanup.sh: could not remove container %s\n' "$name" >&2
        failed=$((failed + 1))
    fi
done

if [ "$VOLUMES" = "1" ]; then
    # The same two filters as above, for the same two reasons: the label finds what the
    # runtime created for a bot, the name scopes it to this stack (a volume is
    # <stack>-<bot id>-home).
    # shellcheck disable=SC2086  # DOCKER may be "sudo docker"
    volumes=$($DOCKER volume ls -q \
        --filter label=vcomputer.bot \
        --filter "name=^${STACK}-")

    # shellcheck disable=SC2086  # the list is newline-separated volume names on purpose
    for volume in $volumes; do
        # shellcheck disable=SC2086  # DOCKER may be "sudo docker"
        if $DOCKER volume rm "$volume" >/dev/null; then
            printf 'removed volume %s\n' "$volume"
            removed=$((removed + 1))
        else
            # A volume still in use by a running container lands here, and that is the
            # one case where saying so beats staying quiet: the container has to go
            # first, which is what --running does in the same pass.
            printf 'cleanup.sh: could not remove volume %s (still in use?)\n' "$volume" >&2
            failed=$((failed + 1))
        fi
    done
fi

if [ "$removed" -eq 0 ] && [ "$failed" -eq 0 ]; then
    # The message names the mode it was looking in, because the two answers mean
    # different things: "no stopped sandbox" is the normal end of a clean teardown, while
    # "no sandbox at all" is what a teardown that missed one would look like too.
    scope='stopped sandbox container and no sandbox volume'
    if [ "$RUNNING" = "1" ]; then
        scope='sandbox container and no sandbox volume'
    fi
    printf 'nothing to clean up: no %s for stack %s\n' "$scope" "$STACK"
fi

if [ "$failed" -ne 0 ]; then
    exit 1
fi
