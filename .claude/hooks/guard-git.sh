#!/bin/sh
# PreToolUse(Bash): refuse commands that silently throw away uncommitted work.
# CLAUDE.md: undo a deliberate break with a reverse edit, because checking the
# file out reverts it to HEAD and destroys the real change being tested.
# Exit 2 blocks the call and hands stderr back to Claude.
cmd=$(cat)
s='[[:space:]]'
end="($s|\"|;|&|\\||\$)"
for pat in \
  "git$s+checkout$s+([^[:space:]]+$s+)?--$s" \
  "git$s+checkout$s+\\.$end" \
  "git$s+restore$s+([^-]|--worktree|--source)" \
  "git$s+reset$s+--hard" \
  "git$s+stash$s*(\"|;|&|\\||\$)" \
  "git$s+stash$s+(push|save|-u|-k|-a|--include-untracked|--keep-index|--all)$end" \
  "git$s+clean$s+-[a-zA-Z]*f"
do
  if printf '%s' "$cmd" | grep -Eq "$pat"; then
    echo "Blocked: this git command discards uncommitted changes, and this tree usually carries real uncommitted work. Undo a deliberate break with a reverse Edit instead (see CLAUDE.md > Verification). If the user explicitly asked for this, ask them to run it themselves with '! <command>'." >&2
    exit 2
  fi
done
exit 0
