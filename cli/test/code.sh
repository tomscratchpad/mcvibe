#!/usr/bin/env bash
# sbx create/pull/delete code round trip against the real cluster.
#   cli/test/code.sh            (admin kubeconfig; acts as alice@corp.com)
# Creates and deletes sandbox "codetest" in sbx-alice. No Claude login needed:
# the "agent" edits are made with sbx exec.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
SBX=("$HERE/../bin/sbx" --as "${AS:-alice@corp.com}") NAME=codetest
WORK=$(mktemp -d) pass=0 fail=0
ok()  { echo "  PASS  $1"; pass=$((pass+1)); }
bad() { echo "  FAIL  $1"; [[ -n ${2:-} ]] && printf '        %s\n' "$2"; fail=$((fail+1)); }
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi; }
in_sbx() { "${SBX[@]}" exec "$NAME" -- sh -c "cd /workspace/repo && $1"; }
cleanup() { "${SBX[@]}" delete --no-pull "$NAME" >/dev/null 2>&1; rm -rf "$WORK"; }
trap cleanup EXIT

# A local repo: main with one commit, plus a pre-existing secret that is not committed.
cd "$WORK" && git init -q -b main app && cd app
git config user.name dev && git config user.email dev@corp.com
printf 'print("hello")\n' > app.py && git add . && git commit -qm "initial"
echo "API_TOKEN=local-only" > .env

echo "== create = sandbox + push"
out=$("${SBX[@]}" create "$NAME" --no-hc --ttl 30m 2>&1); echo "$out" | sed 's/^/        /' | tail -3
check "create pushed the repo"                    grep -q "pushed .* on branch agent/$NAME" <<<"$out"
check "sandbox has the code on agent/$NAME"       in_sbx "test \"\$(git branch --show-current)\" = agent/$NAME && grep -q hello app.py"
check "sandbox repo has no git remote"            in_sbx 'test -z "$(git remote)"'
check "uncommitted local files are not copied"    in_sbx 'test ! -e .env'

echo "== agent works in the sandbox; the engineer keeps working locally"
in_sbx 'printf "def add(a, b):\n    return a + b\n" > util.py && git add util.py && git commit -qm "agent: add util"' >/dev/null
in_sbx 'echo "# TODO from agent" >> app.py && printf "#!/bin/sh\ntouch /tmp/pwned\n" > .git/hooks/post-merge && chmod +x .git/hooks/post-merge' >/dev/null
echo "docs" > README.md && git add README.md && git commit -qm "local: readme"
echo "local edit" >> README.md   # uncommitted local change

echo "== pull (like git fetch: nothing local is touched)"
out=$("${SBX[@]}" pull "$NAME" 2>&1); echo "$out" | sed 's/^/        /' | head -2
check "pull created local branch sbx/$NAME"       git rev-parse -q --verify "sbx/$NAME"
check "agent commit + WIP commit came back"       test "$(git rev-list --count main..sbx/$NAME)" = 2
check "WIP commit holds uncommitted sandbox work" sh -c "git show sbx/$NAME:app.py | grep -q 'TODO from agent'"
check "current branch unchanged"                  test "$(git branch --show-current)" = main
check "uncommitted local edit untouched"          grep -q "local edit" README.md
check "sandbox files untouched by the pull"       in_sbx 'git status --porcelain | grep -q "M app.py"'
check "sandbox hooks are not copied"              test ! -e .git/hooks/post-merge

echo "== review and merge with plain git"
check "merge sbx/$NAME into main (no conflict)"   git merge -q --no-edit "sbx/$NAME"
check "merged tree has local and agent work"      sh -c 'test -f README.md && test -f util.py'
check "no sandbox hook ran on merge"              test ! -e /tmp/pwned
check "uncommitted local edit survived the merge" grep -q "local edit" README.md

echo "== delete = pull + destroy"
in_sbx 'echo "x = 1" > more.py && git add more.py && git commit -qm "agent: more"' >/dev/null
out=$("${SBX[@]}" delete "$NAME" 2>&1); echo "$out" | sed 's/^/        /' | head -2
check "delete pulled the last work first"         git cat-file -e "sbx/$NAME:more.py"
check "sandbox destroyed"                         sh -c "! '${SBX[0]}' --as '${SBX[2]}' list | grep -q '^$NAME '"

echo "== $pass passed, $fail failed"
[[ $fail -eq 0 ]]
