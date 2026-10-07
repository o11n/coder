package coderd_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	protobuf "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"storj.io/drpc"
	"storj.io/drpc/drpcerr"

	"github.com/coder/coder/v2/agent/agentssh"
	"github.com/coder/coder/v2/agent/agenttest"
	agentproto "github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/connectionlog"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbfake"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/coder/v2/provisionersdk/proto"
	"github.com/coder/coder/v2/tailnet"
	tailnetproto "github.com/coder/coder/v2/tailnet/proto"
	"github.com/coder/coder/v2/testutil"
)

// Ported to RPC API from coderd/workspaceagents_test.go
func TestWorkspaceAgentReportStats(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		apiKeyScope rbac.ScopeName
	}{
		{
			name:        "empty (backwards compat)",
			apiKeyScope: "",
		},
		{
			name:        "all",
			apiKeyScope: rbac.ScopeAll,
		},
		{
			name:        "no_user_data",
			apiKeyScope: rbac.ScopeNoUserData,
		},
		{
			name:        "application_connect",
			apiKeyScope: rbac.ScopeApplicationConnect,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tickCh := make(chan time.Time)
			flushCh := make(chan int, 1)
			client, db := coderdtest.NewWithDatabase(t, &coderdtest.Options{
				WorkspaceUsageTrackerFlush: flushCh,
				WorkspaceUsageTrackerTick:  tickCh,
			})
			user := coderdtest.CreateFirstUser(t, client)
			r := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
				OrganizationID: user.OrganizationID,
				OwnerID:        user.UserID,
				LastUsedAt:     dbtime.Now().Add(-time.Minute),
			}).WithAgent(
				func(agent []*proto.Agent) []*proto.Agent {
					for _, a := range agent {
						a.ApiKeyScope = string(tc.apiKeyScope)
					}

					return agent
				},
			).Do()

			ac := agentsdk.New(client.URL, agentsdk.WithFixedToken(r.AgentToken))
			conn, err := ac.ConnectRPC(context.Background())
			require.NoError(t, err)
			defer func() {
				_ = conn.Close()
			}()
			agentAPI := agentproto.NewDRPCAgentClient(conn)

			_, err = agentAPI.UpdateStats(context.Background(), &agentproto.UpdateStatsRequest{
				Stats: &agentproto.Stats{
					ConnectionsByProto:          map[string]int64{"TCP": 1},
					ConnectionCount:             1,
					RxPackets:                   1,
					RxBytes:                     1,
					TxPackets:                   1,
					TxBytes:                     1,
					SessionCountVscode:          1,
					SessionCountJetbrains:       0,
					SessionCountReconnectingPty: 0,
					SessionCountSsh:             0,
					ConnectionMedianLatencyMs:   10,
				},
			})
			require.NoError(t, err)

			tickCh <- dbtime.Now()
			count := <-flushCh
			require.Equal(t, 1, count, "expected one flush with one id")

			newWorkspace, err := client.Workspace(context.Background(), r.Workspace.ID)
			require.NoError(t, err)

			assert.True(t,
				newWorkspace.LastUsedAt.After(r.Workspace.LastUsedAt),
				"%s is not after %s", newWorkspace.LastUsedAt, r.Workspace.LastUsedAt,
			)
		})
	}
}

func TestWorkspaceAgentRPC_TailnetMethods(t *testing.T) {
	t.Parallel()

	ctx := testutil.Context(t, testutil.WaitLong)
	client, db := coderdtest.NewWithDatabase(t, nil)
	user := coderdtest.CreateFirstUser(t, client)
	workspace := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
		OrganizationID: user.OrganizationID,
		OwnerID:        user.UserID,
	}).WithAgent().Do()

	agentClient := agentsdk.New(client.URL, agentsdk.WithFixedToken(workspace.AgentToken))
	conn, err := agentClient.ConnectRPC(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})

	tailnetClient := tailnetproto.NewDRPCTailnetClient(conn)
	_, err = tailnetClient.RefreshResumeToken(ctx, &tailnetproto.RefreshResumeTokenRequest{})
	require.Error(t, err)
	require.EqualValues(t, drpcerr.Unimplemented, drpcerr.Code(err))

	updates, err := tailnetClient.WorkspaceUpdates(ctx, &tailnetproto.WorkspaceUpdatesRequest{
		WorkspaceOwnerId: tailnet.UUIDToByteSlice(user.UserID),
	})
	if err == nil {
		_, err = updates.Recv()
	}
	require.Error(t, err)
	require.EqualValues(t, drpcerr.Unimplemented, drpcerr.Code(err))

	telemetry, err := tailnetClient.PostTelemetry(ctx, &tailnetproto.TelemetryRequest{})
	require.NoError(t, err)
	require.NotNil(t, telemetry)

	agentAPI := agentproto.NewDRPCAgentClient(conn)
	_, err = agentAPI.GetManifest(ctx, &agentproto.GetManifestRequest{})
	require.NoError(t, err)
}

