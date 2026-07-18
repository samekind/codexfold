package fsctl

import (
	"context"
	"fmt"

	"github.com/jstar0/codexfold/internal/storage"
)

const (
	ComponentDaemon   = "daemon"
	ComponentMount    = "mount"
	ComponentPack     = "pack"
	ComponentManifest = "manifest"
	ComponentDelta    = "delta"
	ComponentBacking  = "backing"
	ComponentRoute    = "route"
	ComponentFallback = "fallback"
	ComponentJournal  = "journal"
	ComponentClient   = "client"
	ComponentStorage  = "storage"
)

var RequiredComponents = []string{ComponentDaemon, ComponentMount, ComponentPack, ComponentManifest, ComponentDelta, ComponentBacking, ComponentRoute, ComponentFallback, ComponentJournal, ComponentClient, ComponentStorage}

type Check struct {
	Component string
	Run       func(context.Context) error
}

type Issue struct {
	Component   string `json:"component"`
	Severity    string `json:"severity"`
	SessionID   string `json:"session_id,omitempty"`
	Generation  uint64 `json:"generation,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type DoctorReport struct {
	Healthy         bool              `json:"healthy"`
	IssueCount      int               `json:"issue_count"`
	Issues          []Issue           `json:"issues,omitempty"`
	ComponentHealth map[string]bool   `json:"component_health"`
	Storage         storage.Inventory `json:"storage"`
	StorageLimits   storage.Limits    `json:"storage_limits"`
	AvailableBytes  int64             `json:"available_bytes"`
}

func Doctor(ctx context.Context, checks []Check) DoctorReport {
	report := DoctorReport{Healthy: true, ComponentHealth: make(map[string]bool)}
	seen := make(map[string]bool)
	for _, check := range checks {
		if check.Component == "" || check.Run == nil {
			report.Issues = append(report.Issues, Issue{Component: check.Component, Severity: "error", Message: "invalid doctor check"})
			continue
		}
		if seen[check.Component] {
			report.Issues = append(report.Issues, Issue{Component: check.Component, Severity: "error", Message: "duplicate doctor check"})
			continue
		}
		seen[check.Component] = true
		if err := ctx.Err(); err != nil {
			report.ComponentHealth[check.Component] = false
			report.Issues = append(report.Issues, Issue{Component: check.Component, Severity: "error", Message: err.Error()})
			continue
		}
		if err := check.Run(ctx); err != nil {
			report.ComponentHealth[check.Component] = false
			report.Issues = append(report.Issues, Issue{Component: check.Component, Severity: "error", Message: err.Error()})
		} else {
			report.ComponentHealth[check.Component] = true
		}
	}
	for _, component := range RequiredComponents {
		if !seen[component] {
			report.ComponentHealth[component] = false
			report.Issues = append(report.Issues, Issue{Component: component, Severity: "error", Message: fmt.Sprintf("required %s check is missing", component), Remediation: "register and run the required component check"})
		}
	}
	report.IssueCount = len(report.Issues)
	report.Healthy = report.IssueCount == 0
	return report
}
