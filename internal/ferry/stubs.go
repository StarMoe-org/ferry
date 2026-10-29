package ferry

import (
	"context"
	"errors"
)

var ErrNotConfigured = errors.New("feature is not configured")

// WebhookStatus describes the state of a GitHub push webhook on a source repository.
type WebhookStatus struct {
	Status     string `json:"status"` // "ok" | "missing" | "no-permission" | "unconfigured"
	WebhookURL string `json:"webhookUrl,omitempty"`
	HookID     int64  `json:"hookId,omitempty"`
	Message    string `json:"message,omitempty"`
}

// SnapshotResult is returned by POST /snapshot and POST /api/snapshot.
type SnapshotResult struct {
	Status string `json:"status"` // "committed" | "unchanged"
	Commit string `json:"commit"`
}

type snapshotRunner struct{}

func (s *snapshotRunner) Close() {}

func (e *Engine) initSnapshots() {}

func (e *Engine) Snapshot(_ context.Context, _ string) (SnapshotResult, error) {
	return SnapshotResult{}, ErrNotConfigured
}
