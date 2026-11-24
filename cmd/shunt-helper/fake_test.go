package main

import (
	"strings"
	"time"
)

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

func (f *fakeRunner) RunFor(_ time.Duration, name string, args ...string) (string, error) {
	return f.Run(name, args...)
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

// runnerFunc adapts a function to the runner interface, for tests that need a
// reply to depend on how many times it has been asked.
type runnerFunc func(name string, args ...string) (string, error)

func (r runnerFunc) Run(name string, args ...string) (string, error) { return r(name, args...) }
func (r runnerFunc) RunFor(_ time.Duration, name string, args ...string) (string, error) {
	return r(name, args...)
}
func (r runnerFunc) Ok(name string, args ...string) bool {
	_, err := r(name, args...)
	return err == nil
}
