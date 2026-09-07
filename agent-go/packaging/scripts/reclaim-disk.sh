#!/bin/sh
# reclaim-disk.sh — recover disk that earlier agent versions leaked on
# uninstall.
#
# WHY THIS EXISTS
#
# Agents before 0.5.2 tore a deployment down with a bare
# `docker compose down`. That removes containers and the project network
# but never the images, so every install/uninstall cycle left the full
# image behind — 650MB for a single Postgres, 2GB for a Nextcloud. On a
# box that has cycled through a few apps this is the difference between
# a half-empty disk and a full one. Agents from 0.5.2 on pass
# `--remove-orphans --rmi all`, but that only helps deployments torn down
# from then on; anything already leaked stays leaked. This script cleans
# up the backlog.
#
# SAFETY
#
# Runs as a REPORT ONLY unless you pass an --apply flag. Nothing is
# removed by default. The image pass only ever touches images that no
# container references — the daemon itself refuses to delete an image
# held by a container, running or stopped, so a live deployment cannot
# lose its image. Worst case a stopped deployment re-pulls on next start.
#
# USAGE
#   reclaim-disk.sh                     report only, changes nothing
#   reclaim-disk.sh --apply-images      remove unreferenced images + build cache
#   reclaim-disk.sh --apply-orphan-data remove data/ of uninstalled deployments
#                                       (DESTRUCTIVE — read the warning below)
#
# Exit codes: 0 ok, 1 usage error, 2 docker unavailable.

set -eu

STATE_DIR="${IMPREZA_STATE_DIR:-/var/lib/impreza-agent}"
APPS_DIR="$STATE_DIR/apps"

APPLY_IMAGES=0
APPLY_ORPHAN_DATA=0

for arg in "$@"; do
    case "$arg" in
        --apply-images)      APPLY_IMAGES=1 ;;
        --apply-orphan-data) APPLY_ORPHAN_DATA=1 ;;
        -h|--help)
            sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "unknown argument: $arg (try --help)" >&2
            exit 1
            ;;
    esac
done

if ! command -v docker >/dev/null 2>&1; then
    echo "docker not found in PATH; nothing to do." >&2
    exit 2
fi

human() { awk -v b="${1:-0}" 'BEGIN{ split("B KB MB GB TB",u," "); i=1;
    while (b>=1024 && i<5) { b/=1024; i++ } printf "%.1f%s", b, u[i] }'; }

echo "=============================================================="
echo " Impreza agent — disk reclaim"
echo " state dir: $STATE_DIR"
if [ "$APPLY_IMAGES" -eq 0 ] && [ "$APPLY_ORPHAN_DATA" -eq 0 ]; then
    echo " mode:      REPORT ONLY (pass --apply-images to act)"
else
    echo " mode:      APPLY  images=$APPLY_IMAGES orphan-data=$APPLY_ORPHAN_DATA"
fi
echo "=============================================================="
echo

echo "--- disk before ---"
df -h "$STATE_DIR" 2>/dev/null || df -h /
echo

# ─────────────────────────────────────────────────────────────────────
# 1. Images no container references.
# ─────────────────────────────────────────────────────────────────────
# Build the set of image IDs that ARE in use, then subtract. Comparing
# resolved IDs (not names) is what makes this correct for containers
# started from a digest or from a since-retagged image.
echo "--- unreferenced images ---"
in_use=$(docker ps -aq | while read -r c; do
    [ -n "$c" ] && docker inspect -f '{{.Image}}' "$c" 2>/dev/null
done | sort -u)

# Sizes come from `docker images`, NOT `docker image inspect --format
# {{.Size}}`. On daemons using the containerd image store those disagree
# badly — inspect reports the compressed content size (162MB for
# postgres:18) while the unpacked on-disk footprint is what actually
# fills the customer's disk (650MB for the same image). Reporting the
# compressed number understated the reclaim by roughly 4x.
orphan_ids=""
orphan_list=""
for id in $(docker images -q --no-trunc | sort -u); do
    if ! printf '%s\n' "$in_use" | grep -qxF "$id"; then
        orphan_ids="$orphan_ids $id"
        row=$(docker images --no-trunc --format '{{.ID}}|{{.Repository}}:{{.Tag}}|{{.Size}}' \
              | awk -F'|' -v want="$id" '$1==want {print $2"|"$3; exit}')
        [ -z "$row" ] && row="<untagged>|?"
        orphan_list="$orphan_list
$row"
    fi
done

if [ -z "$orphan_ids" ]; then
    echo "  (none — nothing leaked)"
