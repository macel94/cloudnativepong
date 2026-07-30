---
applyTo: '**'
---

# Delegation Workflow

When the user says "delegate work" or "delegate to subagents", use the following workflow:

1. **Spawn pi sub-sessions in tmux** with a specific provider/model per subagent (e.g., `deepseek/deepseek-v4-flash`)
2. **Parallelize independent tasks** — each subagent gets a clear, self-contained task with specific instructions
3. **Use this command pattern:**
   ```bash
   tmux new-session -d -s <session-name> \
     "pi --provider <provider> --model <model> '<task description>'"
   ```
4. **Monitor progress** via `tmux capture-pane -p -t <session-name>`
5. **List sessions** with `tmux list-sessions`

The user has accounts on the providers configured in their pi setup. If an API key is missing, check `/root/.pi/` for configuration files and use `/login` or environment variables.

**Current config:** Provider is `openrouter`, models are `deepseek/deepseek-v4-pro` (main) and `deepseek/deepseek-v4-flash` (subagents). Always use `--provider openrouter --model deepseek/deepseek-v4-flash` when spawning subagents.