package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestShouldAddTool_ReadOnly_EmptyEnabledTools(t *testing.T) {
	t.Run("all read-only tools registered with empty enabledTools", func(t *testing.T) {
		readOnlyTools := []string{
			ToolConversationsHistory,
			ToolConversationsReplies,
			ToolConversationsSearchMessages,
			ToolChannelsList,
			ToolUsersSearch,
		}
		for _, tool := range readOnlyTools {
			result := shouldAddTool(tool, []string{}, "")
			assert.True(t, result, "tool %s should be registered when enabledTools is empty", tool)
		}
	})

	t.Run("all read-only tools registered with nil enabledTools", func(t *testing.T) {
		result := shouldAddTool(ToolConversationsHistory, nil, "")
		assert.True(t, result, "tool should be registered when enabledTools is nil")
	})

	t.Run("unknown tools also registered with empty enabledTools", func(t *testing.T) {
		result := shouldAddTool("future_new_tool", []string{}, "")
		assert.True(t, result, "unknown tools should be registered when enabledTools is empty")
	})
}

func TestShouldAddTool_ReadOnly_ExplicitEnabledTools(t *testing.T) {
	tests := []struct {
		name         string
		toolName     string
		enabledTools []string
		expected     bool
	}{
		{
			name:         "tool in enabledTools list is registered",
			toolName:     ToolConversationsHistory,
			enabledTools: []string{ToolConversationsHistory, ToolChannelsList},
			expected:     true,
		},
		{
			name:         "tool not in enabledTools list is not registered",
			toolName:     ToolConversationsAddMessage,
			enabledTools: []string{ToolConversationsHistory, ToolChannelsList},
			expected:     false,
		},
		{
			name:         "read-only tool blocked when not in explicit list",
			toolName:     ToolConversationsHistory,
			enabledTools: []string{ToolChannelsList},
			expected:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldAddTool(tt.toolName, tt.enabledTools, "")
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestShouldAddTool_SingleToolEnabled(t *testing.T) {
	enabledTools := []string{ToolChannelsList}

	for _, tool := range ValidToolNames {
		result := shouldAddTool(tool, enabledTools, "")
		if tool == ToolChannelsList {
			assert.True(t, result, "channels_list should be registered")
		} else {
			assert.False(t, result, "%s should NOT be registered when only channels_list is enabled", tool)
		}
	}
}

func TestValidToolNames(t *testing.T) {
	t.Run("ValidToolNames contains all expected tools", func(t *testing.T) {
		expectedTools := map[string]bool{
			ToolConversationsHistory:        true,
			ToolConversationsReplies:        true,
			ToolConversationsAddMessage:     true,
			ToolReactionsAdd:                true,
			ToolReactionsRemove:             true,
			ToolAttachmentGetData:           true,
			ToolConversationsSearchMessages: true,
			ToolConversationsUnreads:        true,
			ToolConversationsMark:           true,
			ToolConversationsDraftMessage:   true,
			ToolConversationsLeave:          true,
			ToolConversationsJoin:           true,
			ToolChannelsList:                true,
			ToolChannelsMe:                  true,
			ToolChannelsMembers:             true,
			ToolUsergroupsList:              true,
			ToolUsergroupsMe:                true,
			ToolUsergroupsCreate:            true,
			ToolUsergroupsUpdate:            true,
			ToolUsergroupsUsersUpdate:       true,
			ToolUsersSearch:                 true,
			ToolSavedList:                   true,
			ToolSavedUpdate:                 true,
			ToolSavedClearCompleted:         true,
		}

		assert.Equal(t, len(expectedTools), len(ValidToolNames), "ValidToolNames should have %d tools", len(expectedTools))

		for _, tool := range ValidToolNames {
			assert.True(t, expectedTools[tool], "unexpected tool in ValidToolNames: %s", tool)
		}
	})

	t.Run("constants match their string values", func(t *testing.T) {
		assert.Equal(t, "conversations_history", ToolConversationsHistory)
		assert.Equal(t, "conversations_replies", ToolConversationsReplies)
		assert.Equal(t, "conversations_add_message", ToolConversationsAddMessage)
		assert.Equal(t, "reactions_add", ToolReactionsAdd)
		assert.Equal(t, "reactions_remove", ToolReactionsRemove)
		assert.Equal(t, "attachment_get_data", ToolAttachmentGetData)
		assert.Equal(t, "conversations_search_messages", ToolConversationsSearchMessages)
		assert.Equal(t, "conversations_unreads", ToolConversationsUnreads)
		assert.Equal(t, "conversations_mark", ToolConversationsMark)
		assert.Equal(t, "conversations_draft_message", ToolConversationsDraftMessage)
		assert.Equal(t, "conversations_leave", ToolConversationsLeave)
		assert.Equal(t, "conversations_join", ToolConversationsJoin)
		assert.Equal(t, "channels_list", ToolChannelsList)
		assert.Equal(t, "channels_me", ToolChannelsMe)
		assert.Equal(t, "channels_members", ToolChannelsMembers)
		assert.Equal(t, "usergroups_list", ToolUsergroupsList)
		assert.Equal(t, "usergroups_me", ToolUsergroupsMe)
		assert.Equal(t, "usergroups_create", ToolUsergroupsCreate)
		assert.Equal(t, "usergroups_update", ToolUsergroupsUpdate)
		assert.Equal(t, "usergroups_users_update", ToolUsergroupsUsersUpdate)
		assert.Equal(t, "users_search", ToolUsersSearch)
		assert.Equal(t, "saved_list", ToolSavedList)
		assert.Equal(t, "saved_update", ToolSavedUpdate)
		assert.Equal(t, "saved_clear_completed", ToolSavedClearCompleted)
	})
}

func TestValidateEnabledTools(t *testing.T) {
	t.Run("empty list is valid", func(t *testing.T) {
		err := ValidateEnabledTools([]string{})
		assert.NoError(t, err)
	})

	t.Run("nil list is valid", func(t *testing.T) {
		err := ValidateEnabledTools(nil)
		assert.NoError(t, err)
	})

	t.Run("all valid tool names pass", func(t *testing.T) {
		err := ValidateEnabledTools(ValidToolNames)
		assert.NoError(t, err)
	})

	t.Run("single valid tool passes", func(t *testing.T) {
		err := ValidateEnabledTools([]string{ToolChannelsList})
		assert.NoError(t, err)
	})

	t.Run("single invalid tool fails", func(t *testing.T) {
		err := ValidateEnabledTools([]string{"invalid_tool"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid_tool")
		assert.Contains(t, err.Error(), "Valid tools are:")
	})

	t.Run("multiple invalid tools listed in error", func(t *testing.T) {
		err := ValidateEnabledTools([]string{"foo", "bar"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "foo")
		assert.Contains(t, err.Error(), "bar")
	})

	t.Run("mix of valid and invalid tools fails", func(t *testing.T) {
		err := ValidateEnabledTools([]string{ToolChannelsList, "invalid_tool", ToolReactionsAdd})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid tool name(s): invalid_tool.")
	})

	t.Run("typo in tool name fails", func(t *testing.T) {
		err := ValidateEnabledTools([]string{"channel_list"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "channel_list")
	})
}

// Helper to set/unset env vars for tests
func setEnv(key, value string) func() {
	old := os.Getenv(key)
	os.Setenv(key, value)
	return func() {
		if old == "" {
			os.Unsetenv(key)
		} else {
			os.Setenv(key, old)
		}
	}
}

func TestShouldAddTool_WriteTool_AddMessage(t *testing.T) {
	t.Run("empty enabledTools and empty env var - not registered", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_ADD_MESSAGE_TOOL", "")
		defer cleanup()

		result := shouldAddTool(ToolConversationsAddMessage, []string{}, "SLACK_MCP_ADD_MESSAGE_TOOL")
		assert.False(t, result, "write tool should NOT be registered when both enabledTools is empty and env var is not set")
	})

	t.Run("empty enabledTools and env var set to true - registered", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_ADD_MESSAGE_TOOL", "true")
		defer cleanup()

		result := shouldAddTool(ToolConversationsAddMessage, []string{}, "SLACK_MCP_ADD_MESSAGE_TOOL")
		assert.True(t, result, "write tool should be registered when enabledTools is empty but env var is set")
	})

	t.Run("empty enabledTools and env var set to channel list - registered", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_ADD_MESSAGE_TOOL", "C123,C456")
		defer cleanup()

		result := shouldAddTool(ToolConversationsAddMessage, []string{}, "SLACK_MCP_ADD_MESSAGE_TOOL")
		assert.True(t, result, "write tool should be registered when enabledTools is empty but env var has channel list")
	})

	t.Run("explicit enabledTools includes tool and empty env var - registered", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_ADD_MESSAGE_TOOL", "")
		defer cleanup()

		result := shouldAddTool(ToolConversationsAddMessage, []string{ToolConversationsAddMessage}, "SLACK_MCP_ADD_MESSAGE_TOOL")
		assert.True(t, result, "write tool should be registered when explicitly in enabledTools even without env var")
	})

	t.Run("explicit enabledTools excludes tool - not registered even with env var", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_ADD_MESSAGE_TOOL", "true")
		defer cleanup()

		result := shouldAddTool(ToolConversationsAddMessage, []string{ToolConversationsHistory}, "SLACK_MCP_ADD_MESSAGE_TOOL")
		assert.False(t, result, "write tool should NOT be registered when not in explicit enabledTools list")
	})
}

func TestShouldAddTool_WriteTool_Reactions(t *testing.T) {
	t.Run("empty enabledTools and no env var - not registered", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_REACTION_TOOL", "")
		defer cleanup()

		result := shouldAddTool(ToolReactionsAdd, []string{}, "SLACK_MCP_REACTION_TOOL")
		assert.False(t, result, "reactions_add should NOT be registered when env var is not set")

		result = shouldAddTool(ToolReactionsRemove, []string{}, "SLACK_MCP_REACTION_TOOL")
		assert.False(t, result, "reactions_remove should NOT be registered when env var is not set")
	})

	t.Run("empty enabledTools and env var set - registered", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_REACTION_TOOL", "true")
		defer cleanup()

		result := shouldAddTool(ToolReactionsAdd, []string{}, "SLACK_MCP_REACTION_TOOL")
		assert.True(t, result, "reactions_add should be registered when env var is set")

		result = shouldAddTool(ToolReactionsRemove, []string{}, "SLACK_MCP_REACTION_TOOL")
		assert.True(t, result, "reactions_remove should be registered when env var is set")
	})

	t.Run("explicit enabledTools includes tool - registered without env var", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_REACTION_TOOL", "")
		defer cleanup()

		result := shouldAddTool(ToolReactionsAdd, []string{ToolReactionsAdd}, "SLACK_MCP_REACTION_TOOL")
		assert.True(t, result, "reactions_add should be registered when explicitly in enabledTools")
	})
}

