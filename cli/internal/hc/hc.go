// Package hc registers sandboxes as herdr-core machines.
//
// herdr-core (`hc`) can reach a machine through any command that passes bytes
// both ways (`hc machine add --via <cmd>`): it appends a shell line
// (`sh -c '... exec herdr-core connect'`) as the last argument. We give it
//
//	sbx exec <name> -- sh -c
//
// so the herdr-core protocol runs over a Kubernetes exec stream. No SSH.
// The hc binary is driven through its CLI with --json (its supported
// interface); machine profiles hold no credentials, the transport command
// authenticates on every connect.
package hc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Prefix of every machine name sbx manages.
const Prefix = "sbx-"

func MachineName(sandbox string) string { return Prefix + sandbox }

type Machine struct {
	Name      string `json:"name"`
	Target    string `json:"target"`
	State     string `json:"state"`
	Temporary bool   `json:"temporary"`
	Via       bool   `json:"via"`
	Version   string `json:"version"`
}

// Available reports whether hc is installed on this workstation.
func Available() bool {
	_, err := exec.LookPath("hc")
	return err == nil
}

// Add registers the sandbox. via is the sh command line hc runs, with the
// remote line appended. temp adds --temp ("short-lived sandbox"): the Herdr
// desktop app HIDES temp machines (no spaces, not in its machine list), so
// sbx registers normal machines by default to make sandboxes visible there.
// No --install: the image ships a pinned herdr-core; sbx never copies a
// binary from the laptop into a sandbox.
func Add(name, via string, temp bool) (*Machine, error) {
	args := []string{"machine", "add", "--via", via, "--name", MachineName(name), "--json"}
	if temp {
		args = append(args, "--temp")
	}
	cmd := exec.Command("hc", args...)
	cmd.Stdin, cmd.Stderr = nil, os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("hc machine add: %w: %s", err, hcError(out))
	}
	var m Machine
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("parse hc output: %w", err)
	}
	return &m, nil
}

// Remove forgets the machine locally. Its terminals die with the sandbox.
// Returns false if it was not registered.
func Remove(name string) (bool, error) {
	ms, err := List()
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.Name == MachineName(name) {
			out, err := exec.Command("hc", "machine", "rm", m.Name, "--json").Output()
			if err != nil {
				return false, fmt.Errorf("hc machine rm %s: %w: %s", m.Name, err, hcError(out))
			}
			return true, nil
		}
	}
	return false, nil
}

func List() ([]Machine, error) {
	out, err := exec.Command("hc", "machine", "ls", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("hc machine ls: %w: %s", err, hcError(out))
	}
	var r struct {
		Machines []Machine `json:"machines"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("parse hc machine ls: %w", err)
	}
	return r.Machines, nil
}

// Managed returns the machines sbx created for the given namespace (matched
// on the via command line, which embeds --namespace).
func Managed(namespace string) ([]Machine, error) {
	ms, err := List()
	if err != nil {
		return nil, err
	}
	var out []Machine
	for _, m := range ms {
		if m.Via && strings.HasPrefix(m.Name, Prefix) && strings.Contains(m.Target, " --namespace "+namespace+" ") {
			out = append(out, m)
		}
	}
	return out, nil
}

func hcError(out []byte) string {
	var e struct {
		Error struct {
			Message, Hint string
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &e) == nil && e.Error.Message != "" {
		return strings.TrimSpace(e.Error.Message + " " + e.Error.Hint)
	}
	return strings.TrimSpace(string(out))
}
