# Sourced at the end of /etc/bash.bashrc (interactive bash only).
#
# Terminals opened from the Herdr desktop app start Claude Code in the
# sandbox's repo. When Claude exits, the terminal stays an ordinary shell.
#
# Only herdr-core terminals are affected: herdr-core sets HC_TERMINAL_ID in
# every terminal it runs. `sbx shell`, `kubectl exec -it` and all
# non-interactive execs (sbx push/pull, the herdr-core transport `sh -c ...
# herdr-core connect`, Claude's own Bash tool) never get here or are skipped.
#
# Opt out: SBX_NO_CLAUDE=1.

case $- in *i*) ;; *) return 0 ;; esac          # interactive shells only
[ -n "${HC_TERMINAL_ID-}" ] || return 0         # Herdr terminals only
[ -z "${SBX_NO_CLAUDE-}" ] || return 0
# Once per terminal: not again in a nested `bash`, or in Claude's subshells.
[ -z "${SBX_CLAUDE_AUTOSTARTED-}" ] || return 0
export SBX_CLAUDE_AUTOSTARTED=1
command -v claude >/dev/null 2>&1 || return 0

[ -d /workspace/repo ] && cd /workspace/repo
claude
