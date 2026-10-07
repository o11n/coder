package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	protobuf "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/testutil"
)

func TestConnectionReporter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		expectedIP string
		report     proto.ConnectEvent
	}{
		{
			// Test that reporting doesn't choke if given an empty IP string, which is
			// what we send if we cannot get the remote address.
			name: "EmptyIP",
			report: proto.ConnectEvent{
				ID:   uuid.New(),
				Type: proto.Connection_TYPE_UNSPECIFIED,
			},
		},
		{
			name:       "WithIP",
			expectedIP: "127.0.0.1",
			report: proto.ConnectEvent{
				ID:   uuid.New(),
				IP:   "127.0.0.1:8080",
				Type: proto.Connection_TYPE_UNSPECIFIED,
			},
		},
		{
			name:       "Localhost",
			expectedIP: "127.0.0.1",
			report: proto.ConnectEvent{
				ID:   uuid.New(),
				IP:   "localhost:8080",
				Type: proto.Connection_TYPE_UNSPECIFIED,
			},
		},
		{
			name: "WithClientSessionID",
			report: proto.ConnectEvent{
				ID:              uuid.New(),
				Type:            proto.Connection_TYPE_UNSPECIFIED,
				ClientSessionID: "0123456789abcdef0123456789abcdef",
			},
		},
		{
			name: "WithAppName",
			report: proto.ConnectEvent{
				ID:      uuid.New(),
				Method:  proto.Connection_METHOD_SSH,
				Type:    proto.Connection_VSCODE,
				AppName: "cursor",
			},
		},
		{
			name: "ReconnectingPTY",
			report: proto.ConnectEvent{
				ID:              uuid.New(),
				Method:          proto.Connection_METHOD_RECONNECTING_PTY,
				Type:            proto.Connection_RECONNECTING_PTY,
				ClientSessionID: "0123456789abcdef0123456789abcdef",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := testutil.Context(t, testutil.WaitShort)
			logger := testutil.Logger(t)
			reporter := newConnectionReporter(ctx, logger)

			sink := newFakeConnectionReportSink()
			loopCtx, loopCancel := context.WithCancel(ctx)
			defer loopCancel()
			go func() {
				err := reporter.reportLoop(loopCtx, sink)
				assert.Error(t, err) // Loop canceled.
			}()

			connReporter := reporter.Connect(tc.report)

			req0 := testutil.RequireReceive(ctx, t, sink.report)
			require.Equal(t, tc.report.Type, req0.GetConnection().GetType())
			require.Equal(t, tc.expectedIP, req0.GetConnection().Ip)
			require.Equal(t, tc.report.ID[:], req0.GetConnection().GetId())
			require.Equal(t, proto.Connection_CONNECT, req0.GetConnection().GetAction())
			require.Equal(t, tc.report.ClientSessionID, req0.GetConnection().GetClientSessionId())
			require.Equal(t, tc.report.AppName, req0.GetConnection().GetAppName())
			require.Equal(t, tc.report.Method, req0.GetConnection().GetConnectionMethod())

			connReporter.Disconnect(proto.DisconnectEvent{
				Code:   0,
				Reason: "because",
			})

			req1 := testutil.RequireReceive(ctx, t, sink.report)
			require.Equal(t, tc.report.Type, req1.GetConnection().GetType())
			require.Equal(t, tc.expectedIP, req1.GetConnection().Ip)
			require.Equal(t, tc.report.ID[:], req1.GetConnection().GetId())
			require.Equal(t, proto.Connection_DISCONNECT, req1.GetConnection().GetAction())
			require.Equal(t, "because", req1.GetConnection().GetReason())
			require.Equal(t, tc.report.ClientSessionID, req1.GetConnection().GetClientSessionId())
			require.Equal(t, tc.report.AppName, req1.GetConnection().GetAppName())
			require.Equal(t, tc.report.Method, req1.GetConnection().GetConnectionMethod())
		})
	}

	t.Run("DropsPastLimit", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		logger := testutil.Logger(t)
		reporter := newConnectionReporter(ctx, logger)

		// Given: reports past the buffer limit.
		limit := 2
		reporter.report = make(chan *proto.ReportConnectionRequest, limit)
		for i := range limit {
			connReporter := reporter.Connect(proto.ConnectEvent{
				ID:   uuid.New(),
				Type: proto.Connection_SSH,
			})
			connReporter.Disconnect(proto.DisconnectEvent{
				Code: i,
			})
		}

		// Then: should only get reports up to the limit.
		for i := range limit + 1 {
			var r *proto.ReportConnectionRequest
			select {
			case r = <-reporter.report:
			default:
			}

			if i < limit {
				require.NotNil(t, r, "should have received a report")
			} else {
				require.Nil(t, r, "report should have been dropped")
			}
		}
	})
}