func TestShouldAddTool_WriteTool_Attachment(t *testing.T) {
	t.Run("empty enabledTools and no env var - not registered", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_ATTACHMENT_TOOL", "")
		defer cleanup()

		result := shouldAddTool(ToolAttachmentGetData, []string{}, "SLACK_MCP_ATTACHMENT_TOOL")
		assert.False(t, result, "attachment_get_data should NOT be registered when env var is not set")
	})

	t.Run("empty enabledTools and env var set - registered", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_ATTACHMENT_TOOL", "true")
		defer cleanup()

		result := shouldAddTool(ToolAttachmentGetData, []string{}, "SLACK_MCP_ATTACHMENT_TOOL")
		assert.True(t, result, "attachment_get_data should be registered when env var is set")
	})

	t.Run("explicit enabledTools includes tool - registered without env var", func(t *testing.T) {
		cleanup := setEnv("SLACK_MCP_ATTACHMENT_TOOL", "")
		defer cleanup()

		result := shouldAddTool(ToolAttachmentGetData, []string{ToolAttachmentGetData}, "SLACK_MCP_ATTACHMENT_TOOL")
		assert.True(t, result, "attachment_get_data should be registered when explicitly in enabledTools")
	})
}

// setupMCPClientServer creates an MCP server with the given options and tool handler,
// wires up a client via stdio pipes, and returns the connected client.
func setupMCPClientServer(t *testing.T, opts []server.ServerOption, toolHandler server.ToolHandlerFunc) *client.Client {
	t.Helper()

	mcpSrv := server.NewMCPServer("test", "1.0.0", opts...)
	mcpSrv.AddTool(mcp.NewTool("test_tool",
		mcp.WithDescription("A test tool"),
	), toolHandler)

	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stdioSrv := server.NewStdioServer(mcpSrv)
	go func() {
		_ = stdioSrv.Listen(ctx, serverReader, serverWriter)
	}()

	var logBuf bytes.Buffer
	tr := transport.NewIO(clientReader, clientWriter, io.NopCloser(&logBuf))
	err := tr.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })

	c := client.NewClient(tr)

	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	_, err = c.Initialize(ctx, initReq)
	require.NoError(t, err)

	return c
}