else
    printf '%s\n' "$orphan_list" | while IFS='|' read -r name size; do
        [ -z "$name" ] && continue
        printf '  %-52s %s\n' "$name" "$size"
    done
    echo "  ------------------------------------------------------------"
    # Sum docker's own human-readable sizes. Deliberately labelled an
    # upper bound: images that share base layers each report their full
    # size, but those shared layers only exist once on disk, so the true
    # reclaim is usually somewhat less than the sum. The authoritative
    # number is the df delta printed at the end of this run.
    printf '%s\n' "$orphan_list" | awk -F'|' '
        /[^[:space:]]/ {
            s = $2
            v = s + 0
            if (s ~ /GB/)      v *= 1024*1024*1024
            else if (s ~ /MB/) v *= 1024*1024
            else if (s ~ /kB/ || s ~ /KB/) v *= 1024
            t += v
        }
        END {
            split("B KB MB GB TB", u, " "); i = 1
            while (t >= 1024 && i < 5) { t /= 1024; i++ }
            printf "  reclaimable: up to %.1f%s (shared layers counted once on disk)\n", t, u[i]
        }'
fi
echo

if [ "$APPLY_IMAGES" -eq 1 ] && [ -n "$orphan_ids" ]; then
    echo "  removing..."
    # No `-f`: if a container grabbed the image between the scan and now,
    # the daemon refuses and we skip it rather than forcing.
    for id in $orphan_ids; do
        docker rmi "$id" >/dev/null 2>&1 || echo "    skipped (now in use): $id"
    done
    echo "  done."
    echo
    echo "--- build cache ---"
    docker builder prune -f 2>/dev/null | tail -2 || echo "  (builder prune unavailable)"
    echo
fi

# ─────────────────────────────────────────────────────────────────────
# 2. State dirs of deployments that are gone.
# ─────────────────────────────────────────────────────────────────────
# An uninstall with purge_data=false intentionally keeps `data/`. That
# data is unreachable though: a reinstall gets a NEW deployment_id, hence
# a new appDir, so nothing ever reads the old tree again. Agent 0.5.2+
# marks these by removing compose.yaml and .env on uninstall, so
# "directory with a data/ but no compose.yaml" identifies them.
echo "--- state dirs with no compose.yaml (uninstalled, data retained) ---"
orphan_dirs=""
orphan_dir_bytes=0
if [ -d "$APPS_DIR" ]; then
    for d in "$APPS_DIR"/*; do
        [ -d "$d" ] || continue
        [ -f "$d/compose.yaml" ] && continue
        dep=$(basename "$d")
        # Belt and braces: never touch a dir that still has containers.
        live=$(docker ps -aq --filter "label=com.docker.compose.project=$(echo "$dep" | tr '[:upper:]' '[:lower:]')" | wc -l)
        if [ "$live" -gt 0 ]; then
            printf '  %-40s SKIP — still has %s container(s)\n' "$dep" "$live"
            continue
        fi
        bytes=$(du -sb "$d" 2>/dev/null | cut -f1)
        [ -z "$bytes" ] && bytes=0
        printf '  %-40s %s\n' "$dep" "$(human "$bytes")"
        orphan_dirs="$orphan_dirs $d"
        orphan_dir_bytes=$((orphan_dir_bytes + bytes))
    done
fi
if [ -z "$orphan_dirs" ]; then
    echo "  (none)"
else
    echo "  ------------------------------------------------------------"
    printf '  reclaimable: %s\n' "$(human "$orphan_dir_bytes")"
    if [ "$APPLY_ORPHAN_DATA" -eq 0 ]; then
        echo
        echo "  NOT removed. These hold app data kept by an uninstall that"
        echo "  ran with purge_data=false. No Impreza flow can restore them,"
        echo "  but they may still be the only copy of a customer's data —"
        echo "  back up anything you might owe a customer BEFORE running:"
        echo "      $0 --apply-orphan-data"
    fi
fi
echo

if [ "$APPLY_ORPHAN_DATA" -eq 1 ] && [ -n "$orphan_dirs" ]; then
    echo "  removing state dirs..."
    for d in $orphan_dirs; do
        case "$d" in
            "$APPS_DIR"/*) rm -rf -- "$d" && echo "    removed $(basename "$d")" ;;
            *) echo "    refused (outside $APPS_DIR): $d" ;;
        esac
    done
    echo "  done."
    echo
fi

echo "--- disk after ---"
df -h "$STATE_DIR" 2>/dev/null || df -h /

if [ "$APPLY_IMAGES" -eq 0 ] && [ "$APPLY_ORPHAN_DATA" -eq 0 ]; then
    echo
    echo "Report only — nothing was changed."
fi
