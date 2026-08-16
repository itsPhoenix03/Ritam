package domain

import "time"

// ArtifactType classifies a produced artifact.
type ArtifactType string

const (
	ArtifactFile          ArtifactType = "FILE"
	ArtifactPatch         ArtifactType = "PATCH"
	ArtifactTestResult    ArtifactType = "TEST_RESULT"
	ArtifactReport        ArtifactType = "REPORT"
	ArtifactDocumentation ArtifactType = "DOCUMENTATION"
	ArtifactLog           ArtifactType = "LOG"
	ArtifactScreenshot    ArtifactType = "SCREENSHOT"
)

// Artifact points at something an agent produced. For now Location is a local
// filesystem path; the field is deliberately a string so it can later hold a
// different locator scheme without changing callers.
type Artifact struct {
	ID          ArtifactID
	TaskID      TaskID
	AgentID     AgentID
	Type        ArtifactType
	Location    string
	Description string
	Timestamp   time.Time
}

// Clone returns a copy of the artifact.
func (a *Artifact) Clone() *Artifact {
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}
