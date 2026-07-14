package version

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnitVersionString(t *testing.T) {
	// Defaults from the stamped vars; overridable via -ldflags at build time.
	out := String()

	assert.Equal(t, "slack-mcp-server 0.0.0\ncommit: unknown\nbuilt:  1970-01-01T00:00:00Z", out)
	assert.False(t, strings.HasSuffix(out, "\n"), "no trailing newline")

	lines := strings.Split(out, "\n")
	require.Len(t, lines, 3, "exactly three lines")

	// Scripts parse line 1, field 2: it must split into exactly two fields.
	fields := strings.Fields(lines[0])
	require.Len(t, fields, 2, "line 1 must have exactly two fields")
	assert.Equal(t, BinaryName, fields[0])
	assert.Equal(t, Version, fields[1])

	assert.Equal(t, "commit: "+CommitHash, lines[1])
	assert.Equal(t, "built:  "+BuildTime, lines[2])
}
