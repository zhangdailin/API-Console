#!/usr/bin/env sh
# Fails when a non-test Go file grows past the ceiling.
#
# The ceiling is a maintenance budget, not a style rule: a file nobody can hold
# in one screen is a file nobody reviews. New files start under it and existing
# oversized ones are listed in EXEMPT with a reason, so the list can only shrink.
set -eu
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"

CEILING=700

# Files over the ceiling before this check existed. Each entry carries the size
# that must not grow; splitting a file removes its entry.
EXEMPT="
internal/qoder/client.go
internal/grok/native_chat.go
internal/workbuddy/auth.go
internal/handler/handler_helpers.go
internal/qoder/stream.go
internal/loadbalancer/loadbalancer.go
internal/grok/handler.go
internal/api/api_ops.go
internal/grok/quality_hold.go
internal/opsagg/opsagg.go
"

status=0
while IFS= read -r path; do
	[ -n "$path" ] || continue
	case "$path" in
	*_test.go) continue ;;
	*.go) ;;
	*) continue ;;
	esac
	[ -f "$path" ] || continue
	lines=$(wc -l <"$path" | tr -d ' ')
	if [ "$lines" -le "$CEILING" ]; then
		continue
	fi
	if printf '%s\n' "$EXEMPT" | grep -qxF "$path"; then
		continue
	fi
	printf '%s: %s lines (ceiling %s)\n' "$path" "$lines" "$CEILING"
	status=1
done <<EOF
$(git ls-files '*.go')
EOF

# An exemption for a file that no longer needs one hides the next violation.
while IFS= read -r path; do
	case "$path" in
	'' | \#*) continue ;;
	esac
	[ -n "$path" ] || continue
	if [ ! -f "$path" ]; then
		printf '%s: exempted but missing\n' "$path"
		status=1
		continue
	fi
	lines=$(wc -l <"$path" | tr -d ' ')
	if [ "$lines" -le "$CEILING" ]; then
		printf '%s: %s lines, no longer needs an exemption — remove it\n' "$path" "$lines"
		status=1
	fi
done <<EOF
$EXEMPT
EOF

if [ "$status" -eq 0 ]; then
	echo "every non-test Go file is at or under $CEILING lines"
fi
exit "$status"
