// @layer: unit
// @spec: falsey-env-registers-tool
package handler

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newMarkHandlerFixture builds a ConversationsHandler backed by a real,
// network-free *provider.ApiProvider booted in "demo" mode, mirroring
// newMembersHandlerFixture / newAddMessageHandlerFixture. SkipCache marks the
// caches ready so IsReady() succeeds without a fetch.
//
// Unlike the add-message and draft handlers, ConversationsMarkHandler has no
// postMessages-style test seam for the Slack client: it calls
// ch.apiProvider.Slack().MarkConversationContext directly. In demo mode,
// ApiProvider.Slack() returns a non-nil SlackAPI wrapping a nil
// *MCPSlackClient (provider.New leaves the client pointer nil for the "demo"
// token), so any call that reaches MarkConversationContext panics with a nil
// pointer dereference before any network I/O happens. Tests use that panic —
// recovered via callMarkRecovered — as a hard, deterministic signal that the
// handler proceeded past the SLACK_MCP_MARK_TOOL gate to the write path,
// without ever performing a real Slack write.
func newMarkHandlerFixture(t *testing.T) *ConversationsHandler {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("SLACK_MCP_XOXP_TOKEN", "demo")
	t.Setenv("SLACK_MCP_XOXB_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXC_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXD_TOKEN", "")
	t.Setenv("SLACK_MCP_USERS_CACHE", filepath.Join(dir, "users_cache.json"))
	t.Setenv("SLACK_MCP_CHANNELS_CACHE", filepath.Join(dir, "channels_cache.json"))
	t.Setenv("SLACK_MCP_CHANNEL_MEMBERS_CACHE", filepath.Join(dir, "members_cache.json"))

	ap := provider.New("stdio", zap.NewNop())
	ap.SkipCache()

	return NewConversationsHandler(ap, zap.NewNop())
}

// callMarkRecovered calls ConversationsMarkHandler with a well-formed,
// raw-channel-ID request (no "#"/"@" prefix, so channel resolution never
// touches the network either) and recovers any panic, reporting whether one
// occurred. A recovered panic proves the call reached the Slack client seam
// (see newMarkHandlerFixture); no panic plus a returned error proves the gate
// short-circuited before that seam was ever touched.
func callMarkRecovered(t *testing.T, h *ConversationsHandler) (res *mcp.CallToolResult, err error, reachedSlackClient bool) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			reachedSlackClient = true
			t.Logf("recovered panic invoking ConversationsMarkHandler (proves the call reached the nil demo Slack client): %v", r)
		}
	}()

	var req mcp.CallToolRequest
	req.Params.Name = "conversations_mark"
	req.Params.Arguments = map[string]any{
		"channels": []map[string]any{
			{"channel_id": "C0DEST", "timestamp": "1234567890.123456"},
		},
	}
	res, err = h.ConversationsMarkHandler(context.Background(), req)
	return
}

// TestUnitConversationsMarkHandlerGateBlocksWhenDisabled is the
// security-relevant regression: ConversationsMarkHandler never had an env
// gate before the fix, so SLACK_MCP_MARK_TOOL=false (or any other falsey /
// unset config) registered the tool AND every call succeeded, performing a
// real write via MarkConversationContext. The gate must now refuse the call
// before ever reaching the Slack client.
//
// @regression
func TestUnitConversationsMarkHandlerGateBlocksWhenDisabled(t *testing.T) {
	cases := []struct {
		name        string
		markTool    string
		enabledTool string
		wantErrSub  string
	}{
		{
			name:       "SLACK_MCP_MARK_TOOL unset, SLACK_MCP_ENABLED_TOOLS empty",
			wantErrSub: "by default, the conversations_mark tool is disabled",
		},
		{
			name:       "SLACK_MCP_MARK_TOOL=false",
			markTool:   "false",
			wantErrSub: "SLACK_MCP_MARK_TOOL must be set to",
		},
		{
			name:       "SLACK_MCP_MARK_TOOL=banana",
			markTool:   "banana",
			wantErrSub: "SLACK_MCP_MARK_TOOL must be set to",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newMarkHandlerFixture(t)
			t.Setenv("SLACK_MCP_MARK_TOOL", tc.markTool)
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", tc.enabledTool)

			res, err, reachedSlackClient := callMarkRecovered(t, h)

			assert.False(t, reachedSlackClient,
				"a disabled mark tool must never reach the Slack client / perform a write")
			require.Error(t, err)
			assert.Nil(t, res)
			assert.Contains(t, err.Error(), tc.wantErrSub)
		})
	}
}

