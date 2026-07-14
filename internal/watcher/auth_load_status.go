package watcher

import "time"

type AuthLoadState string

const (
	AuthLoadStateIdle     AuthLoadState = "idle"
	AuthLoadStateLoading  AuthLoadState = "loading"
	AuthLoadStateReady    AuthLoadState = "ready"
	AuthLoadStateDegraded AuthLoadState = "degraded"
)

type AuthLoadStatus struct {
	State           AuthLoadState `json:"state"`
	FilesDiscovered int64         `json:"files_discovered"`
	FilesProcessed  int64         `json:"files_processed"`
	AuthsLoaded     int64         `json:"auths_loaded"`
	FilesFailed     int64         `json:"files_failed"`
	FilesSkipped    int64         `json:"files_skipped"`
	ScanComplete    bool          `json:"scan_complete"`
	StartedAt       time.Time     `json:"started_at"`
	CompletedAt     *time.Time    `json:"completed_at"`
}

func idleAuthLoadStatus() AuthLoadStatus {
	return AuthLoadStatus{State: AuthLoadStateIdle}
}

func (w *Watcher) publishAuthLoadStatus(status AuthLoadStatus) {
	if w == nil {
		return
	}
	if status.CompletedAt != nil {
		completed := *status.CompletedAt
		status.CompletedAt = &completed
	}
	w.authLoadStatus.Store(status)
}

func (w *Watcher) AuthLoadStatus() AuthLoadStatus {
	if w == nil {
		return idleAuthLoadStatus()
	}
	value := w.authLoadStatus.Load()
	if value == nil {
		return idleAuthLoadStatus()
	}
	return value.(AuthLoadStatus)
}