func TestAgentAPI_LargeManifest(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		apiKeyScope rbac.ScopeName
	}{
		{
			name:        "empty (backwards compat)",
			apiKeyScope: "",
		},
		{
			name:        "all",
			apiKeyScope: rbac.ScopeAll,
		},
		{
			name:        "no_user_data",
			apiKeyScope: rbac.ScopeNoUserData,
		},
		{
			name:        "application_connect",
			apiKeyScope: rbac.ScopeApplicationConnect,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := testutil.Context(t, testutil.WaitLong)
			client, store := coderdtest.NewWithDatabase(t, nil)
			adminUser := coderdtest.CreateFirstUser(t, client)
			n := 512000
			longScript := make([]byte, n)
			for i := range longScript {
				longScript[i] = 'q'
			}
			r := dbfake.WorkspaceBuild(t, store, database.WorkspaceTable{
				OrganizationID: adminUser.OrganizationID,
				OwnerID:        adminUser.UserID,
			}).WithAgent(func(agents []*proto.Agent) []*proto.Agent {
				agents[0].Scripts = []*proto.Script{
					{
						Script: string(longScript),
					},
				}
				agents[0].ApiKeyScope = string(tc.apiKeyScope)
				return agents
			}).Do()
			ac := agentsdk.New(client.URL, agentsdk.WithFixedToken(r.AgentToken))
			conn, err := ac.ConnectRPC(ctx)
			defer func() {
				_ = conn.Close()
			}()
			require.NoError(t, err)
			agentAPI := agentproto.NewDRPCAgentClient(conn)
			manifest, err := agentAPI.GetManifest(ctx, &agentproto.GetManifestRequest{})
			require.NoError(t, err)
			require.Len(t, manifest.Scripts, 1)
			require.Len(t, manifest.Scripts[0].Script, n)
		})
	}
}

// End-to-end wiring: the deployment option must reach the manifest the
// agent actually receives over RPC.
func TestWorkspaceAgentRPCUserSecretFilePathDisabled(t *testing.T) {
	t.Parallel()

	ctx := testutil.Context(t, testutil.WaitLong)
	dv := coderdtest.DeploymentValues(t, func(dv *codersdk.DeploymentValues) {
		dv.DisableUserSecretFilePath = true
	})
	db, ps := dbtestutil.NewDB(t)
	client := coderdtest.New(t, &coderdtest.Options{
		Database: db, Pubsub: ps, DeploymentValues: dv,
	})
	owner := coderdtest.CreateFirstUser(t, client)

	// Seed rows the way a deployment that allowed file paths would have
	// stored them; the API rejects new paths while the policy is on.
	for _, seed := range []database.UserSecret{
		{Name: "env-only", Value: "env-value", EnvName: "ENV_ONLY"},
		{Name: "file-only", Value: "file-value", FilePath: "~/.file-only"},
		{Name: "dual-target", Value: "dual-value", EnvName: "DUAL_TARGET", FilePath: "~/.dual-target"},
	} {
		dbgen.UserSecret(t, db, database.UserSecret{UserID: owner.UserID, Name: seed.Name},
			func(p *database.CreateUserSecretParams) {
				p.Value, p.EnvName, p.FilePath, p.Enabled = seed.Value, seed.EnvName, seed.FilePath, true
			})
	}

	workspace := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
		OrganizationID: owner.OrganizationID,
		OwnerID:        owner.UserID,
	}).WithAgent().Do()

	agentClient := agentsdk.New(client.URL, agentsdk.WithFixedToken(workspace.AgentToken))
	conn, err := agentClient.ConnectRPC(ctx)
	require.NoError(t, err)
	defer conn.Close()

	manifest, err := agentproto.NewDRPCAgentClient(conn).GetManifest(ctx, &agentproto.GetManifestRequest{})
	require.NoError(t, err)

	require.Len(t, manifest.Secrets, 2)
	byEnv := make(map[string][]byte, len(manifest.Secrets))
	for _, secret := range manifest.Secrets {
		require.Empty(t, secret.FilePath)
		byEnv[secret.EnvName] = secret.Value
	}
	require.Equal(t, map[string][]byte{
		"ENV_ONLY":    []byte("env-value"),
		"DUAL_TARGET": []byte("dual-value"),
	}, byEnv)
}

