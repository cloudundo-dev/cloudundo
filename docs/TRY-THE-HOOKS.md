# Trying the hooks inside each agent

Do this on a test AWS account. Each agent takes about 10 minutes.
Write down what you see at each ✅ step; anything different is a finding.

## Before you start

```sh
cloudundo version                       # installed and on PATH
aws sts get-caller-identity             # the TEST account
B=cloudundo-hooktest-$RANDOM$RANDOM
aws s3 mb s3://$B
echo hello > a.txt && aws s3 cp a.txt s3://$B/a.txt
echo $B                                 # you will paste this name into the agent
```

Use the same three prompts in every agent (replace BUCKET with the name above):

1. **Harmless:** `Run this exact command: aws s3 ls s3://BUCKET/`
2. **Protected delete:** `Run this exact command: aws s3 rm s3://BUCKET/a.txt`
3. **Must be blocked:** `Run this exact command: aws s3 rm s3://$UNKNOWN_BUCKET --recursive`

After prompt 2, outside the agent:

```sh
aws s3 ls s3://$B/            # a.txt is gone
cloudundo log                 # the entry shows the agent's name
cloudundo undo <id> --yes
aws s3 cp s3://$B/a.txt -     # hello
```

## Claude Code

```sh
claude --version                          # note the version
cloudundo install claude-code             # read what it will add
cloudundo install claude-code --write     # adds it to ~/.claude/settings.json (keeps a backup)
```

Start `claude` in any folder. Type `/hooks` and check that a PreToolUse hook for `Bash` runs `cloudundo hook claude-code`.

- ✅ Prompt 1 runs normally. Claude Code still asks for permission as usual: cloudundo never approves for you.
- ✅ Prompt 2: you may see a "cloudundo: protected #N" message, and Claude mentions it can be undone. The file is deleted.
- ✅ Prompt 3 is refused; Claude tells you cloudundo blocked it and why, and does not try another way.
- If Claude Code complains about `onFailure` in settings (versions before 2.1.295), remove that one line from `~/.claude/settings.json`.

## Codex

```sh
codex --version
cloudundo install codex --write           # adds it to ~/.codex/hooks.json
```

Start `codex`, type `/hooks`, and **trust** the cloudundo hook (Codex does not run new hooks until you do).

- ✅ Prompt 1 runs normally.
- ✅ Prompt 2 runs; Codex shows no note (it has no way to), but `cloudundo log` has the entry.
- ✅ Prompt 3 is refused with cloudundo's reason.

## Cursor (Windows, working inside WSL)

Open Cursor, then **Connect to WSL** (bottom-left corner, or the command palette: "WSL: Connect to WSL"), and open a folder inside Ubuntu, so the agent's commands run in Ubuntu where cloudundo and `aws` are installed.

Add the hook to the project you opened (inside Ubuntu):

```sh
cd <the folder you opened in Cursor>
mkdir -p .cursor
cat > .cursor/hooks.json <<EOF
{
  "version": 1,
  "hooks": {
    "beforeShellExecution": [
      { "command": "$(command -v cloudundo) hook cursor", "timeout": 600, "failClosed": true }
    ]
  }
}
EOF
cat .cursor/hooks.json
```

Restart Cursor. In Cursor's Output panel, the "Hooks" channel shows each run.

- ✅ Prompt 1 runs normally.
- ✅ Prompt 2 runs; the agent sees cloudundo's note.
- ✅ Prompt 3 is refused with cloudundo's reason.
- If nothing happens at all, the project hook was not loaded: also try `cloudundo install cursor --write` inside Ubuntu (user-level `~/.cursor/hooks.json`) and restart Cursor. Note which one worked.

## Clean up

Empty and delete the `cloudundo-hooktest-…` bucket in the S3 console.
To remove a hook later, delete the cloudundo entry from the settings file; the original is saved next to it as `*.before-cloudundo`.
