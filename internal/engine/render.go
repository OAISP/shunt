package engine

import (
	"fmt"
	"io"
	"strings"

	"github.com/OAISP/shunt/internal/ui"
)

// nameCol is the width service, image and accessory names are padded to, so the
// three sections of a plan line up as one table.
const nameCol = 14

// Render prints the plan in the terraform-ish shape operators already read
// fluently: a symbol, the subject, and the reason it is changing.
func (p *Plan) Render(w io.Writer, s ui.Style) {
	p.renderHeader(w, s)
	p.renderImages(w, s)
	p.renderTransfer(w, s)
	p.renderAccessories(w, s)
	p.renderArtifacts(w, s)
	p.renderStages(w, s)
	p.renderServices(w, s)
	p.renderSecrets(w, s)
	fmt.Fprintln(w)
}

func (p *Plan) renderHeader(w io.Writer, s ui.Style) {
	fmt.Fprintf(w, "\n%s %s\n", s.Bold("release"), p.ReleaseID)
	if p.Current == nil {
		fmt.Fprintf(w, "%s none — this is the first deploy to this host\n", s.Dim("current"))
		return
	}
	fmt.Fprintf(w, "%s %s (%s)\n", s.Dim("current"), p.Current.ID, p.Current.Status)
}

func (p *Plan) renderImages(w io.Writer, s ui.Style) {
	fmt.Fprintf(w, "\n%s\n", s.Bold("images"))
	for _, i := range p.Images {
		switch i.Action {
		case "unchanged":
			fmt.Fprintf(w, "    %s\n", s.Dim(fmt.Sprintf("%-*s unchanged  %s", nameCol, i.Name, ui.ShortDigest(i.NewDgst))))
		case "pull":
			fmt.Fprintf(w, "    %s\n", s.Dim(fmt.Sprintf("%-*s pull       (external)", nameCol, i.Name)))
		case "create":
			fmt.Fprintf(w, "  %s %-*s create     %s\n", s.Add(), nameCol, i.Name, ui.ShortDigest(i.NewDgst))
		case "update":
			fmt.Fprintf(w, "  %s %-*s %s → %s\n", s.Change(), nameCol, i.Name,
				ui.ShortDigest(i.OldDgst), ui.ShortDigest(i.NewDgst))
		}
	}
}

func (p *Plan) renderTransfer(w io.Writer, s ui.Style) {
	fmt.Fprintf(w, "\n%s\n", s.Bold("transfer"))
	if p.Transfer.Total == 0 {
		fmt.Fprintf(w, "    %s\n", s.Dim("(not estimated)"))
		return
	}
	fmt.Fprintf(w, "    %s of %s to send  %s\n",
		ui.Bytes(p.Transfer.Missing), ui.Bytes(p.Transfer.Total),
		s.Dim(fmt.Sprintf("(%d blob(s); %.1f%% already on host)", p.Transfer.Blobs, p.Transfer.CachedPercent())))
}

func (p *Plan) renderAccessories(w io.Writer, s ui.Style) {
	if len(p.Accessories) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s %s\n", s.Bold("accessories"), s.Dim("(booted once; never replaced by a deploy)"))
	for _, a := range p.Accessories {
		switch a.Action {
		case "unchanged":
			fmt.Fprintf(w, "    %s\n", s.Dim(fmt.Sprintf("%-*s running", nameCol, a.Name)))
		case "create":
			fmt.Fprintf(w, "  %s %-*s boot\n", s.Add(), nameCol, a.Name)
		case "drift":
			fmt.Fprintf(w, "  %s %-*s %s\n", s.Warn(), nameCol, a.Name, s.Amber("drifted from shunt.toml"))
			renderReasons(w, s, a.Reasons)
		}
	}
}

func (p *Plan) renderArtifacts(w io.Writer, s ui.Style) {
	if len(p.Artifacts) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s %s\n", s.Bold("artifacts"), s.Dim("(swapped in atomically, after stages)"))
	for _, a := range p.Artifacts {
		host := "not on host"
		if a.HostBytes >= 0 {
			host = ui.Bytes(a.HostBytes) + " on host"
		}
		detail := fmt.Sprintf("%s local · %s", ui.Bytes(a.LocalBytes), host)
		switch {
		case a.HostBytes < 0:
			fmt.Fprintf(w, "  %s %-*s %s\n", s.Add(), nameCol, a.Name, s.Dim(detail))
		case a.Differs():
			fmt.Fprintf(w, "  %s %-*s %s\n", s.Change(), nameCol, a.Name, s.Dim(detail))
		default:
			fmt.Fprintf(w, "    %s\n", s.Dim(fmt.Sprintf("%-*s unchanged · %s", nameCol, a.Name, ui.Bytes(a.LocalBytes))))
			continue
		}
		fmt.Fprintf(w, "      %s\n", s.Dim("→ "+a.Dest))
	}
}

