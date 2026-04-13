package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// ConfluenceConfig holds the configuration for the Confluence client.
type ConfluenceConfig struct {
	BaseURL string
	Token   string
}

const (
	// defaultLimit is the default number of results for paginated requests.
	defaultLimit = 25
	// maxRetries is the maximum number of retries for rate-limited requests.
	maxRetries = 3
)

// loadConfig loads configuration from environment variables.
func loadConfig() (*ConfluenceConfig, error) {
	token := os.Getenv("CONFLUENCE_API_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("CONFLUENCE_API_TOKEN environment variable is required")
	}

	rawURL := os.Getenv("CONFLUENCE_BASE_URL")
	if rawURL == "" {
		rawURL = os.Getenv("CONFLUENCE_API_BASE_PATH")
	}
	if rawURL == "" {
		rawURL = os.Getenv("CONFLUENCE_HOST")
	}

	if rawURL == "" {
		return nil, fmt.Errorf("CONFLUENCE_BASE_URL (or CONFLUENCE_HOST) environment variable is required")
	}

	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	if !strings.HasPrefix(u.Scheme, "http") {
		return nil, fmt.Errorf("base URL must use http or https scheme")
	}

	if !strings.Contains(u.Path, "/rest/api") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/rest/api"
	}

	return &ConfluenceConfig{
		BaseURL: u.String(),
		Token:   token,
	}, nil
}

// ConfluenceClient is a client for the Confluence API.
type ConfluenceClient struct {
	config     *ConfluenceConfig
	httpClient *http.Client
}

// NewConfluenceClient creates a new instance of ConfluenceClient with a default timeout.
func NewConfluenceClient(config *ConfluenceConfig) *ConfluenceClient {
	return &ConfluenceClient{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
			},
		},
	}
}

// executeRequest performs an authenticated HTTP request and returns the response.
// The caller is responsible for closing the response body.
func (c *ConfluenceClient) executeRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	u, err := url.Parse(c.config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	u = u.JoinPath(path)

	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		jsonBytes, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.config.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

// doRequest performs an authenticated HTTP request and returns the body as bytes.
// It handles basic error checking, limits the response size, and implements retry-after rate limit handling.
func (c *ConfluenceClient) doRequest(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := c.executeRequest(ctx, method, path, query, body)
		if err != nil {
			return nil, err
		}

		respBytes, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}

		if handleRateLimit(attempt, resp) {
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("rate limited after %d retries", maxRetries)
		}

		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBytes))
		}

		return respBytes, nil
	}

	return nil, fmt.Errorf("unexpected error: exhausted retries")
}

// getJSON is a helper to perform a GET request and unmarshal the result into a target object efficiently.
// It implements retry-after rate limit handling.
func (c *ConfluenceClient) getJSON(ctx context.Context, path string, query url.Values, target any) error {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := c.executeRequest(ctx, "GET", path, query, nil)
		if err != nil {
			return err
		}

		if handleRateLimit(attempt, resp) {
			_ = resp.Body.Close()
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()
			return fmt.Errorf("rate limited after %d retries", maxRetries)
		}

		if resp.StatusCode >= 400 {
			respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBytes))
		}

		err = json.NewDecoder(resp.Body).Decode(target)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("failed to decode JSON: %w", err)
		}
		return nil
	}

	return fmt.Errorf("unexpected error: exhausted retries")
}

