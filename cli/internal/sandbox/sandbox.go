// Package sandbox manages SandboxClaims (agent-sandbox, kubernetes-sigs).
//
// Developers never write pod specs: a claim only names a platform-owned
// SandboxTemplate (via its warm pool), so isolation, network policy and
// resource limits stay with the platform team.
package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	claimGVR   = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
	sandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
)

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelOwner     = "agent-platform/owner"
	LabelName      = "agent-platform/sandbox"
	AnnOwner       = "agent-platform/owner-username"
	ManagedBy      = "sbx"
)

type Client struct {
	Namespace string
	Kube      kubernetes.Interface
	Dyn       dynamic.Interface
	Owner     string // authenticated username (from SelfSubjectReview)
}

// Info is what sbx needs to know about one sandbox.
type Info struct {
	Name     string // chosen by the developer; also the SandboxClaim name
	Template string
	Ready    bool
	Reason   string
	Message  string
	Pod      string
	Expires  string
	Created  time.Time
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,26}[a-z0-9])?$`)

// ValidateName keeps names DNS-safe; 28 chars so that the herdr-core machine
// name "sbx-<name>" stays within hc's 32-char limit.
func ValidateName(n string) error {
	if !nameRe.MatchString(n) {
		return fmt.Errorf("invalid sandbox name %q: use 1-28 chars of [a-z0-9-]", n)
	}
	return nil
}

// OwnerSlug turns "oidc:alice@corp.com" into "alice". Must match slug() in
// k8s/developers/developer.sh, which creates the namespace.
func OwnerSlug(user string) string {
	s := strings.ToLower(user)
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '@'); i > 0 {
		s = s[:i]
	}
	s = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 20 {
		s = strings.Trim(s[:20], "-")
	}
	if s == "" {
		s = "anon"
	}
	return s
}

// Namespace is the developer's own namespace (k8s/developers): RBAC and the
// owner admission policies only let its owner use it.
func Namespace(user string) string { return "sbx-" + OwnerSlug(user) }

type CreateOpts struct {
	Name     string
	Template string // platform SandboxWarmPool (and template) name
	TTL      time.Duration
}

func (c *Client) Create(ctx context.Context, o CreateOpts) error {
	pool := o.Template
	labels := map[string]any{
		LabelManagedBy: ManagedBy,
		LabelOwner:     OwnerSlug(c.Owner),
		LabelName:      o.Name,
	}
	// No additionalPodMetadata: agent-sandbox only accepts allowlisted label
	// domains there, and ownership is the namespace anyway.
	spec := map[string]any{"warmPoolRef": map[string]any{"name": pool}}
	// Hard expiry: the controller deletes the sandbox at shutdownTime even if
	// the developer forgets it (runaway-spend control, not a convenience).
	if o.TTL > 0 {
		spec["lifecycle"] = map[string]any{
			"shutdownPolicy": "Delete",
			"shutdownTime":   time.Now().Add(o.TTL).UTC().Format(time.RFC3339),
		}
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
		"kind":       "SandboxClaim",
		"metadata": map[string]any{
			"name":      o.Name,
			"namespace": c.Namespace,
			"labels":    labels,
			// Checked by the developer-sandboxclaim-owner admission policy.
			"annotations": map[string]any{AnnOwner: c.Owner},
		},
		"spec": spec,
	}}
	_, err := c.Dyn.Resource(claimGVR).Namespace(c.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("sandbox %q already exists", o.Name)
	}
	return err
}

// WaitReady polls until the claim is Ready and its pod is Running+Ready.
func (c *Client) WaitReady(ctx context.Context, name string, timeout time.Duration, progress func(string)) (*Info, error) {
	var last string
	var info *Info
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		i, err := c.Get(ctx, name)
		if err != nil {
			return false, err
		}
		info = i
		if i.Reason != last && progress != nil {
			progress(i.Reason)
			last = i.Reason
		}
		// Terminal controller verdicts (InvalidMetadata, InvalidTemplate...):
		// fail now instead of waiting out the timeout.
		if strings.HasPrefix(i.Reason, "Invalid") {
			return false, fmt.Errorf("%s: %s", i.Reason, i.Message)
		}
		return i.Ready && i.Pod != "", nil
	})
	if err != nil {
		reason := ""
		if info != nil {
			reason = " (last status: " + info.Reason + ")"
		}
		return info, fmt.Errorf("sandbox %q not ready: %w%s", name, err, reason)
	}
	return info, nil
}

func (c *Client) Get(ctx context.Context, name string) (*Info, error) {
	u, err := c.Dyn.Resource(claimGVR).Namespace(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("sandbox %q not found", name)
	}
	if err != nil {
		return nil, err
	}
	return c.info(ctx, u)
}

func (c *Client) info(ctx context.Context, u *unstructured.Unstructured) (*Info, error) {
	i := &Info{
		Name:    u.GetName(),
		Created: u.GetCreationTimestamp().Time,
		Reason:  "Pending",
	}
	i.Template, _, _ = unstructured.NestedString(u.Object, "spec", "warmPoolRef", "name")
	i.Expires, _, _ = unstructured.NestedString(u.Object, "spec", "lifecycle", "shutdownTime")
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" {
			i.Ready = m["status"] == "True"
			if r, _ := m["reason"].(string); r != "" {
				i.Reason = r
			}
			i.Message, _ = m["message"].(string)
		}
	}
	sbName, _, _ := unstructured.NestedString(u.Object, "status", "sandbox", "name")
	if sbName == "" {
		return i, nil
	}
	pod, err := c.podFor(ctx, sbName)
	if err != nil {
		return nil, err
	}
	if pod != nil {
		i.Pod = pod.Name
		if !podReady(pod) {
			i.Ready = false
			i.Reason = "Pod" + string(pod.Status.Phase)
		}
	} else {
		i.Ready = false
	}
	return i, nil
}

// podFor resolves Sandbox -> Pod through the Sandbox's status.selector rather
// than assuming pod name == sandbox name (true today, not a contract).
func (c *Client) podFor(ctx context.Context, sandbox string) (*corev1.Pod, error) {
	sb, err := c.Dyn.Resource(sandboxGVR).Namespace(c.Namespace).Get(ctx, sandbox, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sel, _, _ := unstructured.NestedString(sb.Object, "status", "selector")
	if sel == "" {
		return nil, nil
	}
	pods, err := c.Kube.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// List returns the sbx-managed sandboxes in the developer's namespace.
func (c *Client) List(ctx context.Context) ([]*Info, error) {
	l, err := c.Dyn.Resource(claimGVR).Namespace(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedBy + "," + LabelOwner + "=" + OwnerSlug(c.Owner),
	})
	if err != nil {
		return nil, err
	}
	var out []*Info
	for i := range l.Items {
		inf, err := c.info(ctx, &l.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, inf)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// Delete removes the claim; the controller garbage-collects Sandbox and Pod.
// With wait=true it blocks until the pod is gone, so "deleted" means the
// workload (and anything in it) really no longer exists.
func (c *Client) Delete(ctx context.Context, name string, waitGone bool) error {
	info, _ := c.Get(ctx, name)
	fg := metav1.DeletePropagationForeground
	err := c.Dyn.Resource(claimGVR).Namespace(c.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("sandbox %q not found", name)
	}
	if err != nil || !waitGone || info == nil || info.Pod == "" {
		return err
	}
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := c.Kube.CoreV1().Pods(c.Namespace).Get(ctx, info.Pod, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
}
