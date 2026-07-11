package fsctl

import (
	"fmt"
	"strings"
)

type Capability string

const (
	StorageEngine      Capability = "storage-engine"
	FSEnginePreview    Capability = "fs-engine-preview"
	PlatformCanary     Capability = "platform-canary"
	CrossPlatformReady Capability = "cross-platform-ready"
)

type Status struct {
	Capability Capability `json:"capability"`
	Platform   string     `json:"platform"`
}

func NewStatus(capability Capability, platform string) (Status, error) {
	valid := capability == StorageEngine || capability == FSEnginePreview || capability == PlatformCanary || capability == CrossPlatformReady
	if strings.HasPrefix(string(capability), "production-ready:") && strings.TrimPrefix(string(capability), "production-ready:") != "" {
		valid = true
	}
	if !valid {
		return Status{}, fmt.Errorf("non-canonical filesystem capability %q", capability)
	}
	if platform == "" {
		return Status{}, fmt.Errorf("status platform is required")
	}
	return Status{Capability: capability, Platform: platform}, nil
}
