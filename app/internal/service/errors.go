package service

// Error codes for machine-readable error identification.
const (
	ErrInvalidURL         = "invalid_url"
	ErrRepoNotAllowed     = "repo_not_allowed"
	ErrSessionNotFound    = "session_not_found"
	ErrInvalidTag         = "invalid_tag"
	ErrInvalidInput       = "invalid_input"
	ErrExecutionFailed    = "execution_failed"
	ErrNotAttachable      = "not_attachable"
	ErrNotProduced        = "not_produced"
	ErrNotCapturable      = "not_capturable"
	ErrHasChildren        = "has_children"
	ErrRelationNotAllowed = "relation_not_allowed"
	ErrChildCapExceeded   = "child_cap_exceeded"
	ErrChildUpInProgress  = "child_up_in_progress"
)

// Error is a structured error with a machine-readable code.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// LifecycleConfigurationWarning carries a lifecycle-configuration change
	// notice (see lifecycleConfigurationNotice) that was computed before
	// this error occurred, so a caller that only sees the error still
	// learns of it -- the same notice a successful Up/Down/Destroy result
	// carries on its own field of the same name.
	LifecycleConfigurationWarning string `json:"lifecycle_configuration_warning,omitempty"`
}

func (e *Error) Error() string {
	return e.Message
}
