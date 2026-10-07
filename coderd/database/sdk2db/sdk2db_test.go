package sdk2db_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	agentproto "github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/db2sdk"
	"github.com/coder/coder/v2/coderd/database/sdk2db"
	"github.com/coder/coder/v2/codersdk"
)

func TestProvisionerDaemonStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  codersdk.ProvisionerDaemonStatus
		expect database.ProvisionerDaemonStatus
	}{
		{"busy", codersdk.ProvisionerDaemonBusy, database.ProvisionerDaemonStatusBusy},
		{"offline", codersdk.ProvisionerDaemonOffline, database.ProvisionerDaemonStatusOffline},
		{"idle", codersdk.ProvisionerDaemonIdle, database.ProvisionerDaemonStatusIdle},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sdk2db.ProvisionerDaemonStatus(tc.input)
			if !got.Valid() {
				t.Errorf("ProvisionerDaemonStatus(%v) returned invalid status", tc.input)
			}
			if got != tc.expect {
				t.Errorf("ProvisionerDaemonStatus(%v) = %v; want %v", tc.input, got, tc.expect)
			}
		})
	}
}

// A row matched by a type filter reads back as that type.
func TestConnectionLogTypeFilter(t *testing.T) {
	t.Parallel()

	sshFamilies := slices.Concat(codersdk.ConnectionTypeVSCode.AppNames(), codersdk.ConnectionTypeJetBrains.AppNames())
	for _, tc := range []struct {
		typ              codersdk.ConnectionType
		method           string
		appNames         []string
		excludedAppNames []string
	}{
		{typ: ""},
		{typ: codersdk.ConnectionTypeSSH, method: "ssh", excludedAppNames: sshFamilies},
		{typ: codersdk.ConnectionTypeVSCode, method: "ssh", appNames: codersdk.ConnectionTypeVSCode.AppNames()},
		{typ: codersdk.ConnectionTypeJetBrains, method: "ssh", appNames: codersdk.ConnectionTypeJetBrains.AppNames()},
		{typ: codersdk.ConnectionTypeReconnectingPTY, method: "reconnecting_pty"},
		{typ: codersdk.ConnectionTypeWorkspaceApp, method: "workspace_app"},
		{typ: codersdk.ConnectionTypePortForwarding, method: "port_forwarding"},
		{typ: codersdk.ConnectionTypeTunnel, method: "tunnel"},
	} {
		method, appNames, excludedAppNames := sdk2db.ConnectionLogTypeFilter(tc.typ)
		require.Equal(t, tc.method, method, tc.typ)
		require.ElementsMatch(t, tc.appNames, appNames, tc.typ)
		require.ElementsMatch(t, tc.excludedAppNames, excludedAppNames, tc.typ)
	}

	for _, typ := range codersdk.FilterableConnectionTypes() {
		method, appNames, excludedAppNames := sdk2db.ConnectionLogTypeFilter(typ)
		require.True(t, database.ConnectionLogMethod(method).Valid(), typ)
		switch {
		case len(appNames) > 0:
			for _, appName := range appNames {
				require.Equal(t, string(typ), db2sdk.ConnectionLogType(database.ConnectionLogMethod(method), appName), appName)
			}
		case len(excludedAppNames) > 0:
			// Plain SSH also matches absent and unregistered identities.
			for _, appName := range []string{"", "ssh", "zed", "an_unregistered_ide"} {
				require.NotContains(t, excludedAppNames, appName)
				require.Equal(t, string(typ), db2sdk.ConnectionLogType(database.ConnectionLogMethod(method), appName), appName)
			}
			for _, appName := range excludedAppNames {
				require.NotEqual(t, string(typ), db2sdk.ConnectionLogType(database.ConnectionLogMethod(method), appName), appName)
			}
		default:
			require.Equal(t, string(typ), db2sdk.ConnectionLogType(database.ConnectionLogMethod(method), ""), typ)
		}
	}
}

func TestConnectionLogFromAgentType(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		typ     agentproto.Connection_Type
		method  database.ConnectionLogMethod
		appName string
	}{
		{agentproto.Connection_SSH, database.ConnectionLogMethodSSH, ""},
		// SSH handlers reported unfamiliar apps as unspecified.
		{agentproto.Connection_TYPE_UNSPECIFIED, database.ConnectionLogMethodSSH, ""},
		{agentproto.Connection_VSCODE, database.ConnectionLogMethodSSH, "vscode"},
		{agentproto.Connection_JETBRAINS, database.ConnectionLogMethodSSH, "jetbrains"},
		{agentproto.Connection_RECONNECTING_PTY, database.ConnectionLogMethodReconnectingPTY, ""},
	} {
		method, appName, err := sdk2db.ConnectionLogFromAgentType(tc.typ)
		require.NoError(t, err, tc.typ)
		require.Equal(t, tc.method, method, tc.typ)
		require.Equal(t, tc.appName, appName, tc.typ)
	}

	_, _, err := sdk2db.ConnectionLogFromAgentType(agentproto.Connection_Type(1000))
	require.Error(t, err)
}

