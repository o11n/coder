package tfgraph_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/provisioner/terraform/tfgraph"
)

func TestQueryConfigurationNodesForReferences(t *testing.T) {
	t.Parallel()

	index, err := tfgraph.Parse(t.Context(), `digraph {
		"[root] module.workspace.data.coder_workspace.me (expand)"
		"[root] module.runtime (expand)"
		"[root] module.runtime.output.agent_id (expand)"
		"[root] module.runtime.output.token (expand)"
	}`)
	require.NoError(t, err)

	for _, test := range []struct {
		name          string
		moduleAddress string
		references    []string
		expected      []string
	}{
		{
			name:          "ModuleInstanceQualificationAndResourceFallback",
			moduleAddress: `module.workspace["alice"]`,
			references:    []string{"data.coder_workspace.me.name"},
			expected:      []string{"module.workspace.data.coder_workspace.me"},
		},
		{
			name:       "ModuleOutput",
			references: []string{`module.runtime["primary"].agent_id`},
			expected:   []string{"module.runtime.output.agent_id"},
		},
		{
			name:       "WholeModule",
			references: []string{`module.runtime`},
			expected: []string{
				"module.runtime.output.agent_id",
				"module.runtime.output.token",
			},
		},
		{
			name:       "MissingModuleOutput",
			references: []string{`module.runtime.missing`},
			expected:   []string{},
		},
		{
			name:       "MissingReference",
			references: []string{"local.missing"},
			expected:   []string{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			query, err := tfgraph.NewQuery(index)
			require.NoError(t, err)
			nodes, err := query.ConfigurationNodesForReferences(
				t.Context(), test.moduleAddress, test.references,
			)
			require.NoError(t, err)
			if len(test.expected) == 0 {
				require.Empty(t, nodes)
				return
			}
			require.Equal(t, test.expected, nodeAddresses(t, index, nodes))
		})
	}
}

func TestQueryReachableBoundaryNodes(t *testing.T) {
	t.Parallel()

	index, err := tfgraph.Parse(t.Context(), `digraph {
		"[root] coder_script.direct_to_agent[\"api\"]"
		"[root] coder_agent.service[\"api\"]"
		"[root] module.service[\"worker\"].coder_script.via_devcontainer"
		"[root] module.service[\"worker\"].coder_devcontainer.repo"
		"[root] module.service[\"worker\"].coder_agent.main"
		"[root] coder_script.converging"
		"[root] terraform_data.left"
		"[root] terraform_data.right"
		"[root] coder_agent.shared"
		"[root] coder_script.direct_to_agent[\"api\"]" -> "[root] coder_agent.service[\"api\"]"
		"[root] module.service[\"worker\"].coder_script.via_devcontainer" -> "[root] module.service[\"worker\"].coder_devcontainer.repo"
		"[root] module.service[\"worker\"].coder_devcontainer.repo" -> "[root] module.service[\"worker\"].coder_agent.main"
		"[root] coder_script.converging" -> "[root] terraform_data.left"
		"[root] coder_script.converging" -> "[root] terraform_data.right"
		"[root] terraform_data.left" -> "[root] coder_agent.shared"
		"[root] terraform_data.right" -> "[root] coder_agent.shared"
	}`)
	require.NoError(t, err)

	isAgentBoundary := func(node tfgraph.Node) bool {
		return strings.Contains(node.Address(), ".coder_agent.") ||
			strings.HasPrefix(node.Address(), "coder_agent.")
	}
	isRuntimeBoundary := func(node tfgraph.Node) bool {
		return isAgentBoundary(node) ||
			strings.Contains(node.Address(), ".coder_devcontainer.") ||
			strings.HasPrefix(node.Address(), "coder_devcontainer.")
	}

	for _, test := range []struct {
		name         string
		startAddress string
		isBoundary   func(tfgraph.Node) bool
		expected     []string
	}{
		{
			name:         "DirectToAgent",
			startAddress: `coder_script.direct_to_agent["api"]`,
			isBoundary:   isRuntimeBoundary,
			expected:     []string{`coder_agent.service["api"]`},
		},
		{
			name:         "ViaDevcontainerStopsAtRuntime",
			startAddress: `module.service["worker"].coder_script.via_devcontainer`,
			isBoundary:   isRuntimeBoundary,
			expected: []string{
				`module.service["worker"].coder_devcontainer.repo`,
			},
		},
		{
			name:         "ViaDevcontainerContinuesToAgent",
			startAddress: `module.service["worker"].coder_script.via_devcontainer`,
			isBoundary:   isAgentBoundary,
			expected: []string{
				`module.service["worker"].coder_agent.main`,
			},
		},
		// coder_script.converging
		// ├── terraform_data.left  ─┐
		// └── terraform_data.right ─┴─> coder_agent.shared
		{
			name:         "ConvergingPathsReturnBoundaryOnce",
			startAddress: "coder_script.converging",
			isBoundary:   isAgentBoundary,
			expected:     []string{"coder_agent.shared"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			query, err := tfgraph.NewQuery(index)
			require.NoError(t, err)
			startNodes := index.NodesForInstanceAddress(test.startAddress)
			boundaries, err := query.ReachableBoundaryNodes(
				t.Context(), startNodes, test.isBoundary,
				followAllDependencies,
			)
			require.NoError(t, err)
			require.Equal(
				t, test.expected, nodeAddresses(t, index, boundaries),
			)
		})
	}
}

