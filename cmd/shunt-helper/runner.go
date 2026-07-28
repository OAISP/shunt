package main

import (
	"os/exec"
	"strings"
)

// runner is how the helper reaches docker.
//
// It exists so the deploy path can be tested. Every interesting failure in this
// binary — a container that will not start, a swap that dies halfway, a health
// check that never passes — is a particular sequence of docker invocations
// going a particular way, and none of that was reachable from a test while the
// calls were literal exec.Command. Substituting a fake here is what makes the
// riskiest code in the project assertable without a Docker daemon.
type runner interface {
	// Run executes a command and returns its combined output.
	Run(name string, args ...string) (string, error)
	// Ok reports whether a command succeeded, discarding its output.
	Ok(name string, args ...string) bool
}

// execRunner is the real implementation: it shells out.
type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func (execRunner) Ok(name string, args ...string) bool {
	return exec.Command(name, args...).Run() == nil
}

// docker is the runner the helper uses. Tests replace it and restore it.
var docker runner = execRunner{}

// fakeRunner records what it was asked to do and replays scripted answers.
//
// Keyed on a prefix of the command line rather than the whole thing, because
// most calls embed a release id or container name a test should not have to
// spell out in full.
type fakeRunner struct {
	calls   []string
	replies map[string]fakeReply
}

type fakeReply struct {
	out string
	err error
}

func newFake() *fakeRunner { return &fakeRunner{replies: map[string]fakeReply{}} }

// on scripts a reply for any command whose line contains match.
func (f *fakeRunner) on(match, out string, err error) *fakeRunner {
	f.replies[match] = fakeReply{out: out, err: err}
	return f
}

func (f *fakeRunner) Run(name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	for match, reply := range f.replies {
		if strings.Contains(line, match) {
			return reply.out, reply.err
		}
	}
	return "", nil
}

func (f *fakeRunner) Ok(name string, args ...string) bool {
	_, err := f.Run(name, args...)
	return err == nil
}

// did reports whether any recorded call contains every one of parts.
func (f *fakeRunner) did(parts ...string) bool {
	for _, c := range f.calls {
		matched := true
		for _, p := range parts {
			if !strings.Contains(c, p) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