func TestWorkspaceAgentRPCRole(t *testing.T) {
	t.Parallel()

	t.Run("AgentRoleMonitorsConnection", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)
		client, db := coderdtest.NewWithDatabase(t, nil)
		user := coderdtest.CreateFirstUser(t, client)
		r := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
			OrganizationID: user.OrganizationID,
			OwnerID:        user.UserID,
		}).WithAgent().Do()

		// Connect with role=agent using ConnectRPCWithRole. This is
		// how the real workspace agent connects.
		ac := agentsdk.New(client.URL, agentsdk.WithFixedToken(r.AgentToken))
		conn, err := ac.ConnectRPCWithRole(ctx, "agent")
		require.NoError(t, err)
		defer func() {
			_ = conn.Close()
		}()

		// The connection monitor updates the database asynchronously,
		// so we need to wait for first_connected_at to be set.
		var agent database.WorkspaceAgent
		require.Eventually(t, func() bool {
			agent, err = db.GetWorkspaceAgentByID(dbauthz.AsSystemRestricted(ctx), r.Agents[0].ID)
			if err != nil {
				return false
			}
			return agent.FirstConnectedAt.Valid
		}, testutil.WaitShort, testutil.IntervalFast)
		assert.True(t, agent.LastConnectedAt.Valid,
			"last_connected_at should be set for agent role")
	})

	t.Run("NonAgentRoleSkipsMonitoring", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)
		client, db := coderdtest.NewWithDatabase(t, nil)
		user := coderdtest.CreateFirstUser(t, client)
		r := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
			OrganizationID: user.OrganizationID,
			OwnerID:        user.UserID,
		}).WithAgent().Do()

		// Connect with a non-agent role using ConnectRPCWithRole.
		// This is how coder-logstream-kube should connect.
		ac := agentsdk.New(client.URL, agentsdk.WithFixedToken(r.AgentToken))
		conn, err := ac.ConnectRPCWithRole(ctx, "logstream-kube")
		require.NoError(t, err)

		// Send a log to confirm the RPC connection is functional.
		agentAPI := agentproto.NewDRPCAgentClient(conn)
		_, err = agentAPI.BatchCreateLogs(ctx, &agentproto.BatchCreateLogsRequest{
			LogSourceId: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		})
		// We don't care about the log source error, just that the
		// RPC is functional.
		_ = err

		// Close the connection and give the server time to process.
		_ = conn.Close()

		// Verify that connectivity timestamps were never set
		// (first_connected_at, last_connected_at, disconnected_at).
		require.Never(t, func() bool {
			agent, err := db.GetWorkspaceAgentByID(dbauthz.AsSystemRestricted(ctx), r.Agents[0].ID)
			if err != nil {
				return false
			}
			return agent.FirstConnectedAt.Valid || agent.LastConnectedAt.Valid || agent.DisconnectedAt.Valid
		}, testutil.IntervalMedium, testutil.IntervalFast, "connectivity timestamps should NOT be set for non-agent role")
	})

	// NOTE: Backward compatibility (empty role) is implicitly tested by
	// existing tests like TestWorkspaceAgentReportStats which use
	// ConnectRPC() (no role). The server defaults to monitoring when
	// the role query parameter is omitted.
}

// rawProtoEncoding sends pre-encoded bytes as-is so a test can put exactly
// what an older agent would send on the wire.
type rawProtoEncoding struct{}

func (rawProtoEncoding) Marshal(msg drpc.Message) ([]byte, error) {
	if b, ok := msg.([]byte); ok {
		return b, nil
	}
	//nolint:forcetypeassert // Every other message here is a protobuf message.
	return protobuf.Marshal(msg.(protobuf.Message))
}

func (rawProtoEncoding) Unmarshal(buf []byte, msg drpc.Message) error {
	//nolint:forcetypeassert // Every response here is a protobuf message.
	return protobuf.Unmarshal(buf, msg.(protobuf.Message))
}

