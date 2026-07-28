package main

import (
	"strings"
	"testing"

	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/release"
)

func TestKindSuffix(t *testing.T) {
	if got := kindSuffix(true); !strings.Contains(got, "directory") {
		t.Errorf("kindSuffix(true) = %q, want it to say directory", got)
	}
	if got := kindSuffix(false); got != "" {
		t.Errorf("kindSuffix(false) = %q, want empty", got)
	}
}

// resolveContainer picks what `shunt exec` attaches to. During a blue/green
// overlap two containers carry the same service label, and attaching to the one
// being retired would show an operator the code they are replacing.
func TestResolveContainerPrefersTheActiveRelease(t *testing.T) {
	state := &engine.RemoteState{
		Ledger: &release.Ledger{Current: "r2"},
		Containers: []engine.Container{
			{Name: "demo-app-r1", Service: "app", Release: "r1", State: "running"},
			{Name: "demo-app-r2", Service: "app", Release: "r2", State: "running"},
		},
	}
	got, err := pickContainer(state, "app")
	if err != nil {
		t.Fatal(err)
	}
	if got != "demo-app-r2" {
		t.Fatalf("picked %q, want the active release's container", got)
	}
}

// A container from the active release that is not running is no use either.
func TestResolveContainerFallsBackToAnyRunningContainer(t *testing.T) {
	state := &engine.RemoteState{
		Ledger: &release.Ledger{Current: "r2"},
		Containers: []engine.Container{
			{Name: "demo-app-r2", Service: "app", Release: "r2", State: "exited"},
			{Name: "demo-app-r1", Service: "app", Release: "r1", State: "running"},
		},
	}
	got, err := pickContainer(state, "app")
	if err != nil {
		t.Fatal(err)
	}
	if got != "demo-app-r1" {
		t.Fatalf("picked %q, want the one that is actually running", got)
	}
}

func TestResolveContainerReportsNothingRunning(t *testing.T) {
	state := &engine.RemoteState{
		Ledger:     &release.Ledger{Current: "r1"},
		Containers: []engine.Container{{Name: "demo-app", Service: "app", State: "exited"}},
	}
	if _, err := pickContainer(state, "app"); err == nil {
		t.Fatal("pickContainer accepted a service with no running container")
	} else if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error should say the containers are stopped, got: %v", err)
	}
}

func TestResolveContainerReportsAnUnknownService(t *testing.T) {
	state := &engine.RemoteState{Ledger: &release.Ledger{Current: "r1"}}
	if _, err := pickContainer(state, "nope"); err == nil {
		t.Fatal("pickContainer accepted a service with no containers at all")
	}
}
