#!/bin/sh
# Runs before every Bash command Claude Code wants to run. It hands the
# hook input to the cloudundo program, which protects the command's AWS
# data or blocks it (exit 2).
#
# Without the cloudundo program installed, commands that may touch AWS
# data are blocked with a message saying how to install it; everything
# else runs as usual.

find_cloudundo() {
  if command -v cloudundo >/dev/null 2>&1; then
    command -v cloudundo
    return 0
  fi
  for p in "$HOME/.local/bin/cloudundo" "/usr/local/bin/cloudundo" "/opt/homebrew/bin/cloudundo"; do
    if [ -x "$p" ]; then
      echo "$p"
      return 0
    fi
  done
  return 1
}

if bin=$(find_cloudundo); then
  exec "$bin" hook claude-code
fi

input=$(cat)
if printf '%s' "$input" | grep -Eq '(^|[^A-Za-z0-9_-])(aws|terraform|tofu|redis-cli|valkey-cli)([^A-Za-z0-9_-]|$)'; then
  echo "cloudundo: blocked: this command may change AWS data, and the cloudundo program is not installed, so it cannot be made recoverable." >&2
  echo "Install it: curl -fsSL https://raw.githubusercontent.com/OWNER/cloudundo/main/install.sh | sh" >&2
  echo "Do not run this another way. Tell the user what was blocked and why." >&2
  exit 2
fi
exit 0