// TestUnitConversationsMarkHandlerGateAllowsWhenEnabled guards against an
// over-broad fix: the enabled paths (explicit "true", and the
// SLACK_MCP_ENABLED_TOOLS allowlist) must still pass the gate.
//
// @regression
func TestUnitConversationsMarkHandlerGateAllowsWhenEnabled(t *testing.T) {
	cases := []struct {
		name        string
		markTool    string
		enabledTool string
	}{
		{
			name:     "SLACK_MCP_MARK_TOOL=true",
			markTool: "true",
		},
		{
			name:        "SLACK_MCP_MARK_TOOL unset, SLACK_MCP_ENABLED_TOOLS contains conversations_mark",
			enabledTool: "conversations_history,conversations_mark",
		},
		{
			name:        "SLACK_MCP_ENABLED_TOOLS wins over falsey SLACK_MCP_MARK_TOOL",
			markTool:    "false",
			enabledTool: "conversations_mark",
		},
		{
			name:        "SLACK_MCP_ENABLED_TOOLS wins over junk SLACK_MCP_MARK_TOOL",
			markTool:    "banana",
			enabledTool: "conversations_mark",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newMarkHandlerFixture(t)
			t.Setenv("SLACK_MCP_MARK_TOOL", tc.markTool)
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", tc.enabledTool)

			_, err, reachedSlackClient := callMarkRecovered(t, h)

			assert.True(t, reachedSlackClient,
				"an enabled mark tool must pass the gate and reach the Slack client call")
			if err != nil {
				assert.NotContains(t, err.Error(), "by default, the conversations_mark tool is disabled")
				assert.NotContains(t, err.Error(), "SLACK_MCP_MARK_TOOL must be set to")
			}
		})
	}
}

// TestUnitFilesGetGateEnabledToolsPrecedence pins the sibling gate in
// parseParamsToolFilesGet to the same rule as the mark gate: a tool listed in
// SLACK_MCP_ENABLED_TOOLS is callable regardless of its own env var. The
// parse function reads only env and request params, so no Slack client seam
// is needed.
//
// @regression
func TestUnitFilesGetGateEnabledToolsPrecedence(t *testing.T) {
	cases := []struct {
		name           string
		attachmentTool string
	}{
		{name: "SLACK_MCP_ENABLED_TOOLS wins over falsey SLACK_MCP_ATTACHMENT_TOOL", attachmentTool: "false"},
		{name: "SLACK_MCP_ENABLED_TOOLS wins over junk SLACK_MCP_ATTACHMENT_TOOL", attachmentTool: "banana"},
		{name: "SLACK_MCP_ENABLED_TOOLS with unset SLACK_MCP_ATTACHMENT_TOOL", attachmentTool: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newMarkHandlerFixture(t)
			t.Setenv("SLACK_MCP_ATTACHMENT_TOOL", tc.attachmentTool)
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "attachment_get_data")

			var req mcp.CallToolRequest
			req.Params.Name = "attachment_get_data"
			req.Params.Arguments = map[string]any{"file_id": "F123"}

			params, err := h.parseParamsToolFilesGet(req)
			require.NoError(t, err)
			assert.Equal(t, "F123", params.fileID)
		})
	}
}
