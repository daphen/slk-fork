// internal/ui/mode_workspace_finder.go
//
// Workspace-finder mode key handler (Phase 5e).
//
// Forwards normalised keys to the workspace finder. On a result
// (Enter on a selected workspace), closes the finder and -- if
// the selection is a different workspace -- dispatches the
// workspace-switcher callback. Esc-driven close from the finder
// itself drops back to Normal.
package ui

import (
	tea "charm.land/bubbletea/v2"
)

func handleWorkspaceFinderMode(a *App, msg tea.KeyMsg) tea.Cmd {
	result := a.workspaceFinder.HandleKey(normalizeFinderKey(msg))
	if result != nil {
		a.workspaceFinder.Close()
		a.SetMode(ModeNormal)
		if a.workspaceSwitcher != nil && result.ID != a.workspaceRail.SelectedID() {
			switcher := a.workspaceSwitcher
			teamID := result.ID
			return func() tea.Msg {
				return switcher(teamID)
			}
		}
	}
	if !a.workspaceFinder.IsVisible() {
		a.SetMode(ModeNormal)
	}
	return nil
}