// parseRetryAfter extracts the retry-after header value from the response in seconds.
func parseRetryAfter(resp *http.Response) int {
	header := resp.Header.Get("retry-after")
	if header == "" {
		return 0
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil {
		return 0
	}
	return seconds
}

// handleRateLimit handles HTTP 429 responses by sleeping and returning whether to retry.
func handleRateLimit(attempt int, resp *http.Response) bool {
	if resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if attempt < maxRetries {
		retryAfter := parseRetryAfter(resp)
		if retryAfter > 0 {
			time.Sleep(time.Duration(retryAfter) * time.Second)
		}
		return true
	}
	return false
}

// SpaceRef represents a reference to a Confluence space in API responses/requests.
type SpaceRef struct {
	Key string `json:"key" `
}

// BodyStorage represents the storage format of the body content.
type BodyStorage struct {
	Value          string `json:"value"`
	Representation string `json:"representation"`
}

// Body represents the body of a Confluence page, typically containing storage format.
type Body struct {
	Storage *BodyStorage `json:"storage,omitempty"`
}

// Version represents the version information of a Confluence page.
type Version struct {
	Number  int    `json:"number"`
	Message string `json:"message,omitempty"`
}

// Ancestor represents an ancestor page of a Confluence page.
type Ancestor struct {
	ID string `json:"id"`
}

// ConfluencePage represents a Confluence page or blogpost structure.
type ConfluencePage struct {
	ID        string     `json:"id,omitempty"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Space     *SpaceRef  `json:"space,omitempty"`
	Body      *Body      `json:"body,omitempty"`
	Version   *Version   `json:"version,omitempty"`
	Ancestors []Ancestor `json:"ancestors,omitempty"`
}

// getArguments helper extracts the "arguments" dictionary from an MCP tool request.
func getArguments(req mcp.CallToolRequest) (map[string]any, error) {
	if req.Params.Arguments == nil {
		return make(map[string]any), nil
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("arguments are not a JSON object")
	}
	return args, nil
}

// ensureExpand adds a property to an expansion string if not already present.
func ensureExpand(current, required string) string {
	if current == "" {
		return required
	}
	parts := strings.Split(current, ",")
	for _, p := range parts {
		if strings.TrimSpace(p) == required {
			return current
		}
	}
	return current + "," + required
}

// newQueryWithCommonArgs helper creates a url.Values object and populates it with common pagination and expansion parameters.
func newQueryWithCommonArgs(args map[string]any) url.Values {
	query := url.Values{}
	if limit, ok := args["limit"].(float64); ok {
		query.Set("limit", fmt.Sprintf("%d", int(limit)))
	} else {
		query.Set("limit", fmt.Sprintf("%d", defaultLimit))
	}
	if start, ok := args["start"].(float64); ok {
		query.Set("start", fmt.Sprintf("%d", int(start)))
	}
	if expand, ok := args["expand"].(string); ok && expand != "" {
		query.Set("expand", expand)
	}
	return query
}

// handleGetContent returns a tool handler for retrieving Confluence content by ID.
func handleGetContent(client *ConfluenceClient) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := getArguments(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		contentID, ok := args["contentId"].(string)
		if !ok || contentID == "" {
			return mcp.NewToolResultError("contentId must be a string and is required"), nil
		}

		if strings.Contains(contentID, "/") || strings.Contains(contentID, "..") {
			return mcp.NewToolResultError("invalid contentId format"), nil
		}

		query := newQueryWithCommonArgs(args)
		query.Set("expand", ensureExpand(query.Get("expand"), "body.storage"))

		resp, err := client.doRequest(ctx, "GET", "/content/"+contentID, query, nil)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("error getting content: %v", err)), nil
		}

		return mcp.NewToolResultText(string(resp)), nil
	}
}

// handleSearchContent returns a tool handler for searching Confluence content using CQL.
func handleSearchContent(client *ConfluenceClient) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := getArguments(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		cql, ok := args["cql"].(string)
		if !ok || cql == "" {
			return mcp.NewToolResultError("cql must be a string and is required"), nil
		}

		query := newQueryWithCommonArgs(args)
		query.Set("cql", cql)

		resp, err := client.doRequest(ctx, "GET", "/search", query, nil)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("error searching content: %v", err)), nil
		}

		return mcp.NewToolResultText(string(resp)), nil
	}
}

// handleCreateContent returns a tool handler for creating new content (page or blogpost) in Confluence.
func handleCreateContent(client *ConfluenceClient) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := getArguments(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		title, ok := args["title"].(string)
		if !ok || title == "" {
			return mcp.NewToolResultError("title is required"), nil
		}
		spaceKey, ok := args["spaceKey"].(string)
		if !ok || spaceKey == "" {
			return mcp.NewToolResultError("spaceKey is required"), nil
		}
		contentStr, ok := args["content"].(string)
		if !ok || contentStr == "" {
			return mcp.NewToolResultError("content is required"), nil
		}

		typeStr, ok := args["type"].(string)
		if !ok || typeStr == "" {
			typeStr = "page"
		}

		parentID, _ := args["parentId"].(string)

		payload := ConfluencePage{
			Type:  typeStr,
			Title: title,
			Space: &SpaceRef{Key: spaceKey},
			Body: &Body{
				Storage: &BodyStorage{
					Value:          contentStr,
					Representation: "storage",
				},
			},
		}

		if parentID != "" {
			payload.Ancestors = []Ancestor{{ID: parentID}}
		}

		resp, err := client.doRequest(ctx, "POST", "/content", nil, payload)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("error creating content: %v", err)), nil
		}

		return mcp.NewToolResultText(string(resp)), nil
	}
}

// handleUpdateContent returns a tool handler for updating existing content in Confluence.
func handleUpdateContent(client *ConfluenceClient) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := getArguments(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		contentID, ok := args["contentId"].(string)
		if !ok || contentID == "" {
			return mcp.NewToolResultError("contentId is required"), nil
		}

		if strings.Contains(contentID, "/") || strings.Contains(contentID, "..") {
			return mcp.NewToolResultError("invalid contentId format"), nil
		}

		query := newQueryWithCommonArgs(args)
		query.Set("expand", "body.storage,version,space")
		var currentData ConfluencePage
		if err := client.getJSON(ctx, "/content/"+contentID, query, &currentData); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to retrieve current content: %v", err)), nil
		}

		var newVersion int
		if v, ok := args["version"].(float64); ok {
			newVersion = int(v)
		} else {
			if currentData.Version == nil {
				return mcp.NewToolResultError("could not determine current version from API response"), nil
			}
			newVersion = currentData.Version.Number + 1
		}

		title, _ := args["title"].(string)
		contentStr, _ := args["content"].(string)
		versionComment, _ := args["versionComment"].(string)

		payload := ConfluencePage{
			ID:    contentID,
			Type:  currentData.Type,
			Space: currentData.Space,
			Version: &Version{
				Number:  newVersion,
				Message: versionComment,
			},
		}

		if title != "" {
			payload.Title = title
		} else {
			payload.Title = currentData.Title
		}

		if contentStr != "" {
			payload.Body = &Body{
				Storage: &BodyStorage{
					Value:          contentStr,
					Representation: "storage",
				},
			}
		} else if currentData.Body != nil {
			payload.Body = currentData.Body
		}

		resp, err := client.doRequest(ctx, "PUT", "/content/"+contentID, nil, payload)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("error updating content: %v", err)), nil
		}

		return mcp.NewToolResultText(string(resp)), nil
	}
}

// handleListSpaces returns a tool handler for listing/searching Confluence spaces.
func handleListSpaces(client *ConfluenceClient) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := getArguments(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		searchText, _ := args["searchText"].(string)
		var cql string
		if searchText == "" {
			cql = "type=space"
		} else {
			safeSearchText := strings.ReplaceAll(searchText, `"`, `\"`)
			cql = fmt.Sprintf(`type=space AND title ~ "%s"`, safeSearchText)
		}
		query := newQueryWithCommonArgs(args)
		query.Set("cql", cql)

		resp, err := client.doRequest(ctx, "GET", "/search", query, nil)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("error listing spaces: %v", err)), nil
		}

		return mcp.NewToolResultText(string(resp)), nil
	}
}

