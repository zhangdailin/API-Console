#!/usr/bin/env bash
# Deploy a released orchids-server build to this host.
#
# Usage:
#   deploy-orchids.sh --artifact ./orchids-server-linux-amd64 \
#                     --checksum ./orchids-server-linux-amd64.sha256 \
#                     [--build-info ./orchids-server-linux-amd64.build-info.txt] \
#                     [--install-dir /opt/orchids-2api] [--service orchids-2api]
#
# The script refuses to install a binary whose checksum does not match, keeps the
# previous binary as a timestamped backup, restarts the service, and rolls back
# automatically when the service does not come up. Deploying a build by hand is
# exactly how a Windows binary once reached a Linux host; the checksum plus the
# built-info manifest make that mistake impossible to miss.
#
# Every deploy also records what is actually installed in
# orchids-server.deploy-info.txt. A release installs the pipeline's build-info
# file; a manual install records its own checksum and time, so the version a
# report quotes can always be traced to the bytes on disk instead of to whatever
# build-info file was left behind by an earlier release.

set -euo pipefail

ARTIFACT=""
CHECKSUM=""
BUILD_INFO=""
INSTALL_DIR="/opt/orchids-2api"
SERVICE="orchids-2api"
BINARY_NAME="orchids-server"
HEALTH_PATH="/admin"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-20}"

while [ $# -gt 0 ]; do
  case "$1" in
    --artifact) ARTIFACT="$2"; shift 2 ;;
    --checksum) CHECKSUM="$2"; shift 2 ;;
    --build-info) BUILD_INFO="$2"; shift 2 ;;
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --service) SERVICE="$2"; shift 2 ;;
    --binary-name) BINARY_NAME="$2"; shift 2 ;;
    --health-path) HEALTH_PATH="$2"; shift 2 ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$ARTIFACT" ] || [ ! -f "$ARTIFACT" ]; then
  echo "usage: $0 --artifact <binary> [--checksum <sha256 file>]" >&2
  exit 2
fi

