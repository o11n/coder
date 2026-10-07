// Package sdk2db provides common conversion routines from codersdk types to database types
package sdk2db

import (
	"golang.org/x/xerrors"

	agentproto "github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/util/slice"
	"github.com/coder/coder/v2/codersdk"
)

func ProvisionerDaemonStatus(status codersdk.ProvisionerDaemonStatus) database.ProvisionerDaemonStatus {
	return database.ProvisionerDaemonStatus(status)
}

func ProvisionerDaemonStatuses(params []codersdk.ProvisionerDaemonStatus) []database.ProvisionerDaemonStatus {
	return slice.List(params, ProvisionerDaemonStatus)
}

// ConnectionLogTypeFilter translates the deprecated type filter into methods
// and SSH app families. Plain SSH includes absent and unregistered identities.
func ConnectionLogTypeFilter(t codersdk.ConnectionType) (method string, appNames, excludedAppNames []string) {
	switch t {
	case "":
		return "", nil, nil
	case codersdk.ConnectionTypeVSCode, codersdk.ConnectionTypeJetBrains:
		return string(database.ConnectionLogMethodSSH), t.AppNames(), nil
	case codersdk.ConnectionTypeSSH:
		return string(database.ConnectionLogMethodSSH), nil, append(codersdk.ConnectionTypeVSCode.AppNames(), codersdk.ConnectionTypeJetBrains.AppNames()...)
	default:
		return string(t), nil, nil
	}
}

// ConnectionLogFromAgentType recovers method and identity from legacy reports.
// An unspecified legacy type was used by SSH handlers for unfamiliar apps.
func ConnectionLogFromAgentType(typ agentproto.Connection_Type) (database.ConnectionLogMethod, string, error) {
	switch typ {
	case agentproto.Connection_SSH, agentproto.Connection_TYPE_UNSPECIFIED:
		return database.ConnectionLogMethodSSH, "", nil
	case agentproto.Connection_JETBRAINS:
		return database.ConnectionLogMethodSSH, "jetbrains", nil
	case agentproto.Connection_VSCODE:
		return database.ConnectionLogMethodSSH, "vscode", nil
	case agentproto.Connection_RECONNECTING_PTY:
		return database.ConnectionLogMethodReconnectingPTY, "", nil
	default:
		return "", "", xerrors.Errorf("unsupported agent connection type %d", typ)
	}
}

// ConnectionLogFromAgentConnection returns the method and app identity to log
// for an agent connection report. An explicit method wins, and the deprecated
// type is read only when the method is unspecified or to recover a VS Code or
// JetBrains identity for SSH without an app name. Unknown methods are rejected
// so they are never recorded as SSH.
func ConnectionLogFromAgentConnection(conn *agentproto.Connection) (database.ConnectionLogMethod, string, error) {
	var method database.ConnectionLogMethod
	switch m := conn.GetConnectionMethod(); m {
	case agentproto.Connection_METHOD_SSH:
		method = database.ConnectionLogMethodSSH
	case agentproto.Connection_METHOD_RECONNECTING_PTY:
		method = database.ConnectionLogMethodReconnectingPTY
	case agentproto.Connection_METHOD_UNSPECIFIED:
		legacyMethod, legacyAppName, err := ConnectionLogFromAgentType(conn.GetType())
		if err != nil {
			return "", "", err
		}
		if appName := conn.GetAppName(); appName != "" {
			return legacyMethod, codersdk.NormalizeAppName(appName), nil
		}
		return legacyMethod, legacyAppName, nil
	case agentproto.Connection_METHOD_PORT_FORWARDING:
		// For port forwarding, the shared column holds the destination,
		// which reports do not carry yet, so the app name is not stored.
		return database.ConnectionLogMethodPortForwarding, "", nil
	default:
		return "", "", xerrors.Errorf("unsupported agent connection method %d", m)
	}

	if appName := conn.GetAppName(); appName != "" {
		return method, codersdk.NormalizeAppName(appName), nil
	}
	if method == database.ConnectionLogMethodSSH {
		if legacyMethod, legacyAppName, err := ConnectionLogFromAgentType(conn.GetType()); err == nil && legacyMethod == method {
			return method, legacyAppName, nil
		}
	}
	return method, "", nil
}
