package ferry

import (
	"context"
	"errors"
	"net/http"

	"github.com/StarMoe-org/ferry/internal/state"
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

type buildManager struct{}

func (b *buildManager) Close() {}

func (e *Engine) initBuilder() {}

func (e *Engine) HandleGitHubWebhook(_ context.Context, _, _ string, _ []byte) (int, string) {
	return http.StatusUnauthorized, "bad signature"
}

func (e *Engine) TriggerBuild(_ context.Context, _, _ string) (state.Event, error) {
	return state.Event{}, ErrNotConfigured
}

func (e *Engine) GetWebhookStatus(_ context.Context, _ string) (WebhookStatus, error) {
	return WebhookStatus{Status: "unconfigured"}, nil
}

func (e *Engine) SetupWebhook(_ context.Context, _, _ string) (WebhookStatus, error) {
	return WebhookStatus{}, ErrNotConfigured
}

type snapshotRunner struct{}

func (s *snapshotRunner) Close() {}

func (e *Engine) initSnapshots() {}

func (e *Engine) Snapshot(_ context.Context, _ string) (SnapshotResult, error) {
	return SnapshotResult{}, ErrNotConfigured
}
