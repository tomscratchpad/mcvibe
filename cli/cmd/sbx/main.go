// sbx: developer CLI for disposable agent sandboxes.
//
//	sbx create <name> [--ttl 8h] [--template claude-code] [--no-hc]
//	sbx connect <name>               # show an existing sandbox in this computer's Herdr app
//	sbx list
//	sbx shell <name>                 # interactive shell (TTY) over k8s exec
//	sbx exec [-t] <name> -- cmd...   # run a command; also herdr-core's transport
//	sbx delete <name>
//	sbx prune                        # forget hc machines of sandboxes that expired
//	sbx whoami
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/agent-platform/sbx/internal/auth"
	"github.com/agent-platform/sbx/internal/hc"
	"github.com/agent-platform/sbx/internal/kexec"
	"github.com/agent-platform/sbx/internal/sandbox"
)

var version = "dev"

type globals struct {
	namespace, authName, kubeconfig, kubeContext, as string
	asGroups                                         []string
}

type env struct {
	cfg *rest.Config
	cs  kubernetes.Interface
	sb  *sandbox.Client
}

func (g *globals) connect(ctx context.Context) (*env, error) {
	p, err := auth.New(g.authName, g.kubeconfig, g.kubeContext, g.as, g.asGroups)
	if err != nil {
		return nil, err
	}
	cfg, err := p.RESTConfig(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	user, err := auth.WhoAmI(ctx, cs)
	if err != nil {
		return nil, err
	}
	ns := g.namespace
	if ns == "" {
		ns = sandbox.Namespace(user)
	}
	return &env{cfg: cfg, cs: cs, sb: &sandbox.Client{Namespace: ns, Kube: cs, Dyn: dyn, Owner: user}}, nil
}

// target resolves a ready sandbox to its pod.
func (e *env) target(ctx context.Context, name string) (kexec.Target, error) {
	info, err := e.sb.Get(ctx, name)
	if err != nil {
		return kexec.Target{}, err
	}
	if !info.Ready || info.Pod == "" {
		return kexec.Target{}, fmt.Errorf("sandbox %q is not ready (%s)", name, info.Reason)
	}
	return kexec.Target{Config: e.cfg, Client: e.cs, Namespace: e.sb.Namespace, Pod: info.Pod}, nil
}

// exitError carries a remote exit code up to main without printing.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := root().ExecuteContext(ctx)
	stop()
	if ee, ok := err.(exitError); ok {
		os.Exit(ee.code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbx:", err)
		if hint := notOnboardedHint(err); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
		os.Exit(1)
	}
}

// notOnboardedHint explains the most common first-run error: the caller has
// no sbx-<slug> namespace because developer.sh never onboarded them (e.g. a
// cluster admin running sbx with the admin kubeconfig, user "system:admin").
func notOnboardedHint(err error) string {
	var se *apierrors.StatusError
	if !errors.As(err, &se) || !apierrors.IsNotFound(se) {
		return ""
	}
	d := se.Status().Details
	if d == nil || d.Kind != "namespaces" || !strings.HasPrefix(d.Name, "sbx-") {
		return ""
	}
	return fmt.Sprintf(`hint: namespace %s does not exist, so this identity is not an onboarded developer.
  developer:      ask an admin to run  k8s/developers/developer.sh add <your-username>
  admin (demo):   act as a developer   sbx --as alice@corp.com %s
  check identity: sbx whoami`, d.Name, strings.Join(os.Args[1:], " "))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func root() *cobra.Command {
	g := &globals{}
	c := &cobra.Command{
		Use:           "sbx",
		Short:         "Create, use and destroy disposable agent sandboxes",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	f := c.PersistentFlags()
	f.StringVarP(&g.namespace, "namespace", "n", os.Getenv("SBX_NAMESPACE"), "sandbox namespace (default: your own, sbx-<you>)")
	f.StringVar(&g.authName, "auth", envOr("SBX_AUTH", "kubeconfig"), "cluster auth provider: kubeconfig (demo) | oidc (placeholder)")
	f.StringVar(&g.kubeconfig, "kubeconfig", "", "kubeconfig path (auth=kubeconfig)")
	f.StringVar(&g.kubeContext, "context", "", "kubeconfig context (auth=kubeconfig)")
	f.StringVar(&g.as, "as", os.Getenv("SBX_AS"), "DEMO ONLY: impersonate this developer (needs an admin kubeconfig)")
	f.StringSliceVar(&g.asGroups, "as-group", nil, "DEMO ONLY: groups for --as")
	_ = f.MarkHidden("as")
	_ = f.MarkHidden("as-group")
	c.AddCommand(createCmd(g), connectCmd(g), deleteCmd(g), pushCmd(g), pullCmd(g), listCmd(g), execCmd(g), shellCmd(g), pruneCmd(g), whoamiCmd(g))
	return c
}

func createCmd(g *globals) *cobra.Command {
	var template string
	var ttl, timeout time.Duration
	var noHC, hcTemp, noPush bool
	var repoDir string
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a sandbox, copy the current git repo into it, register it as a herdr-core machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, name := cmd.Context(), args[0]
			if err := sandbox.ValidateName(name); err != nil {
				return err
			}
			e, err := g.connect(ctx)
			if err != nil {
				return err
			}
			logf("creating sandbox %q in %s (owner %s, template %s, ttl %s)", name, e.sb.Namespace, e.sb.Owner, template, ttl)
			if err := e.sb.Create(ctx, sandbox.CreateOpts{Name: name, Template: template, TTL: ttl}); err != nil {
				return err
			}
			info, err := e.sb.WaitReady(ctx, name, timeout, func(s string) { logf("  status: %s", s) })
			if err != nil {
				return fmt.Errorf("%w\n  inspect: kubectl -n %s describe sandboxclaim %s\n  clean up: sbx delete %s",
					err, e.sb.Namespace, name, name)
			}
			logf("pod %s ready. Shell: sbx shell %s", info.Pod, name)
			if repo := localRepo(repoDir); repo != "" && !noPush {
				t, err := e.target(ctx, name)
				if err == nil {
					err = pushCode(ctx, t, repo, name)
				}
				if err != nil {
					return fmt.Errorf("%w\n  sandbox is ready; retry with: sbx push %s", err, name)
				}
			} else if repo == "" {
				logf("not in a git repository: no code copied (run sbx create from your repo, or sbx push later)")
			}
			if noHC {
				return nil
			}
			if !hc.Available() {
				logf("hc (herdr-core) not found on PATH; skipping machine registration")
				return nil
			}
			via, err := g.viaCommand(e.sb.Namespace, name)
			if err != nil {
				return err
			}
			m, err := hc.Add(name, via, hcTemp)
			if err != nil {
				return fmt.Errorf("%w\n  sandbox is ready; retry with: hc machine add --name %s --via %q", err, hc.MachineName(name), via)
			}
			logf("registered herdr-core machine %q (hc %s, %s)", m.Name, m.Version, m.Target)
			logf("  e.g. hc run --on %s --bg claude", m.Name)
			return nil
		},
	}
	c.Flags().StringVar(&template, "template", "claude-code", "platform sandbox template (warm pool) to claim")
	c.Flags().DurationVar(&ttl, "ttl", 8*time.Hour, "hard expiry; the platform deletes the sandbox after this (0 = none)")
	c.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait for the sandbox to become ready")
	c.Flags().BoolVar(&noHC, "no-hc", false, "do not register the sandbox in herdr-core")
	c.Flags().BoolVar(&hcTemp, "hc-temp", false, "register as an hc --temp machine (hidden from the Herdr desktop app)")
	c.Flags().BoolVar(&noPush, "no-push", false, "do not copy the current git repo into the sandbox")
	c.Flags().StringVar(&repoDir, "repo", ".", "local git repository to copy in")
	return c
}

// viaCommand is the sh line herdr-core runs to reach the sandbox; hc appends
// its remote shell line as the last argument (after `sh -c`). It pins the
// namespace and identity flags so the transport acts as the same developer.
func (g *globals) viaCommand(ns, name string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}
	parts := []string{shq(self)}
	if g.kubeconfig != "" {
		parts = append(parts, "--kubeconfig", shq(g.kubeconfig))
	}
	if g.kubeContext != "" {
		parts = append(parts, "--context", shq(g.kubeContext))
	}
	if g.authName != "kubeconfig" {
		parts = append(parts, "--auth", shq(g.authName))
	}
	if g.as != "" {
		parts = append(parts, "--as", shq(g.as))
		for _, grp := range g.asGroups {
			parts = append(parts, "--as-group", shq(grp))
		}
	}
	parts = append(parts, "--namespace", ns, "exec", name, "--", "sh", "-c")
	return strings.Join(parts, " "), nil
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// connectCmd registers an existing sandbox in this computer's herdr-core, so
// the Herdr app on a second laptop shows it too (sessions are not tied to the
// computer that created them).
func connectCmd(g *globals) *cobra.Command {
	var hcTemp bool
	c := &cobra.Command{
		Use:   "connect <name>",
		Short: "Show an existing sandbox in this computer's Herdr app (register the hc machine)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, name := cmd.Context(), args[0]
			if !hc.Available() {
				return fmt.Errorf("hc (herdr-core) not found on PATH; install the Herdr app first")
			}
			e, err := g.connect(ctx)
			if err != nil {
				return err
			}
			if _, err := e.target(ctx, name); err != nil {
				return err
			}
			forgetMachine(name) // replace a stale registration (e.g. moved sbx binary)
			via, err := g.viaCommand(e.sb.Namespace, name)
			if err != nil {
				return err
			}
			m, err := hc.Add(name, via, hcTemp)
			if err != nil {
				return err
			}
			logf("registered herdr-core machine %q; it is now in the Herdr app sidebar", m.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&hcTemp, "hc-temp", false, "register as an hc --temp machine (hidden from the Herdr desktop app)")
	return c
}

func execCmd(g *globals) *cobra.Command {
	var tty bool
	c := &cobra.Command{
		Use:   "exec [-t] <name> -- command [args...]",
		Short: "Run a command in a sandbox over the Kubernetes exec API (stdio passthrough)",
		Long: "Runs a command in the sandbox's agent container, like kubectl exec -i. stdout\n" +
			"carries only the command's output and the exit code is the command's, so it\n" +
			"can serve as a byte pipe (herdr-core uses it as its machine transport).",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := g.connect(ctx)
			if err != nil {
				return err
			}
			t, err := e.target(ctx, args[0])
			if err != nil {
				return err
			}
			// With interspersed flags off, cobra keeps a literal "--".
			command := args[1:]
			if command[0] == "--" {
				command = command[1:]
			}
			if len(command) == 0 {
				return fmt.Errorf("no command given")
			}
			err = kexec.Exec(ctx, t, command, kexec.Streams{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, TTY: tty})
			if code := kexec.ExitCode(err); code > 0 {
				return exitError{code}
			}
			return err
		},
	}
	c.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY (interactive programs)")
	c.Flags().SetInterspersed(false)
	return c
}

func shellCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "shell <name>",
		Short: "Open an interactive shell in a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := g.connect(ctx)
			if err != nil {
				return err
			}
			t, err := e.target(ctx, args[0])
			if err != nil {
				return err
			}
			err = kexec.Exec(ctx, t, []string{"bash", "-l"}, kexec.Streams{Stdin: os.Stdin, Stdout: os.Stdout, TTY: true})
			if code := kexec.ExitCode(err); code > 0 {
				return exitError{code}
			}
			return err
		},
	}
}

