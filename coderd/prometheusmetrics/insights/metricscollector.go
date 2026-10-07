package insights

import (
	"context"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/pproflabel"
	"github.com/coder/coder/v2/coderd/util/slice"
	"github.com/coder/coder/v2/codersdk"
)

var (
	templatesActiveUsersDesc     = prometheus.NewDesc("coderd_insights_templates_active_users", "The number of active users of the template.", []string{"template_name", "organization_name"}, nil)
	applicationsUsageSecondsDesc = prometheus.NewDesc("coderd_insights_applications_usage_seconds", "The application usage per template.", []string{"template_name", "application_name", "slug", "organization_name"}, nil)
	parametersDesc               = prometheus.NewDesc("coderd_insights_parameters", "The parameter usage per template.", []string{"template_name", "parameter_name", "parameter_type", "parameter_value", "organization_name"}, nil)
)

type MetricsCollector struct {
	database     database.Store
	logger       slog.Logger
	timeWindow   time.Duration
	tickInterval time.Duration

	data atomic.Pointer[insightsData]
}

type insightsData struct {
	templates []templateInsightsRow
	apps      []database.GetTemplateAppInsightsByTemplateRow
	params    []parameterRow

	templateNames     map[uuid.UUID]string
	organizationNames map[uuid.UUID]string // template ID → org name
}

// templateInsightsRow is a GetTemplateInsightsByTemplateRow with its session
// usage decoded and grouped into families.
type templateInsightsRow struct {
	templateID           uuid.UUID
	activeUsers          int64
	usageSecondsByFamily map[codersdk.AppFamilyName]int64
}

type parameterRow struct {
	templateID uuid.UUID
	name       string
	aType      string
	value      string

	count int64
}

var _ prometheus.Collector = new(MetricsCollector)

func NewMetricsCollector(db database.Store, logger slog.Logger, timeWindow time.Duration, tickInterval time.Duration) (*MetricsCollector, error) {
	// The session family series read raw agent stats, which dbpurge keeps
	// until a day behind the latest template usage rollup, so a window longer
	// than about a day would undercount them.
	if timeWindow == 0 {
		timeWindow = 5 * time.Minute
	}
	if timeWindow < 5*time.Minute {
		return nil, xerrors.Errorf("time window must be at least 5 mins")
	}
	if tickInterval == 0 {
		tickInterval = timeWindow
	}

	return &MetricsCollector{
		database:     db,
		logger:       logger.Named("insights_metrics_collector"),
		timeWindow:   timeWindow,
		tickInterval: tickInterval,
	}, nil
}

