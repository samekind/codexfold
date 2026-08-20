package cli

import "context"

type FSKitResidencyServiceOutcome struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type FSKitResidencyOutcome struct {
	SchemaVersion    int                          `json:"schema_version"`
	IncidentMonitor  FSKitResidencyServiceOutcome `json:"incident_monitor"`
	LaunchAtLogin    FSKitResidencyServiceOutcome `json:"launch_at_login"`
	MenuBar          FSKitResidencyServiceOutcome `json:"menu_bar"`
	Ready            bool                         `json:"ready"`
	RequiresApproval bool                         `json:"requires_approval"`
}

func completeFSKitResidency(
	outcome *FSKitResidencyOutcome,
	menuBar FSKitResidencyServiceOutcome,
) {
	if outcome == nil {
		return
	}
	outcome.MenuBar = menuBar
	outcome.Ready = outcome.IncidentMonitor.State == "enabled" &&
		outcome.LaunchAtLogin.State == "enabled" &&
		menuBar.State == "enabled"
}

type fsKitAppTransaction interface {
	AppGroupPath() string
	Changed() bool
	Residency() FSKitResidencyOutcome
	Rollback(context.Context) error
	Commit() error
}