func TestUnitErrorRecoveryMiddleware(t *testing.T) {
	logger := zap.NewNop()

	t.Run("handler error is converted to isError tool result", func(t *testing.T) {
		c := setupMCPClientServer(t,
			[]server.ServerOption{server.WithToolHandlerMiddleware(buildErrorRecoveryMiddleware(logger))},
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return nil, fmt.Errorf("simulated tool error: invalid channel ID")
			},
		)

		var callReq mcp.CallToolRequest
		callReq.Params.Name = "test_tool"
		result, err := c.CallTool(context.Background(), callReq)

		require.NoError(t, err, "should not return a JSON-RPC error")
		require.NotNil(t, result)
		assert.True(t, result.IsError, "result should have isError=true")
		require.Len(t, result.Content, 1)
		textContent, ok := result.Content[0].(mcp.TextContent)
		require.True(t, ok, "content should be TextContent")
		assert.Contains(t, textContent.Text, "simulated tool error: invalid channel ID")
	})

	t.Run("without middleware handler error becomes JSON-RPC error", func(t *testing.T) {
		c := setupMCPClientServer(t,
			nil, // no error recovery middleware
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return nil, fmt.Errorf("simulated tool error: invalid channel ID")
			},
		)

		var callReq mcp.CallToolRequest
		callReq.Params.Name = "test_tool"
		result, err := c.CallTool(context.Background(), callReq)

		assert.Error(t, err, "should return a JSON-RPC error without middleware")
		assert.Nil(t, result)
	})

	t.Run("successful tool call passes through unchanged", func(t *testing.T) {
		c := setupMCPClientServer(t,
			[]server.ServerOption{server.WithToolHandlerMiddleware(buildErrorRecoveryMiddleware(logger))},
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("all good"), nil
			},
		)

		var callReq mcp.CallToolRequest
		callReq.Params.Name = "test_tool"
		result, err := c.CallTool(context.Background(), callReq)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.False(t, result.IsError, "successful result should not have isError=true")
		require.Len(t, result.Content, 1)
		textContent, ok := result.Content[0].(mcp.TextContent)
		require.True(t, ok)
		assert.Equal(t, "all good", textContent.Text)
	})
}