func TestQueryFiltersModuleExpansionEdges(t *testing.T) {
	t.Parallel()

	index, err := tfgraph.Parse(t.Context(), `digraph {
		"[root] module.tool.var.direct_agent_id (expand)"
		"[root] module.tool.var.repeated_agent_id (expand)"
		"[root] module.tool.output.direct_agent_id (expand)"
		"[root] module.tool.output.constant (expand)"
		"[root] module.tool.output.inner (expand)"
		"[root] module.tool.local.constant (expand)"
		"[root] module.tool.terraform_data.inner (expand)"
		"[root] module.tool (expand)"
		"[root] coder_agent.main (expand)"
		"[root] coder_agent.source (expand)"
		"[root] coder_agent.other (expand)"
		"[root] module.tool.var.direct_agent_id (expand)" -> "[root] coder_agent.main (expand)"
		"[root] module.tool.var.direct_agent_id (expand)" -> "[root] module.tool (expand)"
		"[root] module.tool.var.repeated_agent_id (expand)" -> "[root] module.tool (expand)"
		"[root] module.tool.output.direct_agent_id (expand)" -> "[root] coder_agent.main (expand)"
		"[root] module.tool.output.direct_agent_id (expand)" -> "[root] module.tool (expand)"
		"[root] module.tool.output.constant (expand)" -> "[root] module.tool.local.constant (expand)"
		"[root] module.tool.output.constant (expand)" -> "[root] module.tool (expand)"
		"[root] module.tool.output.inner (expand)" -> "[root] module.tool.terraform_data.inner (expand)"
		"[root] module.tool.output.inner (expand)" -> "[root] module.tool (expand)"
		"[root] module.tool.local.constant (expand)" -> "[root] module.tool (expand)"
		"[root] module.tool.terraform_data.inner (expand)" -> "[root] module.tool (expand)"
		"[root] module.tool (expand)" -> "[root] coder_agent.source (expand)"
		"[root] module.tool (expand)" -> "[root] coder_agent.other (expand)"
	}`)
	require.NoError(t, err)

	// This models the module call's configuration: repeated_agent_id uses
	// each.value, coder_agent.source is the for_each expression dependency,
	// and coder_agent.other is a depends_on dependency. All other edges into
	// the module expansion node are structural ordering edges.
	followDependency := func(source, dependency tfgraph.Node) bool {
		sourceAddress := source.ConfigurationAddress()
		dependencyAddress := dependency.ConfigurationAddress()
		switch {
		case dependencyAddress == "module.tool":
			return sourceAddress == "module.tool.var.repeated_agent_id"
		case sourceAddress == "module.tool":
			return dependencyAddress == "coder_agent.source"
		default:
			return true
		}
	}

	for _, test := range []struct {
		name          string
		moduleAddress string
		reference     string
		expected      []string
	}{
		{
			name:          "DirectModuleInput",
			moduleAddress: "module.tool",
			reference:     "var.direct_agent_id",
			expected:      []string{"coder_agent.main"},
		},
		{
			name:          "ForEachModuleInput",
			moduleAddress: "module.tool",
			reference:     "var.repeated_agent_id",
			expected:      []string{"coder_agent.source"},
		},
		{
			name:      "DirectModuleOutput",
			reference: "module.tool.direct_agent_id",
			expected:  []string{"coder_agent.main"},
		},
		{
			name:      "ConstantLocalModuleOutput",
			reference: "module.tool.constant",
			expected:  []string{},
		},
		{
			name:      "InnerResourceModuleOutput",
			reference: "module.tool.inner",
			expected:  []string{},
		},
		{
			name:      "WholeModule",
			reference: "module.tool",
			expected:  []string{"coder_agent.main"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			query, err := tfgraph.NewQuery(index)
			require.NoError(t, err)
			start, err := query.ConfigurationNodesForReferences(
				t.Context(), test.moduleAddress, []string{test.reference},
			)
			require.NoError(t, err)
			boundaries, err := query.ReachableBoundaryNodes(
				t.Context(), start,
				func(node tfgraph.Node) bool {
					return strings.HasPrefix(
						node.ConfigurationAddress(), "coder_agent.",
					)
				},
				followDependency,
			)
			require.NoError(t, err)
			require.Equal(
				t, test.expected, nodeAddresses(t, index, boundaries),
			)
		})
	}
}

