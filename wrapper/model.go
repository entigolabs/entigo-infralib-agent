package wrapper

import "context"

// HandshakeData binds a wrapper stream to a single pipeline execution.
type HandshakeData struct {
	CampaignId string
	Step       string
	Command    string
}

// BackendClient forwards wrapper logs to the portal backend over gRPC.
type BackendClient interface {
	// Connect opens the stream, sends the handshake, and starts the supervisor
	// that owns reconnect logic. Must be called exactly once before SendLog.
	Connect(h HandshakeData) error
	// SendLog forwards a single raw stdout line. Safe to call concurrently
	// from multiple goroutines; non-blocking (drops on buffer overflow).
	SendLog(line string) error
	// Disconnect signals the supervisor to deliver ExecutionComplete on the
	// current stream and shut down. The ctx bounds how long to wait for the
	// supervisor to wind down before giving up. Releases the underlying
	// connection regardless of outcome.
	Disconnect(ctx context.Context, exitCode int, execErr error) error
}