func TestShouldAddTool_Matrix(t *testing.T) {
	// Test the complete matrix from the plan:
	// | ENABLED_TOOLS | TOOL_ENV_VAR | Result |
	// |---------------|--------------|--------|
	// | empty         | empty        | NOT registered |
	// | empty         | true/list    | Registered |
	// | includes tool | empty        | Registered |
	// | includes tool | list         | Registered |
	// | excludes tool | any          | NOT registered |

	tests := []struct {
		name         string
		enabledTools []string
		envVarValue  string
		expected     bool
	}{
		{
			name:         "empty ENABLED_TOOLS + empty env var = NOT registered",
			enabledTools: []string{},
			envVarValue:  "",
			expected:     false,
		},
		{
			name:         "empty ENABLED_TOOLS + env var=true = registered",
			enabledTools: []string{},
			envVarValue:  "true",
			expected:     true,
		},
		{
			name:         "empty ENABLED_TOOLS + env var=channel list = registered",
			enabledTools: []string{},
			envVarValue:  "C123,C456",
			expected:     true,
		},
		{
			name:         "includes tool + empty env var = registered",
			enabledTools: []string{ToolConversationsAddMessage},
			envVarValue:  "",
			expected:     true,
		},
		{
			name:         "includes tool + env var=list = registered",
			enabledTools: []string{ToolConversationsAddMessage},
			envVarValue:  "C123",
			expected:     true,
		},
		{
			name:         "excludes tool + empty env var = NOT registered",
			enabledTools: []string{ToolConversationsHistory},
			envVarValue:  "",
			expected:     false,
		},
		{
			name:         "excludes tool + env var=true = NOT registered",
			enabledTools: []string{ToolConversationsHistory},
			envVarValue:  "true",
			expected:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := setEnv("SLACK_MCP_ADD_MESSAGE_TOOL", tt.envVarValue)
			defer cleanup()

			result := shouldAddTool(ToolConversationsAddMessage, tt.enabledTools, "SLACK_MCP_ADD_MESSAGE_TOOL")
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestUnitShouldAddToolFalseyEnv is the core regression: before the fix, the
// env branch was `os.Getenv(envVarName) != ""`, so any non-empty value —
// including explicit "disabled" spellings like "false" or "0" — registered
// the tool. Falsey values must now be rejected for all five tool env vars,
// and the two boolean-only vars (SLACK_MCP_MARK_TOOL,
// SLACK_MCP_ATTACHMENT_TOOL) additionally require an explicit truthy value,
// so junk like "banana" never registers a tool whose handler gate would
// refuse every call. Channel-scoped vars keep treating any other non-empty
// value as a channel allowlist.
//
// @layer: unit
// @spec: falsey-env-registers-tool
// @regression
func TestUnitShouldAddToolFalseyEnv(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		envVar   string
		boolean  bool
		envSet   bool
		envValue string
		expected bool
	}{
		// Boolean-only: SLACK_MCP_MARK_TOOL.
		{name: "mark false", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "false", expected: false},
		{name: "mark 0", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "0", expected: false},
		{name: "mark no", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "no", expected: false},
		{name: "mark off", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "off", expected: false},
		{name: "mark uppercase FALSE", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "FALSE", expected: false},
		{name: "mark padded false", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: " false ", expected: false},
		{name: "mark whitespace only", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "   ", expected: false},
		{name: "mark junk value does not register", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "banana", expected: false},
		{name: "mark true", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "true", expected: true},
		{name: "mark 1", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "1", expected: true},
		{name: "mark yes", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: true, envValue: "yes", expected: true},
		{name: "mark unset", tool: ToolConversationsMark, envVar: "SLACK_MCP_MARK_TOOL", boolean: true, envSet: false, expected: false},
		// Boolean-only: SLACK_MCP_ATTACHMENT_TOOL.
		{name: "attachment false", tool: ToolAttachmentGetData, envVar: "SLACK_MCP_ATTACHMENT_TOOL", boolean: true, envSet: true, envValue: "false", expected: false},
		{name: "attachment junk value does not register", tool: ToolAttachmentGetData, envVar: "SLACK_MCP_ATTACHMENT_TOOL", boolean: true, envSet: true, envValue: "banana", expected: false},
		{name: "attachment true", tool: ToolAttachmentGetData, envVar: "SLACK_MCP_ATTACHMENT_TOOL", boolean: true, envSet: true, envValue: "true", expected: true},
		// Channel-scoped: SLACK_MCP_ADD_MESSAGE_TOOL.
		{name: "add-message false", tool: ToolConversationsAddMessage, envVar: "SLACK_MCP_ADD_MESSAGE_TOOL", envSet: true, envValue: "false", expected: false},
		{name: "add-message channel allowlist keeps registering", tool: ToolConversationsAddMessage, envVar: "SLACK_MCP_ADD_MESSAGE_TOOL", envSet: true, envValue: "C123,C456", expected: true},
		{name: "add-message negated channel allowlist keeps registering", tool: ToolConversationsAddMessage, envVar: "SLACK_MCP_ADD_MESSAGE_TOOL", envSet: true, envValue: "!C123", expected: true},
		// Channel-scoped: SLACK_MCP_DRAFT_MESSAGE_TOOL.
		{name: "draft off", tool: ToolConversationsDraftMessage, envVar: "SLACK_MCP_DRAFT_MESSAGE_TOOL", envSet: true, envValue: "off", expected: false},
		{name: "draft channel allowlist keeps registering", tool: ToolConversationsDraftMessage, envVar: "SLACK_MCP_DRAFT_MESSAGE_TOOL", envSet: true, envValue: "C123", expected: true},
		// Channel-scoped: SLACK_MCP_REACTION_TOOL.
		{name: "reaction 0", tool: ToolReactionsAdd, envVar: "SLACK_MCP_REACTION_TOOL", envSet: true, envValue: "0", expected: false},
		{name: "reaction channel allowlist keeps registering", tool: ToolReactionsAdd, envVar: "SLACK_MCP_REACTION_TOOL", envSet: true, envValue: "C123", expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSet {
				t.Setenv(tt.envVar, tt.envValue)
			} else {
				// t.Setenv registers the restore; Unsetenv then makes the
				// variable truly absent for the duration of the subtest.
				t.Setenv(tt.envVar, "")
				os.Unsetenv(tt.envVar)
			}

			decide := shouldAddTool
			if tt.boolean {
				decide = shouldAddBooleanTool
			}
			result := decide(tt.tool, []string{}, tt.envVar)
			assert.Equal(t, tt.expected, result, "value %q", tt.envValue)
		})
	}
}

// TestUnitShouldAddToolEnabledToolsPrecedenceOverFalseyEnv guards against an
// over-broad fix: the SLACK_MCP_ENABLED_TOOLS allowlist branch must still win
// outright, regardless of what the tool-specific env var says — for both the
// boolean-only and channel-scoped variants.
//
// @layer: unit
// @spec: falsey-env-registers-tool
// @regression
func TestUnitShouldAddToolEnabledToolsPrecedenceOverFalseyEnv(t *testing.T) {
	t.Run("tool in enabledTools wins even with falsey env var", func(t *testing.T) {
		t.Setenv("SLACK_MCP_MARK_TOOL", "false")

		result := shouldAddBooleanTool(ToolConversationsMark, []string{ToolConversationsMark}, "SLACK_MCP_MARK_TOOL")
		assert.True(t, result)
	})

	t.Run("tool in enabledTools wins even with junk env var", func(t *testing.T) {
		t.Setenv("SLACK_MCP_MARK_TOOL", "banana")

		result := shouldAddBooleanTool(ToolConversationsMark, []string{ToolConversationsMark}, "SLACK_MCP_MARK_TOOL")
		assert.True(t, result)
	})

	t.Run("tool in enabledTools wins even with unset env var", func(t *testing.T) {
		t.Setenv("SLACK_MCP_MARK_TOOL", "")
		os.Unsetenv("SLACK_MCP_MARK_TOOL")

		result := shouldAddBooleanTool(ToolConversationsMark, []string{ToolConversationsMark}, "SLACK_MCP_MARK_TOOL")
		assert.True(t, result)
	})

	t.Run("channel-scoped tool in enabledTools wins even with falsey env var", func(t *testing.T) {
		t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "false")

		result := shouldAddTool(ToolConversationsAddMessage, []string{ToolConversationsAddMessage}, "SLACK_MCP_ADD_MESSAGE_TOOL")
		assert.True(t, result)
	})
}
