package version

import "fmt"

var CommitHash = "unknown"
var BuildTime = "1970-01-01T00:00:00Z"
var Version = "0.0.0"
var BinaryName = "slack-mcp-server"

// String returns the human-readable version banner, exactly three lines with
// no trailing newline:
//
//	<BinaryName> <Version>
//	commit: <CommitHash>
//	built:  <BuildTime>
//
// Scripts parse line 1, field 2 for the version, so line 1 must always split
// into exactly two space-separated fields.
func String() string {
	return fmt.Sprintf("%s %s\ncommit: %s\nbuilt:  %s", BinaryName, Version, CommitHash, BuildTime)
}