# 0. Resolve every input before anything changes directory. Step 3 installs from
#    $INSTALL_DIR, so a relative --artifact (the documented `--artifact
#    ./orchids-server-linux-amd64` usage) would resolve against the install dir
#    and fail with "cannot stat" after the checksum had already passed.
abs_path() {
  case "$1" in
    /*) printf '%s' "$1" ;;
    *) printf '%s/%s' "$(pwd)" "$1" ;;
  esac
}
ARTIFACT="$(abs_path "$ARTIFACT")"
if [ -n "$CHECKSUM" ]; then CHECKSUM="$(abs_path "$CHECKSUM")"; fi
if [ -n "$BUILD_INFO" ]; then BUILD_INFO="$(abs_path "$BUILD_INFO")"; fi

# 1. Refuse anything that is not a Linux/amd64 ELF. This is the guard that the
#    "uploaded server.exe" incident needs.
file_type="$(file -b "$ARTIFACT" || true)"
case "$file_type" in
  *ELF*64-bit*x86-64*) : ;;
  *) echo "refusing to deploy: $ARTIFACT is not a linux/amd64 ELF ($file_type)" >&2; exit 3 ;;
esac

# 2. Verify the published checksum when one is supplied.
if [ -n "$CHECKSUM" ]; then
  if [ ! -f "$CHECKSUM" ]; then
    echo "checksum file not found: $CHECKSUM" >&2
    exit 4
  fi
  expected="$(awk '{print $1}' "$CHECKSUM" | head -1)"
  actual="$(sha256sum "$ARTIFACT" | awk '{print $1}')"
  if [ "$expected" != "$actual" ]; then
    echo "checksum mismatch: expected $expected, got $actual" >&2
    exit 5
  fi
  echo "checksum ok: $actual"
fi

# 3. Snapshot the binary and its metadata before publishing any new files.
cd "$INSTALL_DIR"
stamp="$(date -u +%Y%m%d-%H%M%S)"
backup="${BINARY_NAME}.backup-${stamp}"
metadata="${backup}.metadata"
if [ -e "$backup" ] || [ -e "$metadata" ]; then
  echo "backup already exists: $backup" >&2
  exit 5
fi
mkdir -m 0700 "$metadata"
if [ -f "$BINARY_NAME" ]; then cp -p "$BINARY_NAME" "$backup"; fi
for suffix in build-info.txt deploy-info.txt; do
  if [ -f "${BINARY_NAME}.${suffix}" ]; then
    cp -p "${BINARY_NAME}.${suffix}" "${metadata}/${suffix}"
  fi
done
installed_sha="$(sha256sum "$ARTIFACT" | awk '{print $1}')"

healthy() {
  local deadline code
  deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if systemctl is-active --quiet "$SERVICE"; then
      code="$(curl --connect-timeout 2 --max-time 3 -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:3002${HEALTH_PATH}" || true)"
      case "$code" in 200|301|302) return 0 ;; esac
    fi
    sleep 1
  done
  return 1
}

rollback() {
  trap - ERR
  set +e
  local restored=1 suffix rollback_sha="unavailable" rollback_status="failed"
  echo "deployment failed; restoring $backup and its metadata" >&2
  if [ -f "$backup" ]; then
    install -m 0755 "$backup" "${BINARY_NAME}.rollback" && mv -f "${BINARY_NAME}.rollback" "$BINARY_NAME" || restored=0
    rollback_sha="$(sha256sum "$BINARY_NAME" | awk '{print $1}')"
    [ "$rollback_sha" = "$(sha256sum "$backup" | awk '{print $1}')" ] || restored=0
  else
    restored=0
    rm -f "$BINARY_NAME"
  fi
  for suffix in build-info.txt deploy-info.txt; do
    if [ -f "${metadata}/${suffix}" ]; then
      cp -p "${metadata}/${suffix}" "${BINARY_NAME}.${suffix}.incoming" && mv -f "${BINARY_NAME}.${suffix}.incoming" "${BINARY_NAME}.${suffix}" || restored=0
    else
      rm -f "${BINARY_NAME}.${suffix}" || restored=0
    fi
  done
  if [ "$restored" -eq 1 ] && systemctl restart "$SERVICE" && healthy; then
    rollback_status="healthy"
  fi
  {
    echo "failed_sha256=${installed_sha}"
    echo "rolled_back_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "current_sha256=${rollback_sha}"
    echo "rollback_status=${rollback_status}"
  } > "${BINARY_NAME}.last-failed-deploy.txt.incoming"
  mv -f "${BINARY_NAME}.last-failed-deploy.txt.incoming" "${BINARY_NAME}.last-failed-deploy.txt"
  if [ "$rollback_status" = healthy ]; then
    echo "previous binary and metadata restored; rollback health verified" >&2
    exit 6
  fi
  echo "rollback did not recover a healthy service" >&2
  exit 7
}
trap rollback ERR

install -m 0755 "$ARTIFACT" "${BINARY_NAME}.incoming"
mv -f "${BINARY_NAME}.incoming" "$BINARY_NAME"
if [ -n "$BUILD_INFO" ]; then
  cp -f "$BUILD_INFO" "${BINARY_NAME}.build-info.txt.incoming"
  mv -f "${BINARY_NAME}.build-info.txt.incoming" "${BINARY_NAME}.build-info.txt"
else
  # Metadata from the previous artifact cannot describe a manual build.
  rm -f "${BINARY_NAME}.build-info.txt"
fi
{
  echo "sha256=${installed_sha}"
  echo "deployed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "artifact=${ARTIFACT}"
  if [ -n "$BUILD_INFO" ]; then
    echo "build_info=installed from $(basename "$BUILD_INFO")"
  else
    echo "build_info=not supplied"
  fi
} > "${BINARY_NAME}.deploy-info.txt.incoming"
mv -f "${BINARY_NAME}.deploy-info.txt.incoming" "${BINARY_NAME}.deploy-info.txt"
systemctl restart "$SERVICE"
healthy
trap - ERR

echo "deployed and healthy: ${installed_sha}"
# Prune only stamped binary backups and their companion snapshots after health
# succeeds. Never prune the rollback target while a deployment is in progress.
retain="${BACKUP_RETAIN:-3}"
if ! [[ "$retain" =~ ^[1-9][0-9]*$ ]]; then retain=3; fi
backups=()
for candidate in "${BINARY_NAME}".backup-[0-9]*; do
  if [ -f "$candidate" ] && [[ "$candidate" =~ \.backup-[0-9]{8}-[0-9]{6}$ ]]; then backups+=("$candidate"); fi
done
if [ "${#backups[@]}" -gt "$retain" ]; then
  mapfile -t ordered < <(printf '%s\n' "${backups[@]}" | sort -r)
  for (( i=retain; i<${#ordered[@]}; i++ )); do
    rm -f -- "${ordered[$i]}"
    rm -rf -- "${ordered[$i]}.metadata"
  done
fi
