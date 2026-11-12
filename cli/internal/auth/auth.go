// Package auth decides how sbx authenticates to the Kubernetes API.
//
// Everything sbx does (create/delete claims, open the SSH exec stream) is a
// call to the API server, so the developer's Kubernetes identity is the root
// credential of the whole tool. Kubernetes RBAC and the API server audit log
// then apply to every session operation, and the SSH key is only a second
// factor for the shell itself.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Provider produces an authenticated REST config for the platform cluster.
type Provider interface {
	Name() string
	RESTConfig(ctx context.Context) (*rest.Config, error)
}

// New returns the provider selected by name ("kubeconfig" or "oidc").
func New(name, kubeconfig, kubeContext, as string, asGroups []string) (Provider, error) {
	switch name {
	case "", "kubeconfig":
		return &Kubeconfig{Path: kubeconfig, Context: kubeContext, As: as, AsGroups: asGroups}, nil
	case "oidc":
		return &OIDC{}, nil
	default:
		return nil, fmt.Errorf("unknown auth provider %q (want kubeconfig|oidc)", name)
	}
}

// Kubeconfig uses the standard kubeconfig loading rules ($KUBECONFIG,
// ~/.kube/config, --kubeconfig/--context). DEMO ONLY: on the prototype this is
// the k3s admin certificate, i.e. cluster-admin, so per-developer RBAC is not
// exercised unless As is set. See OIDC for the production path.
//
// As/AsGroups (DEMO/TEST ONLY) impersonate a developer through the admin
// credential, so the prototype can show per-developer RBAC and admission
// without an IdP. Needs the "impersonate" verb, which developers never get.
type Kubeconfig struct {
	Path     string
	Context  string
	As       string
	AsGroups []string
}

func (k *Kubeconfig) Name() string { return "kubeconfig" }

func (k *Kubeconfig) RESTConfig(_ context.Context) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if k.Path != "" {
		rules.ExplicitPath = k.Path
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: k.Context}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cfg.UserAgent = "sbx"
	if k.As != "" {
		cfg.Impersonate = rest.ImpersonationConfig{UserName: k.As, Groups: k.AsGroups}
	}
	return cfg, nil
}

// ErrNotImplemented is returned by the OIDC placeholder.
var ErrNotImplemented = errors.New("not implemented")

// OIDC is a PLACEHOLDER for production authentication. Out of scope for the
// prototype, but required before handing sbx to developers.
//
// Intended design:
//   - The API server trusts the company IdP (structured authentication config,
//     AuthenticationConfiguration / --oidc-issuer-url) with claim mappings
//     username=email and groups=groups, and a dedicated audience for sbx.
//   - sbx runs the OAuth2 device-code (or PKCE loopback) flow against the IdP,
//     caches the refresh token in the OS keychain (never in a plaintext file),
//     and returns a rest.Config with a short-lived ID token as BearerToken plus
//     a WrapTransport that refreshes it. Equivalent to shipping kubelogin as an
//     exec credential plugin, without the extra binary.
//   - The cluster CA is pinned in the sbx release (or fetched from a signed
//     platform discovery endpoint), not trusted from the developer's kubeconfig.
//   - Authorization (implemented, k8s/developers/): one namespace per
//     developer, the "sandbox-developer" ClusterRole bound to the username in
//     that namespace only, and ValidatingAdmissionPolicies checking the
//     namespace owner annotation against request.userInfo.username for claims
//     and pods/exec. The username must be exactly what the IdP mapping yields
//     (e.g. "oidc:alice@corp.com").
//   - Longer term, a session broker owns pods/exec, so access can be recorded
//     and revoked instantly (open exec streams outlive token expiry).
type OIDC struct {
	Issuer   string
	ClientID string
}

func (o *OIDC) Name() string { return "oidc" }

func (o *OIDC) RESTConfig(_ context.Context) (*rest.Config, error) {
	return nil, fmt.Errorf("oidc auth: %w (prototype uses --auth=kubeconfig)", ErrNotImplemented)
}

// WhoAmI asks the API server who we are (SelfSubjectReview). The answer comes
// from the server, so it reflects the real authenticated identity whatever the
// provider, and is what ownership labels are derived from.
func WhoAmI(ctx context.Context, cs kubernetes.Interface) (string, error) {
	r, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("self subject review: %w", err)
	}
	if u := r.Status.UserInfo.Username; u != "" {
		return u, nil
	}
	return os.Getenv("USER"), nil
}