func deleteCmd(g *globals) *cobra.Command {
	var noWait, noPull bool
	var repoDir string
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"destroy", "rm"},
		Short:   "Fetch the sandbox's work into branch sbx/<name>, unregister from herdr-core, destroy the sandbox",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, name := cmd.Context(), args[0]
			if err := sandbox.ValidateName(name); err != nil {
				return err
			}
			e, err := g.connect(ctx)
			if err != nil {
				return err
			}
			// Save the agent's work first; refuse to destroy it if that fails.
			if repo := localRepo(repoDir); repo != "" && !noPull {
				if t, err := e.target(ctx, name); err != nil {
					logf("not pulling: %v", err)
				} else if _, err := pullCode(ctx, t, repo, name); err != nil {
					return fmt.Errorf("%w\n  sandbox NOT deleted. Discard its work with: sbx delete --no-pull %s", err, name)
				}
			}
			// Local cleanup first and unconditionally, so a sandbox already
			// reaped by its TTL still gets its machine entry removed.
			forgetMachine(name)
			logf("deleting sandbox %q in %s", name, e.sb.Namespace)
			if err := e.sb.Delete(ctx, name, !noWait); err != nil {
				return err
			}
			logf("sandbox %q destroyed", name)
			return nil
		},
	}
	c.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for the pod to terminate")
	c.Flags().BoolVar(&noPull, "no-pull", false, "do not fetch the sandbox's work before deleting (discard it)")
	c.Flags().StringVar(&repoDir, "repo", ".", "local git repository to fetch into")
	return c
}