// Agents of every version reach coderd through the same RPC. Older agents
// send only the deprecated type, current agents add connection_method and
// app_name, and unknown methods are rejected without a record.
func TestWorkspaceAgentRPC_ReportConnectionWire(t *testing.T) {
	t.Parallel()

	const sessionID = "0123456789abcdef0123456789abcdef"
	connLogger := connectionlog.NewFake()
	client, db := coderdtest.NewWithDatabase(t, &coderdtest.Options{
		ConnectionLogger: connLogger,
	})
	user := coderdtest.CreateFirstUser(t, client)
	r := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
		OrganizationID: user.OrganizationID,
		OwnerID:        user.UserID,
	}).WithAgent().Do()
	ac := agentsdk.New(client.URL, agentsdk.WithFixedToken(r.AgentToken))

	// connect opens an agent RPC connection for one subtest. Parallel
	// subtests can start long after this function returns, so each owns its
	// context and connection rather than sharing ones whose deadline may
	// already have passed.
	connect := func(t *testing.T) (report func(req []byte) error) {
		ctx := testutil.Context(t, testutil.WaitLong)
		conn, err := ac.ConnectRPC(ctx)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = conn.Close()
		})
		return func(req []byte) error {
			return conn.Invoke(ctx, "/coder.agent.v2.Agent/ReportConnection", rawProtoEncoding{}, req, &emptypb.Empty{})
		}
	}

	timestamp, err := protobuf.Marshal(timestamppb.New(dbtime.Now()))
	require.NoError(t, err)
	// oldAgentReport encodes a request the way an agent without
	// connection_method and app_name does: id, action, type, timestamp, ip,
	// and client_session_id only.
	oldAgentReport := func(id uuid.UUID, action agentproto.Connection_Action, typ agentproto.Connection_Type) []byte {
		var c []byte
		c = protowire.AppendTag(c, 1, protowire.BytesType)
		c = protowire.AppendBytes(c, id[:])
		c = protowire.AppendTag(c, 2, protowire.VarintType)
		c = protowire.AppendVarint(c, uint64(action)) //nolint:gosec // Enum values here are non-negative.
		c = protowire.AppendTag(c, 3, protowire.VarintType)
		c = protowire.AppendVarint(c, uint64(typ)) //nolint:gosec // Enum values here are non-negative.
		c = protowire.AppendTag(c, 4, protowire.BytesType)
		c = protowire.AppendBytes(c, timestamp)
		c = protowire.AppendTag(c, 5, protowire.BytesType)
		c = protowire.AppendString(c, "127.0.0.1")
		c = protowire.AppendTag(c, 8, protowire.BytesType)
		c = protowire.AppendString(c, sessionID)
		var req []byte
		req = protowire.AppendTag(req, 1, protowire.BytesType)
		return protowire.AppendBytes(req, c)
	}
	newAgentReport := func(t *testing.T, id uuid.UUID, action agentproto.Connection_Action, method agentproto.Connection_Method, typ agentproto.Connection_Type, appName string) []byte {
		req, err := protobuf.Marshal(&agentproto.ReportConnectionRequest{
			Connection: &agentproto.Connection{
				Id:               id[:],
				Action:           action,
				Type:             typ,
				Timestamp:        timestamppb.New(dbtime.Now()),
				Ip:               "127.0.0.1",
				ClientSessionId:  sessionID,
				AppName:          appName,
				ConnectionMethod: method,
			},
		})
		require.NoError(t, err)
		return req
	}
	logsFor := func(id uuid.UUID) []database.UpsertConnectionLogParams {
		var logs []database.UpsertConnectionLogParams
		for _, l := range connLogger.ConnectionLogs() {
			if l.ConnectionID.Valid && l.ConnectionID.UUID == id {
				logs = append(logs, l)
			}
		}
		return logs
	}
	oldAgent := func(typ agentproto.Connection_Type) func(*testing.T, uuid.UUID, agentproto.Connection_Action) []byte {
		return func(_ *testing.T, id uuid.UUID, action agentproto.Connection_Action) []byte {
			return oldAgentReport(id, action, typ)
		}
	}
	newAgent := func(method agentproto.Connection_Method, typ agentproto.Connection_Type, appName string) func(*testing.T, uuid.UUID, agentproto.Connection_Action) []byte {
		return func(t *testing.T, id uuid.UUID, action agentproto.Connection_Action) []byte {
			return newAgentReport(t, id, action, method, typ, appName)
		}
	}

	for _, tc := range []struct {
		name    string
		report  func(*testing.T, uuid.UUID, agentproto.Connection_Action) []byte
		method  database.ConnectionLogMethod
		appName string
	}{
		{"OldSSH", oldAgent(agentproto.Connection_SSH), database.ConnectionLogMethodSSH, ""},
		// Agents before connection_method sent unfamiliar SSH apps as
		// unspecified.
		{"OldUnspecified", oldAgent(agentproto.Connection_TYPE_UNSPECIFIED), database.ConnectionLogMethodSSH, ""},
		{"OldVSCode", oldAgent(agentproto.Connection_VSCODE), database.ConnectionLogMethodSSH, "vscode"},
		{"OldJetBrains", oldAgent(agentproto.Connection_JETBRAINS), database.ConnectionLogMethodSSH, "jetbrains"},
		{"OldReconnectingPTY", oldAgent(agentproto.Connection_RECONNECTING_PTY), database.ConnectionLogMethodReconnectingPTY, ""},
		{"NewUnfamiliarApp", newAgent(agentproto.Connection_METHOD_SSH, agentproto.Connection_SSH, "Some-New-IDE"), database.ConnectionLogMethodSSH, "some_new_ide"},
		{"NewVSCodeFork", newAgent(agentproto.Connection_METHOD_SSH, agentproto.Connection_VSCODE, "cursor"), database.ConnectionLogMethodSSH, "cursor"},
		{"NewSSHNamingWebTerminal", newAgent(agentproto.Connection_METHOD_SSH, agentproto.Connection_SSH, "reconnecting_pty"), database.ConnectionLogMethodSSH, "reconnecting_pty"},
		{"NewReconnectingPTY", newAgent(agentproto.Connection_METHOD_RECONNECTING_PTY, agentproto.Connection_RECONNECTING_PTY, ""), database.ConnectionLogMethodReconnectingPTY, ""},
		// Port forwarding is never logged as SSH, and the app name is not
		// stored where the destination belongs.
		{"NewPortForwarding", newAgent(agentproto.Connection_METHOD_PORT_FORWARDING, agentproto.Connection_TYPE_UNSPECIFIED, ""), database.ConnectionLogMethodPortForwarding, ""},
		{"NewPortForwardingSSHType", newAgent(agentproto.Connection_METHOD_PORT_FORWARDING, agentproto.Connection_SSH, "ssh"), database.ConnectionLogMethodPortForwarding, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := connect(t)
			id := uuid.New()
			require.NoError(t, report(tc.report(t, id, agentproto.Connection_CONNECT)))
			require.NoError(t, report(tc.report(t, id, agentproto.Connection_DISCONNECT)))

			logs := logsFor(id)
			require.Len(t, logs, 2)
			for _, l := range logs {
				require.Equal(t, tc.method, l.ConnectionMethod)
				require.Equal(t, sql.NullString{String: tc.appName, Valid: tc.appName != ""}, l.AppNameOrPort)
				require.Equal(t, sessionID, l.ClientSessionID.String)
			}
			require.Equal(t, database.ConnectionStatusConnected, logs[0].ConnectionStatus)
			require.Equal(t, database.ConnectionStatusDisconnected, logs[1].ConnectionStatus)
		})
	}

	for _, tc := range []struct {
		name   string
		method agentproto.Connection_Method
		typ    agentproto.Connection_Type
	}{
		{"UnknownMethod", agentproto.Connection_Method(42), agentproto.Connection_SSH},
		{"UnknownMethodUnspecifiedType", agentproto.Connection_Method(4), agentproto.Connection_TYPE_UNSPECIFIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := connect(t)
			id := uuid.New()
			// Match the server's rejection so a broken connection cannot pass
			// for one.
			err := report(newAgentReport(t, id, agentproto.Connection_CONNECT, tc.method, tc.typ, "ssh"))
			require.ErrorContains(t, err, "unsupported agent connection method")
			require.Empty(t, logsFor(id))

			// A rejected report does not close the connection.
			validID := uuid.New()
			require.NoError(t, report(newAgentReport(t, validID, agentproto.Connection_CONNECT, agentproto.Connection_METHOD_SSH, agentproto.Connection_SSH, "")))
			require.Len(t, logsFor(validID), 1)
		})
	}
}

