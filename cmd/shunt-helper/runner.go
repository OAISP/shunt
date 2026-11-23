package main

import "os/exec"

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