// Agents and coderd of different versions exchange Connection by field number,
// so the numbers and enum values must not change.
func TestConnectionRequestWire(t *testing.T) {
	t.Parallel()

	require.EqualValues(t, 0, proto.Connection_METHOD_UNSPECIFIED)
	require.EqualValues(t, 1, proto.Connection_METHOD_SSH)
	require.EqualValues(t, 2, proto.Connection_METHOD_RECONNECTING_PTY)
	require.EqualValues(t, 3, proto.Connection_METHOD_PORT_FORWARDING)
	require.Len(t, proto.Connection_Method_name, 4)

	const sessionID = "0123456789abcdef0123456789abcdef"

	t.Run("NewAgentKeepsLegacyFields", func(t *testing.T) {
		t.Parallel()

		id := uuid.New()
		connect := &proto.ConnectEvent{
			ID:              id,
			Method:          proto.Connection_METHOD_SSH,
			Type:            proto.Connection_VSCODE,
			AppName:         "cursor",
			ClientSessionID: sessionID,
		}
		for _, disconnect := range []*proto.DisconnectEvent{nil, {Code: 1, Reason: "because"}} {
			b, err := protobuf.Marshal(connectionRequest(connect, disconnect).GetConnection())
			require.NoError(t, err)

			// Coderd without connection_method and app_name reads only the
			// fields it knows, so type and client_session_id must keep
			// their numbers.
			fields := wireFields(t, b)
			require.Equal(t, string(id[:]), fields[1])
			require.Equal(t, uint64(proto.Connection_VSCODE), fields[3])
			require.Equal(t, sessionID, fields[8])
			require.Equal(t, "cursor", fields[9])
			require.Equal(t, uint64(proto.Connection_METHOD_SSH), fields[10])
		}
	})

	t.Run("OldAgentDecodesUnspecified", func(t *testing.T) {
		t.Parallel()

		// An agent without connection_method and app_name sends only type.
		id := uuid.New()
		var b []byte
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, id[:])
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(proto.Connection_CONNECT))
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(proto.Connection_JETBRAINS))
		b = protowire.AppendTag(b, 8, protowire.BytesType)
		b = protowire.AppendString(b, sessionID)

		var conn proto.Connection
		require.NoError(t, protobuf.Unmarshal(b, &conn))
		require.Equal(t, proto.Connection_METHOD_UNSPECIFIED, conn.GetConnectionMethod())
		require.Empty(t, conn.GetAppName())
		require.Equal(t, proto.Connection_JETBRAINS, conn.GetType())
		require.Equal(t, sessionID, conn.GetClientSessionId())
	})

	t.Run("UnsupportedMethodSurvives", func(t *testing.T) {
		t.Parallel()

		// Enums are open, so coderd sees an unsupported method and can
		// reject it instead of reading it as unspecified.
		var b []byte
		b = protowire.AppendTag(b, 10, protowire.VarintType)
		b = protowire.AppendVarint(b, 42)

		var conn proto.Connection
		require.NoError(t, protobuf.Unmarshal(b, &conn))
		require.Equal(t, proto.Connection_Method(42), conn.GetConnectionMethod())
	})
}

// wireFields decodes the varint and length-delimited fields of a message.
func wireFields(t *testing.T, b []byte) map[protowire.Number]any {
	t.Helper()

	fields := make(map[protowire.Number]any)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		require.GreaterOrEqual(t, n, 0, "consume tag")
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			require.GreaterOrEqual(t, n, 0, "consume varint")
			fields[num] = v
			b = b[n:]
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			require.GreaterOrEqual(t, n, 0, "consume bytes")
			fields[num] = string(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			require.GreaterOrEqual(t, n, 0, "consume field")
			b = b[n:]
		}
	}
	return fields
}

type fakeConnectionReportSink struct {
	report chan *proto.ReportConnectionRequest
}

func newFakeConnectionReportSink() *fakeConnectionReportSink {
	return &fakeConnectionReportSink{
		report: make(chan *proto.ReportConnectionRequest),
	}
}

func (r *fakeConnectionReportSink) ReportConnection(_ context.Context, in *proto.ReportConnectionRequest) (*emptypb.Empty, error) {
	r.report <- in
	return &emptypb.Empty{}, nil
}
