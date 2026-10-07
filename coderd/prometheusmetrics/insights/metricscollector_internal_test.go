package insights

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/codersdk"
)

func TestConvertTemplateInsights(t *testing.T) {
	t.Parallel()

	rows := convertTemplateInsights([]database.GetTemplateInsightsByTemplateRow{
		{
			TemplateID:                uuid.New(),
			SessionFamilyUsageSeconds: database.StringMapOfInt{"vscode": 300, "ssh": 60, "unknown": 120},
		},
	})
	require.Len(t, rows, 1)
	// The query already deduplicated each family's minutes, so the values
	// pass through unchanged.
	require.Equal(t, map[codersdk.AppFamilyName]int64{
		codersdk.AppFamilyVSCode:  300,
		codersdk.AppFamilySSH:     60,
		codersdk.AppFamilyUnknown: 120,
	}, rows[0].usageSecondsByFamily)
}

func TestRegistryFamilies(t *testing.T) {
	t.Parallel()

	appNames, appFamilies := registryFamilies()
	require.Len(t, appFamilies, len(appNames))
	registry := codersdk.SessionCountAppFamilies()
	require.Len(t, appNames, len(registry))
	for i, appName := range appNames {
		require.Equal(t, string(registry[appName]), appFamilies[i], "app %q", appName)
	}
}

// Every family the raw agent stats can report has a series, so no session
// usage goes missing from the metric. Any client may supply a registered
// name, sftp included.
func TestFamilySeriesCoverRawFamilies(t *testing.T) {
	t.Parallel()

	reported := map[codersdk.AppFamilyName]bool{codersdk.AppFamilyUnknown: true}
	for _, family := range codersdk.SessionCountAppFamilies() {
		reported[family] = true
	}
	covered := map[codersdk.AppFamilyName]bool{}
	for _, family := range familySeries {
		require.NotEmpty(t, family.DisplayName(), "family %q", family)
		covered[family] = true
	}
	require.Equal(t, reported, covered)
}