func (mc *MetricsCollector) Run(ctx context.Context) (func(), error) {
	ctx, closeFunc := context.WithCancel(ctx)
	done := make(chan struct{})

	// Use time.Nanosecond to force an initial tick. It will be reset to the
	// correct duration after executing once.
	ticker := time.NewTicker(time.Nanosecond)
	doTick := func() {
		defer ticker.Reset(mc.tickInterval)

		now := time.Now()
		startTime := now.Add(-mc.timeWindow)
		endTime := now

		// Phase 1: Fetch insights from database
		// FIXME errorGroup will be used to fetch insights for apps and parameters
		eg, egCtx := errgroup.WithContext(ctx)
		eg.SetLimit(3)

		var templateInsights []templateInsightsRow
		var appInsights []database.GetTemplateAppInsightsByTemplateRow
		var paramInsights []parameterRow

		eg.Go(func() error {
			appNames, appFamilies := registryFamilies()
			rows, err := mc.database.GetTemplateInsightsByTemplate(egCtx, database.GetTemplateInsightsByTemplateParams{
				AppNames:    appNames,
				AppFamilies: appFamilies,
				StartTime:   startTime,
				EndTime:     endTime,
			})
			if err != nil {
				mc.logger.Error(ctx, "unable to fetch template insights from database", slog.Error(err))
				return err
			}
			templateInsights = convertTemplateInsights(rows)
			return nil
		})
		eg.Go(func() error {
			var err error
			appInsights, err = mc.database.GetTemplateAppInsightsByTemplate(egCtx, database.GetTemplateAppInsightsByTemplateParams{
				StartTime: startTime,
				EndTime:   endTime,
			})
			if err != nil {
				mc.logger.Error(ctx, "unable to fetch application insights from database", slog.Error(err))
			}
			return err
		})
		eg.Go(func() error {
			var err error
			rows, err := mc.database.GetTemplateParameterInsights(egCtx, database.GetTemplateParameterInsightsParams{
				StartTime: startTime,
				EndTime:   endTime,
			})
			if err != nil {
				mc.logger.Error(ctx, "unable to fetch parameter insights from database", slog.Error(err))
			}
			paramInsights = convertParameterInsights(rows)
			return err
		})
		err := eg.Wait()
		if err != nil {
			return
		}

		// Phase 2: Collect template IDs, and fetch relevant details
		templateIDs := uniqueTemplateIDs(templateInsights, appInsights, paramInsights)

		templateNames := make(map[uuid.UUID]string, len(templateIDs))
		organizationNames := make(map[uuid.UUID]string, len(templateIDs))
		if len(templateIDs) > 0 {
			templates, err := mc.database.GetTemplatesWithFilter(ctx, database.GetTemplatesWithFilterParams{
				IDs: templateIDs,
			})
			if err != nil {
				mc.logger.Error(ctx, "unable to fetch template details from database", slog.Error(err))
				return
			}
			templateNames = onlyTemplateNames(templates)

			// Build org name lookup so that metrics can
			// distinguish templates with the same name across
			// different organizations.
			orgIDs := make([]uuid.UUID, 0, len(templates))
			for _, t := range templates {
				orgIDs = append(orgIDs, t.OrganizationID)
			}
			orgIDs = slice.Unique(orgIDs)

			orgs, err := mc.database.GetOrganizations(ctx, database.GetOrganizationsParams{
				IDs: orgIDs,
			})
			if err != nil {
				mc.logger.Error(ctx, "unable to fetch organizations from database", slog.Error(err))
				return
			}
			orgNameByID := make(map[uuid.UUID]string, len(orgs))
			for _, o := range orgs {
				orgNameByID[o.ID] = o.Name
			}
			organizationNames = make(map[uuid.UUID]string, len(templates))
			for _, t := range templates {
				organizationNames[t.ID] = orgNameByID[t.OrganizationID]
			}
		}

		// Refresh the collector state
		mc.data.Store(&insightsData{
			templates: templateInsights,
			apps:      appInsights,
			params:    paramInsights,

			templateNames:     templateNames,
			organizationNames: organizationNames,
		})
	}

	pproflabel.Go(ctx, pproflabel.Service(pproflabel.ServiceMetricCollector), func(ctx context.Context) {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ticker.Stop()
				doTick()
			}
		}
	})
	return func() {
		closeFunc()
		<-done
	}, nil
}

func (*MetricsCollector) Describe(descCh chan<- *prometheus.Desc) {
	descCh <- templatesActiveUsersDesc
	descCh <- applicationsUsageSecondsDesc
	descCh <- parametersDesc
}

func (mc *MetricsCollector) Collect(metricsCh chan<- prometheus.Metric) {
	// Phase 3: Collect metrics

	data := mc.data.Load()
	if data == nil {
		return // insights data not loaded yet
	}

	// Custom apps
	for _, appRow := range data.apps {
		metricsCh <- prometheus.MustNewConstMetric(applicationsUsageSecondsDesc, prometheus.GaugeValue, float64(appRow.UsageSeconds), data.templateNames[appRow.TemplateID],
			appRow.DisplayName, appRow.SlugOrPort, data.organizationNames[appRow.TemplateID])
	}

	// Built-in apps, one series per session family.
	for _, templateRow := range data.templates {
		orgName := data.organizationNames[templateRow.templateID]
		for _, series := range familySeries {
			metricsCh <- prometheus.MustNewConstMetric(applicationsUsageSecondsDesc, prometheus.GaugeValue,
				float64(templateRow.usageSecondsByFamily[series.family]),
				data.templateNames[templateRow.templateID],
				series.applicationName,
				"", orgName)
		}
	}

	// Templates
	for _, templateRow := range data.templates {
		metricsCh <- prometheus.MustNewConstMetric(templatesActiveUsersDesc, prometheus.GaugeValue, float64(templateRow.activeUsers), data.templateNames[templateRow.templateID], data.organizationNames[templateRow.templateID])
	}

	// Parameters
	for _, parameterRow := range data.params {
		metricsCh <- prometheus.MustNewConstMetric(parametersDesc, prometheus.GaugeValue, float64(parameterRow.count), data.templateNames[parameterRow.templateID], parameterRow.name, parameterRow.aType, parameterRow.value, data.organizationNames[parameterRow.templateID])
	}
}

// Helper functions below.

