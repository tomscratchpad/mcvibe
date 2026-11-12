// Package kexec runs commands in a sandbox over the Kubernetes exec API
// (what `kubectl exec` does), with the caller's identity from internal/auth.
//
// It is the ONLY remote-access path to a sandbox. There is no SSH server, no
// listening port and no Service for access: every byte goes through the API
// server (authentication, RBAC, the developer-exec-owner admission policy,
// audit log) and the kubelet.
package kexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
)

// Container is the agent container name in the platform SandboxTemplate.
const Container = "agent"

type Target struct {
	Config    *rest.Config
	Client    kubernetes.Interface
	Namespace string
	Pod       string
}

type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	TTY    bool
}

// ExitCode extracts the remote command's exit status from an Exec error
// (0 for nil, -1 if the failure was not the remote command's).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ce exec.CodeExitError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return -1
}

// Exec runs cmd in the agent container and streams stdio.
func Exec(ctx context.Context, t Target, cmd []string, s Streams) error {
	req := t.Client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(t.Namespace).Name(t.Pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: Container,
			Command:   cmd,
			Stdin:     s.Stdin != nil,
			Stdout:    s.Stdout != nil,
			Stderr:    s.Stderr != nil && !s.TTY, // a TTY merges stderr into stdout
			TTY:       s.TTY,
		}, scheme.ParameterCodec)

	// WebSocket (KEP-4006) first, SPDY fallback for older API servers.
	ws, err := remotecommand.NewWebSocketExecutor(t.Config, "GET", req.URL().String())
	if err != nil {
		return fmt.Errorf("websocket executor: %w", err)
	}
	spdy, err := remotecommand.NewSPDYExecutor(t.Config, http.MethodPost, req.URL())
	if err != nil {
		return fmt.Errorf("spdy executor: %w", err)
	}
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: s.Stdin, Stdout: s.Stdout, Tty: s.TTY}
	if !s.TTY {
		opts.Stderr = s.Stderr
	}
	if s.TTY {
		fd := int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			return errors.New("--tty needs a terminal on stdin")
		}
		old, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer term.Restore(fd, old)
		q := newSizeQueue(ctx, fd)
		defer q.stop()
		opts.TerminalSizeQueue = q
	}
	return ex.StreamWithContext(ctx, opts)
}

// sizeQueue forwards the local terminal size (initially and on SIGWINCH).
type sizeQueue struct {
	ch   chan remotecommand.TerminalSize
	sig  chan os.Signal
	done chan struct{}
}

func newSizeQueue(ctx context.Context, fd int) *sizeQueue {
	q := &sizeQueue{ch: make(chan remotecommand.TerminalSize, 1), sig: make(chan os.Signal, 1), done: make(chan struct{})}
	signal.Notify(q.sig, syscall.SIGWINCH)
	send := func() {
		if w, h, err := term.GetSize(fd); err == nil {
			select {
			case q.ch <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}:
			default:
			}
		}
	}
	send()
	go func() {
		for {
			select {
			case <-q.sig:
				send()
			case <-ctx.Done():
				return
			case <-q.done:
				return
			}
		}
	}()
	return q
}

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case s := <-q.ch:
		return &s
	case <-q.done:
		return nil
	}
}

func (q *sizeQueue) stop() { signal.Stop(q.sig); close(q.done) }