func TestConnectionLogFromAgentConnection(t *testing.T) {
	t.Parallel()

	const (
		ssh  = database.ConnectionLogMethodSSH
		pty  = database.ConnectionLogMethodReconnectingPTY
		port = database.ConnectionLogMethodPortForwarding
	)
	unknownType := agentproto.Connection_Type(1000)
	for _, tc := range []struct {
		name    string
		method  agentproto.Connection_Method
		typ     agentproto.Connection_Type
		appName string
		want    database.ConnectionLogMethod
		wantApp string
	}{
		// Current agents send the method, the app, and a legacy type.
		{"SSH", agentproto.Connection_METHOD_SSH, agentproto.Connection_SSH, "ssh", ssh, "ssh"},
		{"VSCodeFork", agentproto.Connection_METHOD_SSH, agentproto.Connection_VSCODE, "cursor", ssh, "cursor"},
		{"Unfamiliar", agentproto.Connection_METHOD_SSH, agentproto.Connection_SSH, "Some-New-IDE", ssh, "some_new_ide"},
		{"WhitespaceName", agentproto.Connection_METHOD_SSH, agentproto.Connection_SSH, "  ", ssh, "unknown"},
		// An SSH client naming reconnecting_pty stays SSH.
		{"SSHClaimsWebTerminal", agentproto.Connection_METHOD_SSH, agentproto.Connection_SSH, "reconnecting_pty", ssh, "reconnecting_pty"},
		{"SSHIgnoresPTYType", agentproto.Connection_METHOD_SSH, agentproto.Connection_RECONNECTING_PTY, "", ssh, ""},
		{"SSHRecoversVSCode", agentproto.Connection_METHOD_SSH, agentproto.Connection_VSCODE, "", ssh, "vscode"},
		{"SSHRecoversJetBrains", agentproto.Connection_METHOD_SSH, agentproto.Connection_JETBRAINS, "", ssh, "jetbrains"},
		{"SSHWithoutApp", agentproto.Connection_METHOD_SSH, agentproto.Connection_SSH, "", ssh, ""},
		{"SSHIgnoresUnknownType", agentproto.Connection_METHOD_SSH, unknownType, "", ssh, ""},
		{"WebTerminal", agentproto.Connection_METHOD_RECONNECTING_PTY, agentproto.Connection_RECONNECTING_PTY, "", pty, ""},
		{"WebTerminalDoesNotRecover", agentproto.Connection_METHOD_RECONNECTING_PTY, agentproto.Connection_VSCODE, "", pty, ""},
		{"WebTerminalApp", agentproto.Connection_METHOD_RECONNECTING_PTY, agentproto.Connection_RECONNECTING_PTY, "Some-App", pty, "some_app"},
		// Port forwarding never falls back to the legacy type, and the app
		// name is not stored where the destination belongs.
		{"PortForwarding", agentproto.Connection_METHOD_PORT_FORWARDING, agentproto.Connection_TYPE_UNSPECIFIED, "", port, ""},
		{"PortForwardingIgnoresSSHType", agentproto.Connection_METHOD_PORT_FORWARDING, agentproto.Connection_SSH, "ssh", port, ""},
		{"PortForwardingDoesNotRecover", agentproto.Connection_METHOD_PORT_FORWARDING, agentproto.Connection_VSCODE, "", port, ""},
		{"PortForwardingIgnoresAppName", agentproto.Connection_METHOD_PORT_FORWARDING, agentproto.Connection_TYPE_UNSPECIFIED, "8080", port, ""},
		// Agents without connection_method send only the type.
		{"LegacySSH", agentproto.Connection_METHOD_UNSPECIFIED, agentproto.Connection_SSH, "", ssh, ""},
		{"LegacyUnspecified", agentproto.Connection_METHOD_UNSPECIFIED, agentproto.Connection_TYPE_UNSPECIFIED, "", ssh, ""},
		{"LegacyVSCode", agentproto.Connection_METHOD_UNSPECIFIED, agentproto.Connection_VSCODE, "", ssh, "vscode"},
		{"LegacyJetBrains", agentproto.Connection_METHOD_UNSPECIFIED, agentproto.Connection_JETBRAINS, "", ssh, "jetbrains"},
		{"LegacyPTY", agentproto.Connection_METHOD_UNSPECIFIED, agentproto.Connection_RECONNECTING_PTY, "", pty, ""},
		{"LegacyWithAppName", agentproto.Connection_METHOD_UNSPECIFIED, agentproto.Connection_VSCODE, "Cursor", ssh, "cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			method, appName, err := sdk2db.ConnectionLogFromAgentConnection(&agentproto.Connection{
				ConnectionMethod: tc.method,
				Type:             tc.typ,
				AppName:          tc.appName,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, method)
			require.Equal(t, tc.wantApp, appName)
		})
	}

	// Unknown methods are rejected before the legacy type is read, so none is
	// recorded as SSH.
	for _, method := range []agentproto.Connection_Method{
		agentproto.Connection_Method(4),
		agentproto.Connection_Method(42),
	} {
		for _, typ := range []agentproto.Connection_Type{
			agentproto.Connection_TYPE_UNSPECIFIED,
			agentproto.Connection_SSH,
			agentproto.Connection_VSCODE,
		} {
			_, _, err := sdk2db.ConnectionLogFromAgentConnection(&agentproto.Connection{
				ConnectionMethod: method,
				Type:             typ,
				AppName:          "ssh",
			})
			require.Error(t, err, "method %d type %s", method, typ)
		}
	}

	_, _, err := sdk2db.ConnectionLogFromAgentConnection(&agentproto.Connection{Type: unknownType})
	require.Error(t, err)
}
