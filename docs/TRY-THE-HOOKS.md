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

Done on 24 October 2026 with Claude Code 2.1.310: all three prompts behaved as below, and `undo` brought the file back.

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

Terraform was tried on 25 October 2026 in both agents: the agent ran `terraform destroy -auto-approve` on a bucket, an object and a DynamoDB table, cloudundo backed them up first, and `undo` brought them back. In Cursor this found that its hook input has an empty `cwd` and the project folder in `workspace_roots`; fixed in 0.4.0-rc7.

Done on 24 October 2026 with the project hook below (Cursor 3.24): all three prompts behaved as below, and `undo` brought the file back. Cursor retried the delete once outside its sandbox, so it shows twice in `cloudundo log`, both protected. A first try hung until the hook stopped waiting for Cursor to close its input (fixed in 0.4.0-rc2).
`cloudundo install cursor --write` run inside Ubuntu (user-level `~/.cursor/hooks.json` there) works the same way with Cursor connected to WSL.

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

## Part 2: the plugins

The same protection, installed as a plugin instead of with `cloudundo install`. Test them one agent at a time, **after removing that agent's hook from Part 1** so the command is not checked twice:

```sh
cp ~/.claude/settings.json.before-cloudundo ~/.claude/settings.json   # Claude Code
cp ~/.codex/hooks.json.before-cloudundo ~/.codex/hooks.json           # Codex
rm -f <your project>/.cursor/hooks.json                                # Cursor (and undo the ~/.cursor one if you used it)
```

(If a `.before-cloudundo` file does not exist, the file did not exist before: delete it instead.)

Put the project's source in Ubuntu at `~/cloudundo-src` (the test kit has it as the `cloudundo-src` folder: `cp -r cloudundo-src ~/`). Then run the same three prompts as in Part 1 after each install.

### Claude Code plugin

Inside `claude`:

```
/plugin marketplace add ~/cloudundo-src
/plugin install cloudundo@cloudundo
```

Restart `claude`, then `/hooks` should list the plugin's PreToolUse hook. Run the three prompts.

### Codex plugin

```sh
codex plugin marketplace add ~/cloudundo-src
```

Start `codex`, open `/plugins`, install cloudundo, start a new session, then `/hooks` and **trust** the plugin's hook. Run the three prompts.
If `marketplace add` does not accept a local folder, write down the message; we will test this one after the project is on GitHub.

### Cursor plugin

Cursor loads local plugins from its own `plugins/local` folder. With Cursor on Windows connected to WSL, it is not yet known whether that folder is the Windows one or the Ubuntu one, and whether the plugin's hook (a shell script) runs inside Ubuntu. Try Ubuntu first:

```sh
mkdir -p ~/.cursor/plugins/local
cp -r ~/cloudundo-src/plugin ~/.cursor/plugins/local/cloudundo
```

Restart Cursor (or "Developer: Reload Window"), open **Customize**, and check that cloudundo is listed with a hook. Run the three prompts.

- ⚠️ If **every** command is blocked, even `ls`, the hook script could not run (the hook fails closed). Write down the error from Output > Hooks and remove the folder again. This is exactly what this test is for.
- If cloudundo is not listed, copy the same `plugin` folder to `C:\Users\<you>\.cursor\plugins\local\cloudundo` instead and reload.

## Clean up

Empty and delete the `cloudundo-hooktest-…` bucket in the S3 console.
To remove a hook later, delete the cloudundo entry from the settings file; the original is saved next to it as `*.before-cloudundo`. To remove a plugin, use the agent's plugin menu (Claude Code `/plugin`, Codex `/plugins`) or delete the Cursor `plugins/local/cloudundo` folder.
