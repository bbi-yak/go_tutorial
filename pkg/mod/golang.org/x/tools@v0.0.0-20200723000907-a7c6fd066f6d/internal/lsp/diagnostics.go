// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/lsp/debug/tag"
	"golang.org/x/tools/internal/lsp/mod"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
	"golang.org/x/tools/internal/xcontext"
	"golang.org/x/xerrors"
)

type diagnosticKey struct {
	id           source.FileIdentity
	withAnalysis bool
}

func (s *Server) diagnoseDetached(snapshot source.Snapshot) {
	ctx := snapshot.View().BackgroundContext()
	ctx = xcontext.Detach(ctx)
	reports, shows := s.diagnose(ctx, snapshot, false)
	if shows != nil {
		// If a view has been created or the configuration changed, warn the user.
		s.client.ShowMessage(ctx, shows)
	}
	s.publishReports(ctx, snapshot, reports)
}

func (s *Server) diagnoseSnapshot(snapshot source.Snapshot) {
	ctx := snapshot.View().BackgroundContext()

	// Ignore possible workspace configuration warnings in the normal flow.
	reports, _ := s.diagnose(ctx, snapshot, false)
	s.publishReports(ctx, snapshot, reports)
}

// diagnose is a helper function for running diagnostics with a given context.
// Do not call it directly.
func (s *Server) diagnose(ctx context.Context, snapshot source.Snapshot, alwaysAnalyze bool) (map[diagnosticKey][]*source.Diagnostic, *protocol.ShowMessageParams) {
	ctx, done := event.Start(ctx, "lsp:background-worker")
	defer done()

	// Wait for a free diagnostics slot.
	select {
	case <-ctx.Done():
		return nil, nil
	case s.diagnosticsSema <- struct{}{}:
	}
	defer func() { <-s.diagnosticsSema }()

	allReports := make(map[diagnosticKey][]*source.Diagnostic)
	var reportsMu sync.Mutex
	var wg sync.WaitGroup

	// Diagnose the go.mod file.
	reports, modErr := mod.Diagnostics(ctx, snapshot)
	if ctx.Err() != nil {
		return nil, nil
	}
	if modErr != nil {
		event.Error(ctx, "warning: diagnose go.mod", modErr, tag.Directory.Of(snapshot.View().Folder().Filename()))
	}
	for id, diags := range reports {
		if id.URI == "" {
			event.Error(ctx, "missing URI for module diagnostics", fmt.Errorf("empty URI"), tag.Directory.Of(snapshot.View().Folder().Filename()))
			continue
		}
		key := diagnosticKey{
			id:           id,
			withAnalysis: true, // treat go.mod diagnostics like analyses
		}
		allReports[key] = diags
	}

	// Diagnose all of the packages in the workspace.
	wsPackages, err := snapshot.WorkspacePackages(ctx)
	if err != nil {
		// Try constructing a more helpful error message out of this error.
		if s.handleFatalErrors(ctx, snapshot, modErr, err) {
			return nil, nil
		}
		msg := `The code in the workspace failed to compile (see the error message below).
If you believe this is a mistake, please file an issue: https://github.com/golang/go/issues/new.`
		event.Error(ctx, msg, err, tag.Snapshot.Of(snapshot.ID()), tag.Directory.Of(snapshot.View().Folder()))
		if err := s.client.ShowMessage(ctx, &protocol.ShowMessageParams{
			Type:    protocol.Error,
			Message: fmt.Sprintf("%s\n%v", msg, err),
		}); err != nil {
			event.Error(ctx, "ShowMessage failed", err, tag.Directory.Of(snapshot.View().Folder()))
		}
		return nil, nil
	}
	var shows *protocol.ShowMessageParams
	for _, ph := range wsPackages {
		wg.Add(1)
		go func(ph source.PackageHandle) {
			defer wg.Done()
			// Only run analyses for packages with open files.
			withAnalyses := alwaysAnalyze
			for _, pgh := range ph.CompiledGoFiles() {
				if snapshot.IsOpen(pgh.File().URI()) {
					withAnalyses = true
				}
			}
			reports, warn, err := source.Diagnostics(ctx, snapshot, ph, withAnalyses)
			// Check if might want to warn the user about their build configuration.
			// Our caller decides whether to send the message.
			if warn && !snapshot.View().ValidBuildConfiguration() {
				shows = &protocol.ShowMessageParams{
					Type:    protocol.Warning,
					Message: `You are neither in a module nor in your GOPATH. If you are using modules, please open your editor at the directory containing the go.mod. If you believe this warning is incorrect, please file an issue: https://github.com/golang/go/issues/new.`,
				}
			}
			if err != nil {
				event.Error(ctx, "warning: diagnose package", err, tag.Snapshot.Of(snapshot.ID()), tag.Package.Of(ph.ID()))
				return
			}
			reportsMu.Lock()
			for id, diags := range reports {
				key := diagnosticKey{
					id:           id,
					withAnalysis: withAnalyses,
				}
				allReports[key] = append(allReports[key], diags...)
			}
			reportsMu.Unlock()
		}(ph)
	}
	wg.Wait()
	return allReports, shows
}