// setupServer configures the MCP server and returns it.
func setupServer(client *ConfluenceClient) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer(
		"atlassian-confluence-dc-go-mcp",
		"1.0.0",
		mcpserver.WithToolCapabilities(true),
	)

	s.AddTool(mcp.NewTool("confluence_get_content",
		mcp.WithDescription("Get Confluence content by ID from the Confluence Data Center edition instance"),
		mcp.WithString("contentId", mcp.Required(), mcp.Description("Confluence Data Center content ID")),
		mcp.WithString("expand", mcp.Description("Comma-separated list of properties to expand")),
	), handleGetContent(client))

	s.AddTool(mcp.NewTool("confluence_search_content",
		mcp.WithDescription("Search for content in Confluence Data Center edition instance using CQL"),
		mcp.WithString("cql", mcp.Required(), mcp.Description("Confluence Query Language (CQL) search string for Confluence Data Center")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of results to return (default: 25)")),
		mcp.WithNumber("start", mcp.Description("The starting index of the results to return")),
		mcp.WithString("expand", mcp.Description("Comma-separated list of properties to expand")),
	), handleSearchContent(client))

	s.AddTool(mcp.NewTool("confluence_create_content",
		mcp.WithDescription("Create new content in Confluence Data Center edition instance"),
		mcp.WithString("title", mcp.Required(), mcp.Description("The title of the new content")),
		mcp.WithString("spaceKey", mcp.Required(), mcp.Description("The key of the space where content will be created")),
		mcp.WithString("content", mcp.Required(), mcp.Description("The content of the page in Confluence storage format")),
		mcp.WithString("type", mcp.Description("The type of content (page or blogpost)")),
		mcp.WithString("parentId", mcp.Description("The ID of the parent content (optional)")),
	), handleCreateContent(client))

	s.AddTool(mcp.NewTool("confluence_update_content",
		mcp.WithDescription("Update existing content in Confluence Data Center edition instance"),
		mcp.WithString("contentId", mcp.Required(), mcp.Description("The ID of the content to update")),
		mcp.WithNumber("version", mcp.Description("The new version number (optional, defaults to current version + 1)")),
		mcp.WithString("title", mcp.Description("New title for the content")),
		mcp.WithString("content", mcp.Description("New content in storage format")),
		mcp.WithString("versionComment", mcp.Description("A comment for the new version")),
	), handleUpdateContent(client))

	s.AddTool(mcp.NewTool("confluence_list_spaces",
		mcp.WithDescription("List and search for spaces in Confluence Data Center edition instance"),
		mcp.WithString("searchText", mcp.Description("Text to search for in space names or descriptions (optional, returns all spaces if omitted)")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of spaces to return")),
		mcp.WithNumber("start", mcp.Description("The starting index of the results to return")),
		mcp.WithString("expand", mcp.Description("Comma-separated list of properties to expand")),
	), handleListSpaces(client))

	return s
}

type serveFunc func(*mcpserver.MCPServer) error

func run(serve serveFunc) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("configuration error: %v", err)
	}

	client := NewConfluenceClient(config)
	s := setupServer(client)

	if err := serve(s); err != nil {
		return fmt.Errorf("server error: %v", err)
	}
	return nil
}

func main() {
	if err := run(func(s *mcpserver.MCPServer) error {
		return mcpserver.ServeStdio(s)
	}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
