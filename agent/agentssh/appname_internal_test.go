package agentssh

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/codersdk"
)

func TestExtractAppName(t *testing.T) {
	t.Parallel()

	envWith := func(value string) []string {
		return []string{
			"FOO=bar",
			fmt.Sprintf("%s=%s", AppNameEnvironmentVariable, value),
			"BAZ=qux",
		}
	}

	for _, tc := range []struct {
		name string
		env  []string
		// keepEnv, when set, must survive filtering.
		keepEnv string
		want    string
	}{
		{"NoEnvDefaultsToSSH", []string{"FOO=bar"}, "FOO=bar", "ssh"},
		{"EmptyValueDefaultsToSSH", envWith(""), "", "ssh"},
		{"VSCode", envWith("vscode"), "", "vscode"},
		{"JetBrainsLegacyCasing", envWith("JetBrains"), "", "jetbrains"},
		{"UnknownTypeNormalized", envWith("Cursor-Nightly"), "", "cursor_nightly"},
		// The method, not the app name, marks reconnecting PTY connections,
		// so the name and its session count key stay as supplied.
		{"WebTerminalNameKept", envWith("Reconnecting-PTY"), "", "reconnecting_pty"},
		{"LastInstanceWins", append(envWith("vscode"), AppNameEnvironmentVariable+"=cursor"), "", "cursor"},
		// A variable that merely starts with the same characters is a
		// different variable, not an app name.
		{
			"LongerVariableIgnored",
			[]string{AppNameEnvironmentVariable + "_FOO=bar"},
			AppNameEnvironmentVariable + "_FOO=bar",
			"ssh",
		},
		{
			"LongerVariableDoesNotShadowAppName",
			append(envWith("cursor"), AppNameEnvironmentVariable+"_FOO=bar"),
			AppNameEnvironmentVariable + "_FOO=bar",
			"cursor",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			appName, _, filteredEnv := extractAppName(tc.env)
			require.Equal(t, tc.want, appName)
			for _, kv := range filteredEnv {
				require.NotContains(t, kv, AppNameEnvironmentVariable+"=")
			}
			if tc.keepEnv != "" {
				require.Contains(t, filteredEnv, tc.keepEnv)
			}
		})
	}
}

func TestSSHConnectionType(t *testing.T) {
	t.Parallel()

	for appName, want := range map[string]proto.Connection_Type{
		"ssh":              proto.Connection_SSH,
		"zed":              proto.Connection_SSH,
		"vscode":           proto.Connection_VSCODE,
		"cursor":           proto.Connection_VSCODE,
		"jetbrains":        proto.Connection_JETBRAINS,
		"some_new_ide":     proto.Connection_SSH,
		"reconnecting_pty": proto.Connection_SSH,
		"unknown":          proto.Connection_SSH,
	} {
		require.Equal(t, want, sshConnectionType(codersdk.AppNameFamily(appName)), appName)
	}
}

func TestAppNameMetricLabel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		appName string
		want    string
	}{
		{"SSH", "ssh", "ssh"},
		{"ForkUsesFamily", "cursor", "vscode"},
		// No alias for the nightly variant, so it does not get a label.
		{"UnlistedVariant", "cursor_nightly", "unknown"},
		{"Unknown", "some_new_ide", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, appNameMetricLabel(tc.appName))
		})
	}
}

func TestSessionCounts(t *testing.T) {
	t.Parallel()

	s := &Server{sessionCounts: make(map[string]int64)}

	// Counters are created on demand and names are normalized.
	_ = s.startSession("ssh")
	_ = s.startSession("vscode")
	endCursor1 := s.startSession("Cursor")
	endCursor2 := s.startSession("cursor")
	require.Equal(t, map[string]int64{
		"ssh":    1,
		"vscode": 1,
		"cursor": 2,
	}, s.SessionCounts())

	// Zero-count entries are dropped, not reported as idle apps.
	endCursor1()
	endCursor2()
	require.Equal(t, map[string]int64{
		"ssh":    1,
		"vscode": 1,
	}, s.SessionCounts())
}