func (p *Plan) renderStages(w io.Writer, s ui.Style) {
	if len(p.Stages) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s %s\n", s.Bold("stages"), s.Dim("(run before any service is replaced)"))

	// The pipeline reads as a sequence, so keep it on one line and mark the
	// entries the manifest changed rather than breaking it into a list.
	names := make([]string, 0, len(p.Stages))
	var notes []string
	for _, st := range p.Stages {
		switch st.Action {
		case "remove":
			notes = append(notes, fmt.Sprintf("%s %s no longer in shunt.toml — it will not run again", s.Remove(), st.Name))
			continue
		case "create":
			notes = append(notes, fmt.Sprintf("%s %s added", s.Add(), st.Name))
		case "update":
			notes = append(notes, fmt.Sprintf("%s %s changed", s.Change(), st.Name))
		}
		names = append(names, st.Name)
	}
	if len(names) > 0 {
		fmt.Fprintf(w, "    %s\n", strings.Join(names, " → "))
	}
	for _, n := range notes {
		fmt.Fprintf(w, "  %s\n", n)
	}
}

func (p *Plan) renderServices(w io.Writer, s ui.Style) {
	fmt.Fprintf(w, "\n%s\n", s.Bold("services"))
	for _, svc := range p.Services {
		switch svc.Action {
		case "unchanged":
			fmt.Fprintf(w, "    %s\n", s.Dim(fmt.Sprintf("%-*s unchanged", nameCol, svc.Name)))
		case "create":
			fmt.Fprintf(w, "  %s %-*s create%s\n", s.Add(), nameCol, svc.Name, downtimeNote(svc, s))
		case "update":
			fmt.Fprintf(w, "  %s %-*s %s%s\n", s.Change(), nameCol, svc.Name, swapVerb(svc), downtimeNote(svc, s))
			renderReasons(w, s, svc.Reasons)
		case "orphaned":
			fmt.Fprintf(w, "  %s %-*s %s\n", s.Warn(), nameCol, svc.Name, s.Amber("orphaned"))
			renderReasons(w, s, svc.Reasons)
		}
	}
}

func (p *Plan) renderSecrets(w io.Writer, s ui.Style) {
	sc := p.Secrets
	fmt.Fprintf(w, "\n%s %d key(s)", s.Bold("secrets"), sc.Total)
	if len(sc.Added)+len(sc.Removed)+len(sc.Changed) == 0 {
		fmt.Fprintf(w, " %s\n", s.Dim("unchanged"))
		return
	}
	fmt.Fprintln(w)
	for _, k := range sc.Added {
		fmt.Fprintf(w, "  %s %s\n", s.Add(), k)
	}
	for _, k := range sc.Changed {
		fmt.Fprintf(w, "  %s %s %s\n", s.Change(), k, s.Dim("(value changed)"))
	}
	for _, k := range sc.Removed {
		fmt.Fprintf(w, "  %s %s\n", s.Remove(), k)
	}
}

func renderReasons(w io.Writer, s ui.Style, reasons []string) {
	for _, r := range reasons {
		fmt.Fprintf(w, "      %s\n", s.Dim(r))
	}
}

// swapVerb names how a service is replaced, since the two paths have very
// different availability characteristics.
func swapVerb(s ServiceChange) string {
	if s.ZeroDowntime {
		return "blue/green"
	}
	return "recreate"
}

func downtimeNote(sc ServiceChange, s ui.Style) string {
	if !sc.ZeroDowntime {
		return "  " + s.Dim("(brief gap while it restarts)")
	}
	if !sc.ProxyGated {
		// Overlapping without a proxy-pollable health check means the proxy will
		// route to the new container as soon as it exists. A backend that is not
		// listening yet is covered by retry; one that is listening and still
		// warming up is not, and the operator should know which they have.
		return "  " + s.Amber("(starts alongside; proxy cannot poll this health check)")
	}
	return "  " + s.Dim("(starts alongside; no downtime)")
}
