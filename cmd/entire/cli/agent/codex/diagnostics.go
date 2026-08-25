package codex

import "context"

// HookDiagnostics keeps current-checkout ownership separate from the
// read-only file Codex discovers.
type HookDiagnostics struct {
	Discovery       HookDiscovery
	WorktreeHooks   WorktreeHooksPath
	Worktree        HookConfigInspection
	Discovered      HookConfigInspection
	Trust           HookTrustInspection
	WorktreePathErr error
}

// InspectHookDiagnostics collects the local and effective Codex hook state
// without creating, rewriting, or removing either file.
func InspectHookDiagnostics(ctx context.Context) HookDiagnostics {
	diagnostics := HookDiagnostics{Discovery: ResolveHookDiscovery(ctx)}
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	return finishHookDiagnostics(ctx, diagnostics, worktreeHooks, err)
}

func inspectHookDiagnosticsAt(ctx context.Context, worktreeRoot string) HookDiagnostics {
	diagnostics := HookDiagnostics{Discovery: resolveHookDiscovery(worktreeRoot)}
	worktreeHooks, err := resolveWorktreeHooksPath(worktreeRoot)
	return finishHookDiagnostics(ctx, diagnostics, worktreeHooks, err)
}

func finishHookDiagnostics(
	ctx context.Context,
	diagnostics HookDiagnostics,
	worktreeHooks WorktreeHooksPath,
	err error,
) HookDiagnostics {
	if err != nil {
		diagnostics.WorktreePathErr = err
		diagnostics.Worktree = HookConfigInspection{State: HookFileInvalid, Err: err}
	} else {
		diagnostics.WorktreeHooks = worktreeHooks
		diagnostics.Worktree = inspectWorktreeHookConfig(ctx, worktreeHooks)
	}

	if diagnostics.Discovery.State != HookDiscoveryResolved {
		diagnostics.Discovered = HookConfigInspection{
			State: HookFileInvalid,
			Err:   diagnostics.Discovery.Diagnostic,
		}
		return diagnostics
	}

	diagnostics.Discovered = inspectDiscoveredHookConfig(ctx, diagnostics.Discovery.DiscoveredHooks)
	if diagnostics.Discovery.ProjectLayerExists() && diagnostics.Discovered.State == HookFileEntire {
		diagnostics.Trust = inspectHookTrustForDeclared(
			diagnostics.Discovery.DiscoveredHooks.Path(),
			diagnostics.Discovered.Declared,
		)
	}
	return diagnostics
}

// PathsDiffer reports whether current-checkout mutation and Codex discovery
// refer to different hook files.
func (d HookDiagnostics) PathsDiffer() bool {
	return d.WorktreeHooks.Path() != "" &&
		d.Discovery.DiscoveredHooks.Path() != "" &&
		d.WorktreeHooks.Path() != d.Discovery.DiscoveredHooks.Path()
}
