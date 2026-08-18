package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUnitResolveEnabledTools pins the flag/env resolution for the enabled
// tools allowlist: the --enabled-tools flag wins over SLACK_MCP_ENABLED_TOOLS,
// and the resolved value is exported back to the environment so handler-side
// gates that read SLACK_MCP_ENABLED_TOOLS see the same allowlist regardless
// of which mechanism the operator used. Before this export, enabling a tool
// via the flag alone registered it but left every call refused by its
// handler gate.
//
// @layer: unit
// @spec: falsey-env-registers-tool
// @regression
func TestUnitResolveEnabledTools(t *testing.T) {
	cases := []struct {
		name      string
		flagValue string
		envSet    bool
		envValue  string
		wantTools []string
		wantEnv   string
	}{
		{
			name:      "flag only is parsed and exported to env",
			flagValue: "conversations_mark",
			wantTools: []string{"conversations_mark"},
			wantEnv:   "conversations_mark",
		},
		{
			name:      "env var is the fallback when flag is empty",
			envSet:    true,
			envValue:  "conversations_mark,reactions_add",
			wantTools: []string{"conversations_mark", "reactions_add"},
			wantEnv:   "conversations_mark,reactions_add",
		},
		{
			name:      "flag wins over env var and overwrites it",
			flagValue: "conversations_mark",
			envSet:    true,
			envValue:  "reactions_add",
			wantTools: []string{"conversations_mark"},
			wantEnv:   "conversations_mark",
		},
		{
			name:      "both empty resolves to no allowlist",
			wantTools: nil,
			wantEnv:   "",
		},
		{
			name:      "whitespace and empty items are dropped from the parsed list",
			flagValue: " conversations_mark , ,reactions_add ",
			wantTools: []string{"conversations_mark", "reactions_add"},
			wantEnv:   " conversations_mark , ,reactions_add ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv registers the restore; Unsetenv then makes the
			// variable truly absent when the case needs it unset.
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")
			if tc.envSet {
				t.Setenv("SLACK_MCP_ENABLED_TOOLS", tc.envValue)
			} else {
				os.Unsetenv("SLACK_MCP_ENABLED_TOOLS")
			}

			got := resolveEnabledTools(tc.flagValue)

			assert.Equal(t, tc.wantTools, got)
			assert.Equal(t, tc.wantEnv, os.Getenv("SLACK_MCP_ENABLED_TOOLS"),
				"resolved allowlist must be exported back to SLACK_MCP_ENABLED_TOOLS")
		})
	}
}
