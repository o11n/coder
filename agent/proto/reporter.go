package proto

import (
	"github.com/google/uuid"
)

type ConnectionReporter interface {
	Connect(ConnectEvent) DisconnectionReporter
}

type DisconnectionReporter interface {
	Disconnect(DisconnectEvent)
}

type NoopConnectionReporter struct{}

func (*NoopConnectionReporter) Connect(ConnectEvent) DisconnectionReporter {
	return &NoopDisconnectionReporter{}
}

type NoopDisconnectionReporter struct{}

func (*NoopDisconnectionReporter) Disconnect(DisconnectEvent) {}

// ConnectEvent describes a connection. The disconnect report for the same
// connection repeats every field.
type ConnectEvent struct {
	ID uuid.UUID
	// Method is how the client reached the agent. Handlers set it themselves
	// so client-supplied app names cannot change it.
	Method Connection_Method
	// Type is the deprecated connection type for coderd that predates Method
	// and AppName.
	Type Connection_Type
	// AppName is the client-supplied app, or empty when unknown.
	AppName         string
	IP              string
	ClientSessionID string
}

type DisconnectEvent struct {
	Code   int
	Reason string
}