// The identity a real agent reports for SSH and reconnecting PTY does not
// depend on the workspace usage experiment, which only changes how session
// counts are collected.
func TestWorkspaceAgentConnectionLogIdentity(t *testing.T) {
	t.Parallel()

	for _, experiment := range []bool{false, true} {
		name := "WorkspaceUsageDisabled"
		if experiment {
			name = "WorkspaceUsageEnabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := testutil.Context(t, testutil.WaitLong)
			dv := coderdtest.DeploymentValues(t)
			if experiment {
				dv.Experiments = []string{string(codersdk.ExperimentWorkspaceUsage)}
			}
			connLogger := connectionlog.NewFake()
			client, db := coderdtest.NewWithDatabase(t, &coderdtest.Options{
				DeploymentValues: dv,
				ConnectionLogger: connLogger,
			})
			user := coderdtest.CreateFirstUser(t, client)
			r := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
				OrganizationID: user.OrganizationID,
				OwnerID:        user.UserID,
			}).WithAgent().Do()
			_ = agenttest.New(t, client.URL, r.AgentToken)
			resources := coderdtest.NewWorkspaceAgentWaiter(t, client, r.Workspace.ID).Wait()

			conn, err := workspacesdk.New(client).DialAgent(ctx, resources[0].Agents[0].ID, &workspacesdk.DialAgentOptions{
				Logger: testutil.Logger(t),
			})
			require.NoError(t, err)
			defer conn.Close()

			sshClient, err := conn.SSHClient(ctx)
			require.NoError(t, err)
			defer sshClient.Close()
			for _, env := range []string{"", "ssh", "Cursor", "Some-New-IDE", "Reconnecting-PTY"} {
				sess, err := sshClient.NewSession()
				require.NoError(t, err)
				if env != "" {
					require.NoError(t, sess.Setenv(agentssh.AppNameEnvironmentVariable, env))
				}
				require.NoError(t, sess.Run("echo hello"), env)
			}

			ptyConn, err := conn.ReconnectingPTY(ctx, uuid.New(), 80, 80, "echo hello")
			require.NoError(t, err)
			require.NoError(t, ptyConn.Close())

			type identity struct {
				method  database.ConnectionLogMethod
				appName sql.NullString
			}
			want := []identity{
				// Without a supplied app, the identity stays absent.
				{database.ConnectionLogMethodSSH, sql.NullString{}},
				{database.ConnectionLogMethodSSH, sql.NullString{String: "ssh", Valid: true}},
				{database.ConnectionLogMethodSSH, sql.NullString{String: "cursor", Valid: true}},
				{database.ConnectionLogMethodSSH, sql.NullString{String: "some_new_ide", Valid: true}},
				{database.ConnectionLogMethodSSH, sql.NullString{String: "reconnecting_pty", Valid: true}},
				// Reconnecting PTY reports no app identity.
				{database.ConnectionLogMethodReconnectingPTY, sql.NullString{}},
			}
			// Each connection logs a connect and a disconnect, and both carry
			// the same identity.
			var got map[uuid.UUID][]database.UpsertConnectionLogParams
			require.Eventually(t, func() bool {
				got = make(map[uuid.UUID][]database.UpsertConnectionLogParams)
				for _, l := range connLogger.ConnectionLogs() {
					if l.WorkspaceID == r.Workspace.ID && l.ConnectionID.Valid {
						got[l.ConnectionID.UUID] = append(got[l.ConnectionID.UUID], l)
					}
				}
				if len(got) != len(want) {
					return false
				}
				for _, logs := range got {
					if len(logs) != 2 {
						return false
					}
				}
				return true
			}, testutil.WaitLong, testutil.IntervalFast)

			identities := make([]identity, 0, len(got))
			for _, logs := range got {
				require.Equal(t, logs[0].ConnectionMethod, logs[1].ConnectionMethod)
				require.Equal(t, logs[0].AppNameOrPort, logs[1].AppNameOrPort)
				require.Equal(t, logs[0].ClientSessionID, logs[1].ClientSessionID)
				identities = append(identities, identity{logs[0].ConnectionMethod, logs[0].AppNameOrPort})
			}
			require.ElementsMatch(t, want, identities)
		})
	}
}
