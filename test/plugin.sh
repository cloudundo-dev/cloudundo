#!/bin/sh
# Checks the agent plugins in ./plugin: every manifest is valid JSON and
# points at files that exist, and the shared hook script blocks or allows
# the right commands for each agent, with and without the cloudundo program.
#
# Usage: test/plugin.sh path/to/cloudundo-binary
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
bin=$1
fail=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

ok()  { echo "ok    $*"; }
bad() { echo "FAIL  $*"; fail=1; }

for f in .claude-plugin/marketplace.json .cursor-plugin/marketplace.json .agents/plugins/marketplace.json \
         plugin/.claude-plugin/plugin.json plugin/.cursor-plugin/plugin.json plugin/.codex-plugin/plugin.json \
         plugin/hooks/hooks.json plugin/hooks/cursor.json plugin/hooks/codex.json; do
  if python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$root/$f" 2>/dev/null; then ok "valid JSON: $f"; else bad "invalid JSON: $f"; fi
done
for m in plugin/.cursor-plugin/plugin.json plugin/.codex-plugin/plugin.json; do
  h=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["hooks"])' "$root/$m")
  if [ -f "$root/plugin/$h" ]; then ok "$m hooks file exists ($h)"; else bad "$m points at missing $h"; fi
done
[ -x "$root/plugin/scripts/hook.sh" ] && ok "hook.sh is executable" || bad "hook.sh is not executable"

# A directory holding only the binary under test, named cloudundo.
mkdir -p "$tmp/with"
cp "$bin" "$tmp/with/cloudundo"

run() { # agent input path -> exit code
  printf '%s' "$2" | env -i HOME="$tmp/home" PATH="$3" sh "$root/plugin/scripts/hook.sh" $1 >/dev/null 2>&1 && echo 0 || echo $?
}
expect() { # name want got
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: exit $3, want $2"; fi
}

risky='aws s3 rm s3://$B --recursive'
cursor() { printf '{"hook_event_name":"beforeShellExecution","command":"%s","cwd":"%s"}' "$1" "$tmp"; }
pre()    { printf '{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"%s"},"cwd":"%s"}' "$1" "$tmp"; }

for where in without with; do
  if [ $where = with ]; then p="$tmp/with:/usr/bin:/bin"; else p="/usr/bin:/bin"; fi
  expect "cursor, $where program: risky command blocked" 2 "$(run cursor "$(cursor "$risky")" "$p")"
  expect "cursor, $where program: ls allowed" 0 "$(run cursor "$(cursor ls)" "$p")"
  for a in codex claude-code ""; do
    expect "${a:-default}, $where program: risky command blocked" 2 "$(run "$a" "$(pre "$risky")" "$p")"
    expect "${a:-default}, $where program: ls allowed" 0 "$(run "$a" "$(pre ls)" "$p")"
  done
done
expect "unknown agent blocked" 2 "$(run nope "$(pre ls)" "$tmp/with:/usr/bin:/bin")"

exit $fail
