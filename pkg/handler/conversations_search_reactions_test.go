package handler

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestUnitSplitQueryReactionFilters(t *testing.T) {
	freeText, filters := splitQuery("hasmy::pushpin: roadmap has::eyes:")

	assert.Equal(t, []string{"roadmap"}, freeText)
	assert.Equal(t, []string{":pushpin:"}, filters["hasmy"])
	assert.Equal(t, []string{":eyes:"}, filters["has"])
}

func TestUnitBuildQueryReactionFilters(t *testing.T) {
	filters := map[string][]string{
		"has":   {":eyes:"},
		"hasmy": {":pushpin:"},
	}

	query := buildQuery([]string{"roadmap"}, filters)

	assert.Equal(t, "roadmap has::eyes: hasmy::pushpin:", query)
}

func TestUnitNormalizeEmojiFilter(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pushpin", ":pushpin:"},
		{":pushpin:", ":pushpin:"},
		{" eyes ", ":eyes:"},
		{"white_check_mark", ":white_check_mark:"},
		{"+1", ":+1:"},
		{"📌", "📌"}, // unicode emoji passes through unchanged
	}
	for _, c := range cases {
		assert.Equal(t, c.want, normalizeEmojiFilter(c.in), "input %q", c.in)
	}
}

func TestUnitParseParamsToolSearchReactionFilters(t *testing.T) {
	ch := &ConversationsHandler{logger: zap.NewNop()}

	req := mcp.CallToolRequest{}
	req.Params.Name = "conversations_search_messages"
	req.Params.Arguments = map[string]any{
		"search_query":        "roadmap",
		"filter_has_reaction": "eyes",
		"filter_my_reaction":  ":pushpin:",
	}

	params, err := ch.parseParamsToolSearch(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, "roadmap has::eyes: hasmy::pushpin:", params.query)
}
