package server

// MessageSender abstracts sending permission prompts back to the message source.
// The agent's own replies are no longer a sender responsibility: the Stop hook
// publishes them as events directly, bypassing this interface entirely.
type MessageSender interface {
	SendPermissionPrompt(text string) error
}
