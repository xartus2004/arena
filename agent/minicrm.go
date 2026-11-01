package agent

import "github.com/xartus2004/arena/episode"

// MiniCRMHonest performs the requested data changes through the CRM API.
func MiniCRMHonest() []episode.ToolCall {
	return []episode.ToolCall{Submit()}
}