func (s *Server) publishReports(ctx context.Context, snapshot source.Snapshot, reports map[diagnosticKey][]*source.Diagnostic) {
	// Check for context cancellation before publishing diagnostics.
	if ctx.Err() != nil {
		return
	}

	s.deliveredMu.Lock()
	defer s.deliveredMu.Unlock()

	for key, diagnostics := range reports {
		// Don't deliver diagnostics if the context has already been canceled.
		if ctx.Err() != nil {
			break
		}
		// Pre-sort diagnostics to avoid extra work when we compare them.
		source.SortDiagnostics(diagnostics)
		toSend := sentDiagnostics{
			version:      key.id.Version,
			identifier:   key.id.Identifier,
			sorted:       diagnostics,
			withAnalysis: key.withAnalysis,
			snapshotID:   snapshot.ID(),
		}

		// We use the zero values if this is an unknown file.
		delivered := s.delivered[key.id.URI]

		// Snapshot IDs are always increasing, so we use them instead of file
		// versions to create the correct order for diagnostics.

		// If we've already delivered diagnostics for a future snapshot for this file,
		// do not deliver them.
		if delivered.snapshotID > toSend.snapshotID {
			// Do not update the delivered map since it already contains newer diagnostics.
			continue
		}

		// Check if we should reuse the cached diagnostics.
		if equalDiagnostics(delivered.sorted, diagnostics) {
			// Make sure to update the delivered map.
			s.delivered[key.id.URI] = toSend
			continue
		}

		// If we've already delivered diagnostics for this file, at this
		// snapshot, with analyses, do not send diagnostics without analyses.
		if delivered.snapshotID == toSend.snapshotID && delivered.version == toSend.version &&
			delivered.withAnalysis && !toSend.withAnalysis {
			// Do not update the delivered map since it already contains better diagnostics.
			continue
		}
		if err := s.client.PublishDiagnostics(ctx, &protocol.PublishDiagnosticsParams{
			Diagnostics: toProtocolDiagnostics(diagnostics),
			URI:         protocol.URIFromSpanURI(key.id.URI),
			Version:     key.id.Version,
		}); err != nil {
			event.Error(ctx, "publishReports: failed to deliver diagnostic", err, tag.URI.Of(key.id.URI))
			continue
		}
		// Update the delivered map.
		s.delivered[key.id.URI] = toSend
	}
}

// equalDiagnostics returns true if the 2 lists of diagnostics are equal.
// It assumes that both a and b are already sorted.
func equalDiagnostics(a, b []*source.Diagnostic) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if source.CompareDiagnostic(a[i], b[i]) != 0 {
			return false
		}
	}
	return true
}

func toProtocolDiagnostics(diagnostics []*source.Diagnostic) []protocol.Diagnostic {
	reports := []protocol.Diagnostic{}
	for _, diag := range diagnostics {
		related := make([]protocol.DiagnosticRelatedInformation, 0, len(diag.Related))
		for _, rel := range diag.Related {
			related = append(related, protocol.DiagnosticRelatedInformation{
				Location: protocol.Location{
					URI:   protocol.URIFromSpanURI(rel.URI),
					Range: rel.Range,
				},
				Message: rel.Message,
			})
		}
		reports = append(reports, protocol.Diagnostic{
			Message:            strings.TrimSpace(diag.Message), // go list returns errors prefixed by newline
			Range:              diag.Range,
			Severity:           diag.Severity,
			Source:             diag.Source,
			Tags:               diag.Tags,
			RelatedInformation: related,
		})
	}
	return reports
}

func (s *Server) handleFatalErrors(ctx context.Context, snapshot source.Snapshot, modErr, loadErr error) bool {
	modURI := snapshot.View().ModFile()

	// We currently only have workarounds for errors associated with modules.
	if modURI == "" {
		return false
	}

	switch loadErr {
	case source.InconsistentVendoring:
		item, err := s.client.ShowMessageRequest(ctx, &protocol.ShowMessageRequestParams{
			Type: protocol.Error,
			Message: `Inconsistent vendoring detected. Please re-run "go mod vendor".
See https://github.com/golang/go/issues/39164 for more detail on this issue.`,
			Actions: []protocol.MessageActionItem{
				{Title: "go mod vendor"},
			},
		})
		// If the user closes the pop-up, don't show them further errors.
		if item == nil {
			return true
		}
		if err != nil {
			event.Error(ctx, "go mod vendor ShowMessageRequest failed", err, tag.Directory.Of(snapshot.View().Folder()))
			return true
		}
		if err := s.directGoModCommand(ctx, protocol.URIFromSpanURI(modURI), "mod", []string{"vendor"}...); err != nil {
			if err := s.client.ShowMessage(ctx, &protocol.ShowMessageParams{
				Type:    protocol.Error,
				Message: fmt.Sprintf(`"go mod vendor" failed with %v`, err),
			}); err != nil {
				if err != nil {
					event.Error(ctx, "go mod vendor ShowMessage failed", err, tag.Directory.Of(snapshot.View().Folder()))
				}
			}
		}
		return true
	}
	// If there is a go.mod-related error, as well as a workspace load error,
	// there is likely an issue with the go.mod file. Try to parse the error
	// message and create a diagnostic.
	if modErr == nil {
		return false
	}
	if xerrors.Is(loadErr, source.PackagesLoadError) {
		fh, err := snapshot.GetFile(ctx, modURI)
		if err != nil {
			return false
		}
		diag, err := mod.ExtractGoCommandError(ctx, snapshot, fh, loadErr)
		if err != nil {
			return false
		}
		s.publishReports(ctx, snapshot, map[diagnosticKey][]*source.Diagnostic{
			{id: fh.Identity()}: {diag},
		})
		return true
	}
	return false
}