func forgetMachine(name string) {
	if !hc.Available() {
		return
	}
	if ok, err := hc.Remove(name); err != nil {
		logf("warning: %v", err)
	} else if ok {
		logf("unregistered herdr-core machine %q", hc.MachineName(name))
	}
}

func listCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your sandboxes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			l, err := e.sb.List(cmd.Context())
			if err != nil {
				return err
			}
			machines := map[string]string{}
			if hc.Available() {
				if ms, err := hc.Managed(e.sb.Namespace); err == nil {
					for _, m := range ms {
						machines[m.Name] = m.State
					}
				}
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tHC MACHINE\tTEMPLATE\tAGE\tEXPIRES")
			for _, i := range l {
				st := i.Reason
				if i.Ready {
					st = "Ready"
				}
				m := "-"
				if s, ok := machines[hc.MachineName(i.Name)]; ok {
					m = hc.MachineName(i.Name) + " (" + s + ")"
				}
				exp := "-"
				if t, err := time.Parse(time.RFC3339, i.Expires); err == nil {
					exp = "in " + time.Until(t).Round(time.Minute).String()
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", i.Name, st, m, i.Template,
					time.Since(i.Created).Round(time.Second), exp)
			}
			return tw.Flush()
		},
	}
}

func pruneCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Forget herdr-core machines whose sandbox no longer exists (e.g. expired)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			l, err := e.sb.List(cmd.Context())
			if err != nil {
				return err
			}
			live := map[string]bool{}
			for _, i := range l {
				live[hc.MachineName(i.Name)] = true
			}
			ms, err := hc.Managed(e.sb.Namespace)
			if err != nil {
				return err
			}
			for _, m := range ms {
				if !live[m.Name] {
					forgetMachine(strings.TrimPrefix(m.Name, hc.Prefix))
				}
			}
			return nil
		},
	}
}

func whoamiCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the identity the cluster sees and your sandbox namespace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("user:      %s\nnamespace: %s\n", e.sb.Owner, e.sb.Namespace)
			return nil
		},
	}
}

func logf(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
