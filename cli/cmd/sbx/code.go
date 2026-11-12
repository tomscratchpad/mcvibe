package main

// Code in and out of a sandbox (demo-grade; production design: an L7 git
// gateway, see cli/README.md "Code in and out").
//
// push: the local repo's committed history (git bundle of HEAD) is streamed
//       over the Kubernetes exec API and cloned into /workspace/repo on branch
//       agent/<sandbox>. No GitHub remote, no credentials in the sandbox.
// pull: the sandbox's branch (plus a WIP commit of uncommitted work, made
//       without touching its files) comes back as a git bundle and is fetched
//       into the local branch sbx/<sandbox>. Like `git fetch`: the working
//       tree and current branch are never touched, so a pull never conflicts;
//       the engineer merges/rebases when they choose. Only commits and refs
//       cross over, never hooks or config, so nothing from the sandbox runs on
//       the laptop.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/agent-platform/sbx/internal/kexec"
)

// RepoDir is where pushed code lives in every sandbox.
const RepoDir = "/workspace/repo"

// localRepo returns the top level of the git repo containing dir, or "".
func localRepo(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func git(dir string, args ...string) (string, error) {
	var stderr bytes.Buffer
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// remote runs a shell script in the sandbox with the given stdin; stdout is returned.
func remote(ctx context.Context, t kexec.Target, script string, stdin []byte) ([]byte, string, error) {
	var out, errb bytes.Buffer
	s := kexec.Streams{Stdout: &out, Stderr: &errb}
	if stdin != nil {
		s.Stdin = bytes.NewReader(stdin)
	}
	err := kexec.Exec(ctx, t, []string{"sh", "-c", script}, s)
	return out.Bytes(), strings.TrimSpace(errb.String()), err
}

func pushCode(ctx context.Context, t kexec.Target, repo, name string) error {
	head, err := git(repo, "rev-parse", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("%s has no commits to push: %w", repo, err)
	}
	if st, _ := git(repo, "status", "--porcelain"); st != "" {
		logf("  note: uncommitted local changes are not copied (only committed history)")
	}
	bundle, err := exec.Command("git", "-C", repo, "bundle", "create", "-", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("git bundle: %w", err)
	}
	script := `set -e
test ! -e ` + RepoDir + ` || { echo "` + RepoDir + ` already exists in the sandbox" >&2; exit 3; }
b=/workspace/.sbx-push.bundle; cat > $b
git -c advice.detachedHead=false clone -q $b ` + RepoDir + `; rm -f $b
cd ` + RepoDir + `
git switch -q -c agent/` + name + `
git remote remove origin
git update-ref refs/sbx/base HEAD
git config user.name "agent (sandbox ` + name + `)"
git config user.email "agent+` + name + `@sandbox.invalid"`
	if _, serr, err := remote(ctx, t, script, bundle); err != nil {
		return fmt.Errorf("push failed: %v %s", err, serr)
	}
	logf("pushed %s (HEAD %s) to %s on branch agent/%s", repo, head, RepoDir, name)
	return nil
}

// pullCode fetches the sandbox's work into local branch sbx/<name>.
// Returns false (no error) if the sandbox has no pushed repo.
func pullCode(ctx context.Context, t kexec.Target, repo, name string) (bool, error) {
	script := `set -e
cd ` + RepoDir + ` 2>/dev/null || exit 4
git rev-parse -q --verify refs/sbx/base >/dev/null || exit 4
# Snapshot uncommitted work as a commit without touching files or the index.
export GIT_INDEX_FILE=.git/sbx-index; rm -f $GIT_INDEX_FILE
git add -A; tree=$(git write-tree); rm -f $GIT_INDEX_FILE; unset GIT_INDEX_FILE
ref=$(git rev-parse HEAD)
if [ "$tree" != "$(git rev-parse HEAD^{tree})" ]; then
  ref=$(git -c user.name=sbx -c user.email=sbx@sandbox.invalid commit-tree "$tree" -p HEAD -m "WIP: uncommitted work in sandbox ` + name + `")
  echo wip >&2
fi
git update-ref refs/sbx/export "$ref"
[ "$ref" = "$(git rev-parse refs/sbx/base)" ] && { echo nochange >&2; exit 0; }
git bundle create -q - refs/sbx/export ^refs/sbx/base`
	out, serr, err := remote(ctx, t, script, nil)
	if kexec.ExitCode(err) == 4 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pull failed: %v %s", err, serr)
	}
	if strings.Contains(serr, "nochange") {
		logf("sandbox %q: no new commits or changes since push", name)
		return true, nil
	}
	f, err := os.CreateTemp("", "sbx-pull-*.bundle")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(out); err != nil {
		return false, err
	}
	f.Close()
	branch := "sbx/" + name
	if _, err := git(repo, "fetch", "-q", f.Name(), "+refs/sbx/export:refs/heads/"+branch); err != nil {
		return false, err
	}
	n, _ := git(repo, "rev-list", "--count", "HEAD.."+branch)
	logf("pulled sandbox %q into local branch %s (%s commit(s) not in your HEAD)", name, branch, n)
	if strings.Contains(serr, "wip") {
		logf("  includes a WIP commit with the sandbox's uncommitted work")
	}
	logf("  review: git log -p HEAD..%s   merge: git merge %s", branch, branch)
	return true, nil
}

func pushCmd(g *globals) *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "push <name>",
		Short: "Copy the current git repo's committed history into the sandbox (" + RepoDir + ")",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := localRepo(dir)
			if repo == "" {
				return fmt.Errorf("%s is not in a git repository", dir)
			}
			e, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			t, err := e.target(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return pushCode(cmd.Context(), t, repo, args[0])
		},
	}
	c.Flags().StringVar(&dir, "repo", ".", "local git repository")
	return c
}

func pullCmd(g *globals) *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "pull <name>",
		Short: "Fetch the sandbox's work into local branch sbx/<name> (never touches your working tree)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := localRepo(dir)
			if repo == "" {
				return fmt.Errorf("%s is not in a git repository", dir)
			}
			e, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			t, err := e.target(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			ok, err := pullCode(cmd.Context(), t, repo, args[0])
			if err == nil && !ok {
				err = fmt.Errorf("sandbox %q has no pushed repo (%s)", args[0], RepoDir)
			}
			return err
		},
	}
	c.Flags().StringVar(&dir, "repo", ".", "local git repository")
	return c
}
