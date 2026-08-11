package upstream

import (
	"context"
	"fmt"
	"net/url"
	"path"
)

type MCPToolSkeleton struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type FilteredEdgeTools map[string][]MCPToolSkeleton

type InstalledMCP struct {
	ServerID string            `json:"serverId"`
	Tools    []MCPToolSkeleton `json:"tools"`
	Status   string            `json:"status"`
}

type Edge struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Status        string                  `json:"status"`
	InstalledMCPs map[string]InstalledMCP `json:"installedMCPs"`
}

// FirstOnlineEdge returns the first Edge whose current status is ONLINE.
func (c *Client) FirstOnlineEdge(ctx context.Context) (string, error) {
	var edges []Edge
	if err := c.do(ctx, "GET", "/edges", nil, &edges); err != nil {
		return "", err
	}
	for _, edge := range edges {
		if edge.Status == "ONLINE" {
			return edge.ID, nil
		}
	}
	return "", fmt.Errorf("account has no online Edge")
}

// EdgeTools returns the Edge MCP tool skeletons accepted by allowTools.
// Empty allowTools means all discovered tools.
func (c *Client) EdgeTools(ctx context.Context, edgeID string, allowTools []string) (FilteredEdgeTools, error) {
	var edge Edge
	if err := c.do(ctx, "GET", fmt.Sprintf("/edges/%s", url.PathEscape(edgeID)), nil, &edge); err != nil {
		return nil, err
	}

	filtered := FilteredEdgeTools{}
	for key, installed := range edge.InstalledMCPs {
		serverID := installed.ServerID
		if serverID == "" {
			serverID = key
		}
		for _, tool := range installed.Tools {
			if edgeToolAllowed(tool.Name, allowTools) {
				filtered[serverID] = append(filtered[serverID], tool)
			}
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	return filtered, nil
}

func edgeToolAllowed(name string, allowTools []string) bool {
	if len(allowTools) == 0 {
		return true
	}
	for _, pattern := range allowTools {
		matched, err := path.Match(pattern, name)
		if err == nil && matched {
			return true
		}
	}
	return false
}