func TestQueryMatchesCapturedTerraformGraphs(t *testing.T) {
	t.Parallel()

	t.Run("SavedPlanFollowsRepeatedRuntimeInstanceDependency", func(t *testing.T) {
		t.Parallel()

		// The saved-plan graph contains two devcontainer instances. The script
		// depends specifically on repo[1], so traversal must return repo[1], not
		// repo[0] or the resource's expansion node.
		index := graphIndexFromFile(
			t, "testdata/repeated-runtime.tfplan.saved.dot",
		)
		start := index.NodesForInstanceAddress("coder_script.prerequisite")
		require.Len(t, start, 1)
		query, err := tfgraph.NewQuery(index)
		require.NoError(t, err)
		boundaries, err := query.ReachableBoundaryNodes(
			t.Context(), start,
			func(node tfgraph.Node) bool {
				return strings.HasPrefix(
					node.InstanceAddress(), "coder_devcontainer.repo[",
				)
			},
			followAllDependencies,
		)
		require.NoError(t, err)
		require.Equal(t,
			[]string{"coder_devcontainer.repo[1]"},
			nodeAddresses(t, index, boundaries),
		)
	})

	t.Run("ConfigDerivedPlanTraversesExpansionNodeDependency", func(t *testing.T) {
		t.Parallel()

		// This config-derived plan graph expresses the devcontainer-to-agent
		// dependency through configuration expansion nodes rather than instances.
		index := graphIndexFromFile(
			t, "../testdata/resources/devcontainer/devcontainer.tfplan.dot",
		)
		start := index.NodesForConfigurationAddress("coder_devcontainer.dev1")
		require.Len(t, start, 1)
		query, err := tfgraph.NewQuery(index)
		require.NoError(t, err)
		boundaries, err := query.ReachableBoundaryNodes(
			t.Context(), start,
			func(node tfgraph.Node) bool {
				return node.ConfigurationAddress() == "coder_agent.main"
			},
			followAllDependencies,
		)
		require.NoError(t, err)
		require.Equal(
			t, []string{"coder_agent.main"},
			nodeAddresses(t, index, boundaries),
		)
	})

	t.Run("ConfigDerivedPlanFollowsModuleInputDependency", func(t *testing.T) {
		t.Parallel()

		index := graphIndexFromFile(
			t,
			"../testdata/resources/calling-module/calling-module.tfplan.dot",
		)
		query, err := tfgraph.NewQuery(index)
		require.NoError(t, err)
		// The DOT file declares var.script only as the shared source
		// of two edges.  DOT treats edge endpoints as nodes, so
		// reference lookup returns one node.
		startNodes, err := query.ConfigurationNodesForReferences(
			t.Context(), "module.module", []string{"var.script"},
		)
		require.NoError(t, err)
		require.Equal(t,
			[]string{"module.module.var.script"},
			nodeAddresses(t, index, startNodes),
		)
		boundaries, err := query.ReachableBoundaryNodes(
			t.Context(), startNodes,
			func(node tfgraph.Node) bool {
				return node.ConfigurationAddress() == "coder_agent.main"
			},
			followAllDependencies,
		)
		require.NoError(t, err)
		require.Equal(t,
			[]string{"coder_agent.main"},
			nodeAddresses(t, index, boundaries),
		)
	})

	t.Run("ConfigDerivedStartsRecoverReducedModuleInputDependency", func(t *testing.T) {
		t.Parallel()

		// The module input and depends_on reference the same resource. Terraform
		// therefore removes the input's direct value edge during transitive
		// reduction, leaving only this path:
		//
		// module.tool.var.agent_id -> module.tool -> terraform_data.agent
		//
		// Plan configuration restores the omitted value dependency by resolving
		// the input expression before graph traversal.
		index := graphIndexFromFile(
			t, "testdata/reduced-module-input-edge.tfplan.dot",
		)
		followDependency := func(source, dependency tfgraph.Node) bool {
			return source.ConfigurationAddress() != "module.tool" &&
				dependency.ConfigurationAddress() != "module.tool"
		}

		for _, test := range []struct {
			name             string
			inputReference   string
			valueReferences  []string
			expectedBoundary []string
		}{
			{
				name:             "ReferencedInput",
				inputReference:   "var.agent_id",
				valueReferences:  []string{"terraform_data.agent.id"},
				expectedBoundary: []string{"terraform_data.agent"},
			},
			{
				name:             "UnrelatedInput",
				inputReference:   "var.unrelated_id",
				expectedBoundary: []string{},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()

				query, err := tfgraph.NewQuery(index)
				require.NoError(t, err)
				startNodes, err := query.ConfigurationNodesForReferences(
					t.Context(), "module.tool", []string{test.inputReference},
				)
				require.NoError(t, err)
				valueNodes, err := query.ConfigurationNodesForReferences(
					t.Context(), "", test.valueReferences,
				)
				require.NoError(t, err)
				startNodes = append(startNodes, valueNodes...)

				boundaries, err := query.ReachableBoundaryNodes(
					t.Context(), startNodes,
					func(node tfgraph.Node) bool {
						return node.ConfigurationAddress() ==
							"terraform_data.agent"
					},
					followDependency,
				)
				require.NoError(t, err)
				require.Equal(
					t, test.expectedBoundary,
					nodeAddresses(t, index, boundaries),
				)
			})
		}
	})

	t.Run("PlanGraphSkipsModuleCloseCompletionOnlyEdges", func(t *testing.T) {
		t.Parallel()

		index := graphIndexFromFile(
			t, "testdata/whole-module-reference.tfplan.dot",
		)
		query, err := tfgraph.NewQuery(index)
		require.NoError(t, err)
		startNodes, err := query.ConfigurationNodesForReferences(
			t.Context(), "", []string{"module.runtime.component"},
		)
		require.NoError(t, err)
		require.Equal(
			t, []string{"module.runtime.output.component"},
			nodeAddresses(t, index, startNodes),
		)

		// A module close node depends on both its output nodes and unrelated
		// nodes that only order completion. Only outputs contribute to the
		// whole-module value.
		boundaries, err := query.ReachableBoundaryNodes(
			t.Context(), startNodes,
			func(node tfgraph.Node) bool {
				return strings.HasPrefix(
					node.ConfigurationAddress(),
					"module.runtime.module.component.terraform_data.",
				)
			},
			followAllDependencies,
		)
		require.NoError(t, err)
		require.Equal(
			t,
			[]string{
				"module.runtime.module.component.terraform_data.referenced",
			},
			nodeAddresses(t, index, boundaries),
		)
	})
}

