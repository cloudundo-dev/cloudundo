#!/bin/sh
# At session start, tell the user if the cloudundo program is missing.
if command -v cloudundo >/dev/null 2>&1 || [ -x "$HOME/.local/bin/cloudundo" ] || [ -x /usr/local/bin/cloudundo ] || [ -x /opt/homebrew/bin/cloudundo ]; then
  exit 0
fi
printf '%s\n' '{"systemMessage":"cloudundo: the cloudundo program is not installed, so commands that may change AWS data will be blocked. Install it: curl -fsSL https://raw.githubusercontent.com/OWNER/cloudundo/main/install.sh | sh"}'
exit 0