func uniqueTemplateIDs(templateInsights []templateInsightsRow, appInsights []database.GetTemplateAppInsightsByTemplateRow, paramInsights []parameterRow) []uuid.UUID {
	tids := map[uuid.UUID]bool{}
	for _, t := range templateInsights {
		tids[t.templateID] = true
	}
	for _, t := range appInsights {
		tids[t.TemplateID] = true
	}
	for _, t := range paramInsights {
		tids[t.templateID] = true
	}

	uniqueUUIDs := make([]uuid.UUID, len(tids))
	var i int
	for t := range tids {
		uniqueUUIDs[i] = t
		i++
	}
	return uniqueUUIDs
}

func onlyTemplateNames(templates []database.Template) map[uuid.UUID]string {
	m := map[uuid.UUID]string{}
	for _, t := range templates {
		m[t.ID] = t.Name
	}
	return m
}

// familySeries lists the session families the application usage metric
// reports, so the series stay bounded however many app names agents report.
// Template apps have their own series.
var familySeries = []struct {
	family          codersdk.AppFamilyName
	applicationName string
}{
	{codersdk.AppFamilyVSCode, codersdk.TemplateBuiltinAppDisplayNameVSCode},
	{codersdk.AppFamilyJetBrains, codersdk.TemplateBuiltinAppDisplayNameJetBrains},
	{codersdk.AppFamilyReconnectingPTY, codersdk.TemplateBuiltinAppDisplayNameWebTerminal},
	{codersdk.AppFamilySSH, codersdk.TemplateBuiltinAppDisplayNameSSH},
	// No Coder client reports sftp, but any client may supply the name.
	{codersdk.AppFamilySFTP, codersdk.TemplateBuiltinAppDisplayNameSFTP},
	{codersdk.AppFamilyPortForwarding, codersdk.TemplateBuiltinAppDisplayNamePortForwarding},
	{codersdk.AppFamilyUnknown, codersdk.TemplateBuiltinAppDisplayNameUnknown},
}

// registryFamilies returns the registry's app-to-family mapping as the
// parallel arrays GetTemplateInsightsByTemplate takes.
func registryFamilies() (appNames, appFamilies []string) {
	registry := codersdk.SessionCountAppFamilies()
	appNames = slices.Sorted(maps.Keys(registry))
	appFamilies = make([]string, 0, len(appNames))
	for _, appName := range appNames {
		appFamilies = append(appFamilies, string(registry[appName]))
	}
	return appNames, appFamilies
}

// convertTemplateInsights decodes each row's per-family session usage.
func convertTemplateInsights(rows []database.GetTemplateInsightsByTemplateRow) []templateInsightsRow {
	converted := make([]templateInsightsRow, 0, len(rows))
	for _, row := range rows {
		usageSecondsByFamily := make(map[codersdk.AppFamilyName]int64, len(row.SessionFamilyUsageSeconds))
		for family, seconds := range row.SessionFamilyUsageSeconds {
			usageSecondsByFamily[codersdk.AppFamilyName(family)] = seconds
		}
		converted = append(converted, templateInsightsRow{
			templateID:           row.TemplateID,
			activeUsers:          row.ActiveUsers,
			usageSecondsByFamily: usageSecondsByFamily,
		})
	}
	return converted
}

func convertParameterInsights(rows []database.GetTemplateParameterInsightsRow) []parameterRow {
	type uniqueKey struct {
		templateID     uuid.UUID
		parameterName  string
		parameterType  string
		parameterValue string
	}

	m := map[uniqueKey]int64{}
	for _, r := range rows {
		for _, t := range r.TemplateIDs {
			key := uniqueKey{
				templateID:     t,
				parameterName:  r.Name,
				parameterType:  r.Type,
				parameterValue: r.Value,
			}

			if _, ok := m[key]; !ok {
				m[key] = 0
			}
			m[key] += r.Count
		}
	}

	converted := make([]parameterRow, len(m))
	var i int
	for k, c := range m {
		converted[i] = parameterRow{
			templateID: k.templateID,
			name:       k.parameterName,
			aType:      k.parameterType,
			value:      k.parameterValue,
			count:      c,
		}
		i++
	}

	slices.SortFunc(converted, func(a, b parameterRow) int {
		if a.templateID != b.templateID {
			return slice.Ascending(a.templateID.String(), b.templateID.String())
		}
		if a.name != b.name {
			return slice.Ascending(a.name, b.name)
		}
		return slice.Ascending(a.value, b.value)
	})

	return converted
}