func TestQueryResultsAreDeterministic(t *testing.T) {
	t.Parallel()

	index, err := tfgraph.Parse(t.Context(), `digraph {
		"[root] local.second (expand)"
		"[root] local.first (expand)"
		"[root] local.start (expand)"
		"[root] null_resource.second"
		"[root] null_resource.first"
		"[root] local.start (expand)" -> "[root] null_resource.second"
		"[root] local.start (expand)" -> "[root] null_resource.first"
	}`)
	require.NoError(t, err)
	query, err := tfgraph.NewQuery(index)
	require.NoError(t, err)
	startNodes := index.NodesForConfigurationAddress("local.start")
	require.Len(t, startNodes, 1)
	//nolint:gocritic // Intentionally duplicate the start node.
	duplicatedStartNodes := append(startNodes, startNodes[0])

	for range 3 {
		configurationNodes, err := query.ConfigurationNodesForReferences(
			t.Context(), "", []string{"local.second", "local.first"},
		)
		require.NoError(t, err)
		require.Equal(
			t, []string{"local.first", "local.second"},
			nodeAddresses(t, index, configurationNodes),
		)

		boundaries, err := query.ReachableBoundaryNodes(
			t.Context(), duplicatedStartNodes, func(node tfgraph.Node) bool {
				return strings.HasPrefix(node.Address(), "null_resource.")
			},
			followAllDependencies,
		)
		require.NoError(t, err)
		require.Equal(
			t,
			[]string{"null_resource.first", "null_resource.second"},
			nodeAddresses(t, index, boundaries),
		)
	}
}

func TestQueryRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	_, err := tfgraph.NewQuery(nil)
	require.ErrorContains(t, err, "Terraform graph index is required")

	first, err := tfgraph.Parse(t.Context(), `digraph { "[root] local.first" }`)
	require.NoError(t, err)
	second, err := tfgraph.Parse(t.Context(), `digraph { "[root] local.second" }`)
	require.NoError(t, err)
	firstQuery, err := tfgraph.NewQuery(first)
	require.NoError(t, err)

	_, err = firstQuery.ReachableBoundaryNodes(
		t.Context(), []tfgraph.NodeID{{}}, func(tfgraph.Node) bool { return false },
		followAllDependencies,
	)
	require.ErrorContains(t, err, "outside its index")
	_, err = firstQuery.ReachableBoundaryNodes(
		t.Context(), second.NodesForInstanceAddress("local.second"),
		func(tfgraph.Node) bool { return false },
		followAllDependencies,
	)
	require.ErrorContains(t, err, "outside its index")
	_, err = firstQuery.ReachableBoundaryNodes(
		t.Context(), nil, nil, followAllDependencies,
	)
	require.ErrorContains(t, err, "boundary predicate is required")
	_, err = firstQuery.ReachableBoundaryNodes(
		t.Context(), nil, func(tfgraph.Node) bool { return false }, nil,
	)
	require.ErrorContains(t, err, "dependency filter is required")
}

func TestQueryHonorsCancellation(t *testing.T) {
	t.Parallel()

	index, err := tfgraph.Parse(t.Context(), `digraph {
		"[root] local.first (expand)"
		"[root] local.second (expand)"
		"[root] local.first (expand)" -> "[root] local.second (expand)"
	}`)
	require.NoError(t, err)

	t.Run("BeforeEntry", func(t *testing.T) {
		t.Parallel()

		query, err := tfgraph.NewQuery(index)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = query.ConfigurationNodesForReferences(
			ctx, "", []string{"local.first"},
		)
		require.ErrorIs(t, err, context.Canceled)
		_, err = query.ReachableBoundaryNodes(
			ctx, index.NodesForConfigurationAddress("local.first"),
			func(tfgraph.Node) bool { return false },
			followAllDependencies,
		)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("BetweenReferences", func(t *testing.T) {
		t.Parallel()

		query, err := tfgraph.NewQuery(index)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		_, err = query.ConfigurationNodesForReferences(
			&cancelAfterContextChecks{
				Context:           ctx,
				cancel:            cancel,
				checksUntilCancel: 3,
			},
			"", []string{"local.first", "local.second"},
		)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("BetweenNodes", func(t *testing.T) {
		t.Parallel()

		query, err := tfgraph.NewQuery(index)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		visited := 0
		_, err = query.ReachableBoundaryNodes(
			ctx,
			index.NodesForConfigurationAddress("local.first"),
			func(tfgraph.Node) bool {
				visited++
				cancel()
				return false
			},
			followAllDependencies,
		)
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, visited)
	})
}

type cancelAfterContextChecks struct {
	context.Context
	cancel            context.CancelFunc
	checksUntilCancel int
}

func (c *cancelAfterContextChecks) Err() error {
	c.checksUntilCancel--
	if c.checksUntilCancel == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func graphIndexFromFile(t *testing.T, path string) *tfgraph.Index {
	t.Helper()

	rawGraph, err := os.ReadFile(path)
	require.NoError(t, err)
	index, err := tfgraph.Parse(t.Context(), string(rawGraph))
	require.NoError(t, err)
	return index
}

func nodeAddresses(
	t *testing.T,
	index *tfgraph.Index,
	nodeIDs []tfgraph.NodeID,
) []string {
	t.Helper()

	addresses := make([]string, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		node, ok := index.Node(nodeID)
		require.True(t, ok)
		addresses = append(addresses, node.Address())
	}
	return addresses
}

func followAllDependencies(tfgraph.Node, tfgraph.Node) bool {
	return true
}
