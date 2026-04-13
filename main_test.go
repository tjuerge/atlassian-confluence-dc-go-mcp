package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// TestLoadConfig tests configuration loading from environment variables.
func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		wantURL string
	}{
		{
			name: "valid config with CONFLUENCE_BASE_URL",
			env: map[string]string{
				"CONFLUENCE_API_TOKEN": "test-token",
				"CONFLUENCE_BASE_URL":  "https://example.atlassian.net",
			},
			wantErr: false,
			wantURL: "https://example.atlassian.net/rest/api",
		},
		{
			name: "valid config with CONFLUENCE_HOST",
			env: map[string]string{
				"CONFLUENCE_API_TOKEN": "test-token",
				"CONFLUENCE_HOST":      "example.atlassian.net",
			},
			wantErr: false,
			wantURL: "https://example.atlassian.net/rest/api",
		},
		{
			name: "missing token",
			env: map[string]string{
				"CONFLUENCE_BASE_URL": "https://example.atlassian.net",
			},
			wantErr: true,
		},
		{
			name: "missing URL",
			env: map[string]string{
				"CONFLUENCE_API_TOKEN": "test-token",
			},
			wantErr: true,
		},
		{
			name: "invalid URL format",
			env: map[string]string{
				"CONFLUENCE_API_TOKEN": "test-token",
				"CONFLUENCE_BASE_URL":  "://invalid-url",
			},
			wantErr: true,
		},
		{
			name: "invalid URL scheme",
			env: map[string]string{
				"CONFLUENCE_API_TOKEN": "test-token",
				"CONFLUENCE_BASE_URL":  "ftp://example.com",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			config, err := loadConfig()
			if (err != nil) != tt.wantErr {
				t.Errorf("loadConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && config.BaseURL != tt.wantURL {
				t.Errorf("loadConfig() BaseURL = %v, want %v", config.BaseURL, tt.wantURL)
			}
		})
	}
}

// TestEnsureExpand tests appending expansion properties.
func TestEnsureExpand(t *testing.T) {
	tests := []struct {
		current  string
		required string
		want     string
	}{
		{"", "body.storage", "body.storage"},
		{"version", "body.storage", "version,body.storage"},
		{"body.storage", "body.storage", "body.storage"},
		{"version,body.storage", "body.storage", "version,body.storage"},
	}

	for _, tt := range tests {
		got := ensureExpand(tt.current, tt.required)
		if got != tt.want {
			t.Errorf("ensureExpand(%q, %q) = %q, want %q", tt.current, tt.required, got, tt.want)
		}
	}
}

// TestGetArguments tests extracting arguments from MCP requests.
func TestGetArguments(t *testing.T) {
	t.Run("nil arguments", func(t *testing.T) {
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: nil}}
		args, err := getArguments(req)
		if err != nil || len(args) != 0 {
			t.Errorf("expected empty args, got %v, %v", args, err)
		}
	})

	t.Run("invalid arguments type", func(t *testing.T) {
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: "not-a-map"}}
		_, err := getArguments(req)
		if err == nil {
			t.Error("expected error for non-map arguments")
		}
	})
}

// TestNewQueryWithCommonArgs tests mapping MCP arguments to URL query parameters.
func TestNewQueryWithCommonArgs(t *testing.T) {
	args := map[string]any{
		"limit":  float64(10),
		"start":  float64(5),
		"expand": "body.storage",
	}
	query := newQueryWithCommonArgs(args)

	if query.Get("limit") != "10" {
		t.Errorf("expected limit 10, got %s", query.Get("limit"))
	}
	if query.Get("start") != "5" {
		t.Errorf("expected start 5, got %s", query.Get("start"))
	}
	if query.Get("expand") != "body.storage" {
		t.Errorf("expected expand body.storage, got %s", query.Get("expand"))
	}
}

// TestHandleGetContent tests retrieving Confluence content.
func TestHandleGetContent(t *testing.T) {
	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/content/123" {
			t.Errorf("expected path /rest/api/content/123, got %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "expand=body.storage") {
			t.Errorf("expected expand=body.storage in query, got %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"123","title":"Test Page"}`))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{
		BaseURL: server.URL + "/rest/api",
		Token:   "test-token",
	})

	handler := handleGetContent(client)
	ctx := context.Background()
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "confluence_get_content",
			Arguments: map[string]any{
				"contentId": "123",
			},
		},
	}

	result, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	if result.IsError {
		t.Fatalf("handler returned error: %v", result.Content)
	}

	var page map[string]any
	if err := json.Unmarshal([]byte(result.Content[0].(mcp.TextContent).Text), &page); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if page["id"] != "123" || page["title"] != "Test Page" {
		t.Errorf("unexpected page content: %v", page)
	}

	t.Run("missing contentId", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "confluence_get_content",
				Arguments: map[string]any{},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for missing contentId")
		}
	})

	t.Run("invalid contentId format", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "confluence_get_content",
				Arguments: map[string]any{
					"contentId": "../etc/passwd",
				},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for invalid contentId format")
		}
	})

	t.Run("api error", func(t *testing.T) {
		errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
		}))
		defer errServer.Close()

		errClient := NewConfluenceClient(&ConfluenceConfig{BaseURL: errServer.URL, Token: "token"})
		errHandler := handleGetContent(errClient)
		result, _ := errHandler(ctx, req)
		if !result.IsError {
			t.Error("expected error for API 404")
		}
	})
}

// TestHandleListSpaces tests listing and searching spaces.
func TestHandleListSpaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/search" {
			t.Errorf("expected path /rest/api/search, got %s", r.URL.Path)
		}
		cql := r.URL.Query().Get("cql")
		if !strings.Contains(cql, "type=space") {
			t.Errorf("expected cql to contain type=space, got %s", cql)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{
		BaseURL: server.URL + "/rest/api",
		Token:   "test-token",
	})

	handler := handleListSpaces(client)
	ctx := context.Background()

	t.Run("list all spaces", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "confluence_list_spaces",
				Arguments: map[string]any{},
			},
		}
		result, err := handler(ctx, req)
		if err != nil || result.IsError {
			t.Fatalf("handler failed: %v, %v", err, result)
		}
	})

	t.Run("search spaces", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "confluence_list_spaces",
				Arguments: map[string]any{
					"searchText": "Test",
				},
			},
		}
		result, err := handler(ctx, req)
		if err != nil || result.IsError {
			t.Fatalf("handler failed: %v, %v", err, result)
		}
	})
}

// TestHandleSearchContent tests searching content via CQL.
func TestHandleSearchContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cql := r.URL.Query().Get("cql")
		if cql != "title ~ \"Test\"" {
			t.Errorf("expected cql title ~ \"Test\", got %s", cql)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{
		BaseURL: server.URL + "/rest/api",
		Token:   "test-token",
	})

	handler := handleSearchContent(client)
	ctx := context.Background()
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "confluence_search_content",
			Arguments: map[string]any{
				"cql": "title ~ \"Test\"",
			},
		},
	}

	result, err := handler(ctx, req)
	if err != nil || result.IsError {
		t.Fatalf("handler failed: %v, %v", err, result)
	}
}

// TestHandleCreateContent tests creating new content.
func TestHandleCreateContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var page ConfluencePage
		if err := json.NewDecoder(r.Body).Decode(&page); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if page.Title != "New Page" {
			t.Errorf("expected title New Page, got %s", page.Title)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"456","title":"New Page"}`))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{
		BaseURL: server.URL + "/rest/api",
		Token:   "test-token",
	})

	handler := handleCreateContent(client)
	ctx := context.Background()
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "confluence_create_content",
			Arguments: map[string]any{
				"title":    "New Page",
				"spaceKey": "TEST",
				"content":  "<p>Hello</p>",
			},
		},
	}

	result, err := handler(ctx, req)
	if err != nil || result.IsError {
		t.Fatalf("handler failed: %v, %v", err, result)
	}
}

// TestHandleUpdateContent tests updating existing content.
func TestHandleUpdateContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"123","title":"Old Title","version":{"number":1}}`))
			return
		}
		if r.Method == "PUT" {
			var page ConfluencePage
			if err := json.NewDecoder(r.Body).Decode(&page); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}
			if page.Version.Number != 2 {
				t.Errorf("expected version 2, got %d", page.Version.Number)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"123","title":"New Title","version":{"number":2}}`))
			return
		}
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{
		BaseURL: server.URL + "/rest/api",
		Token:   "test-token",
	})

	handler := handleUpdateContent(client)
	ctx := context.Background()
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "confluence_update_content",
			Arguments: map[string]any{
				"contentId": "123",
				"title":     "New Title",
			},
		},
	}

	result, err := handler(ctx, req)
	if err != nil || result.IsError {
		t.Fatalf("handler failed: %v, %v", err, result)
	}
}

// TestHandleSearchContentErrors tests error handling in search.
func TestHandleSearchContentErrors(t *testing.T) {
	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
	handler := handleSearchContent(client)
	ctx := context.Background()

	t.Run("missing cql", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "confluence_search_content",
				Arguments: map[string]any{},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for missing cql")
		}
	})
}

// TestHandleCreateContentErrors tests error handling in create.
func TestHandleCreateContentErrors(t *testing.T) {
	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
	handler := handleCreateContent(client)
	ctx := context.Background()

	t.Run("missing required fields", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "confluence_create_content",
				Arguments: map[string]any{},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for missing title")
		}
	})
}

// TestHandleUpdateContentErrors tests error handling in update.
func TestHandleUpdateContentErrors(t *testing.T) {
	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
	handler := handleUpdateContent(client)
	ctx := context.Background()

	t.Run("missing contentId", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "confluence_update_content",
				Arguments: map[string]any{},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for missing contentId")
		}
	})
}

// TestExecuteRequestErrors tests edge cases in request execution.
func TestExecuteRequestErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid base URL in client", func(t *testing.T) {
		client := &ConfluenceClient{
			config: &ConfluenceConfig{BaseURL: "%%"}, // Invalid URL
		}
		_, err := client.executeRequest(ctx, "GET", "/path", nil, nil)
		if err == nil {
			t.Error("expected error for invalid base URL")
		}
	})

	t.Run("invalid body marshal", func(t *testing.T) {
		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
		// Channel cannot be marshaled to JSON
		_, err := client.executeRequest(ctx, "POST", "/path", nil, make(chan int))
		if err == nil {
			t.Error("expected error for unmarshalable body")
		}
	})
}

// TestGetJSONErrors tests error paths in getJSON.
func TestGetJSONErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid json response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{invalid-json}`))
		}))
		defer server.Close()

		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
		var target map[string]any
		err := client.getJSON(ctx, "/", nil, &target)
		if err == nil {
			t.Error("expected error for invalid JSON")
		}
	})

	t.Run("api error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`error message`))
		}))
		defer server.Close()

		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
		var target map[string]any
		err := client.getJSON(ctx, "/", nil, &target)
		if err == nil || !strings.Contains(err.Error(), "API error") {
			t.Errorf("expected API error, got %v", err)
		}
	})
}

// TestDoRequestAPIError tests API errors in doRequest.
func TestDoRequestAPIError(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal server error`))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	_, err := client.doRequest(ctx, "GET", "/", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "API error (status 500)") {
		t.Errorf("expected 500 API error, got %v", err)
	}
}

// TestLoadConfigMore covers additional paths in loadConfig.
func TestLoadConfigMore(t *testing.T) {
	t.Run("valid config with CONFLUENCE_API_BASE_PATH", func(t *testing.T) {
		t.Setenv("CONFLUENCE_API_TOKEN", "test-token")
		t.Setenv("CONFLUENCE_API_BASE_PATH", "https://example.com/wiki")
		config, err := loadConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if config.BaseURL != "https://example.com/wiki/rest/api" {
			t.Errorf("expected URL with /rest/api, got %s", config.BaseURL)
		}
	})

	t.Run("URL without protocol", func(t *testing.T) {
		t.Setenv("CONFLUENCE_API_TOKEN", "test-token")
		t.Setenv("CONFLUENCE_BASE_URL", "example.com")
		config, err := loadConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(config.BaseURL, "https://") {
			t.Errorf("expected https prefix, got %s", config.BaseURL)
		}
	})
}

// TestHandleCreateContentMore covers additional paths in handleCreateContent.
func TestHandleCreateContentMore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var page ConfluencePage
		_ = json.NewDecoder(r.Body).Decode(&page)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	handler := handleCreateContent(client)
	ctx := context.Background()

	t.Run("create with parentId and type", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{
					"title":    "Child Page",
					"spaceKey": "TEST",
					"content":  "content",
					"type":     "blogpost",
					"parentId": "123",
				},
			},
		}
		result, err := handler(ctx, req)
		if err != nil || result.IsError {
			t.Fatalf("handler failed: %v, %v", err, result)
		}
		if !strings.Contains(result.Content[0].(mcp.TextContent).Text, `"type":"blogpost"`) {
			t.Error("expected blogpost type in result")
		}
		if !strings.Contains(result.Content[0].(mcp.TextContent).Text, `"ancestors":[{"id":"123"}]`) {
			t.Error("expected ancestors in result")
		}
	})

	t.Run("getArguments failure", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: "invalid",
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for invalid arguments")
		}
	})
}

// TestHandleUpdateContentMore covers additional paths in handleUpdateContent.
func TestHandleUpdateContentMore(t *testing.T) {
	ctx := context.Background()

	t.Run("update with explicit version and versions comment", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				_ = json.NewEncoder(w).Encode(ConfluencePage{
					ID:    "123",
					Title: "Old",
					Type:  "page",
					Space: &SpaceRef{Key: "TS"},
					Body:  &Body{Storage: &BodyStorage{Value: "old content"}},
				})
				return
			}
			var page ConfluencePage
			_ = json.NewDecoder(r.Body).Decode(&page)
			if page.Version.Number != 10 {
				t.Errorf("expected version 10, got %d", page.Version.Number)
			}
			if page.Version.Message != "update msg" {
				t.Errorf("expected message update msg, got %s", page.Version.Message)
			}
			_ = json.NewEncoder(w).Encode(page)
		}))
		defer server.Close()

		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
		handler := handleUpdateContent(client)
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{
					"contentId":      "123",
					"version":        float64(10),
					"versionComment": "update msg",
					"content":        "new content",
				},
			},
		}
		result, err := handler(ctx, req)
		if err != nil || result.IsError {
			t.Fatalf("handler failed: %v, %v", err, result)
		}
	})

	t.Run("update without new title/content (use current)", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				_ = json.NewEncoder(w).Encode(ConfluencePage{
					ID:      "123",
					Title:   "Current Title",
					Version: &Version{Number: 1},
					Body:    &Body{Storage: &BodyStorage{Value: "Current Content"}},
				})
				return
			}
			var page ConfluencePage
			_ = json.NewDecoder(r.Body).Decode(&page)
			if page.Title != "Current Title" {
				t.Errorf("expected current title, got %s", page.Title)
			}
			_ = json.NewEncoder(w).Encode(page)
		}))
		defer server.Close()

		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
		handler := handleUpdateContent(client)
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{
					"contentId": "123",
				},
			},
		}
		_, _ = handler(ctx, req)
	})

	t.Run("missing current version error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(ConfluencePage{ID: "123"}) // No version
		}))
		defer server.Close()

		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
		handler := handleUpdateContent(client)
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{"contentId": "123"},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError || !strings.Contains(result.Content[0].(mcp.TextContent).Text, "could not determine current version") {
			t.Error("expected version error")
		}
	})

	t.Run("invalid contentId format", func(t *testing.T) {
		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
		handler := handleUpdateContent(client)
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{"contentId": "../bad"},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for bad contentId")
		}
	})

	t.Run("getArguments failure", func(t *testing.T) {
		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
		handler := handleUpdateContent(client)
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: "invalid",
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for invalid arguments")
		}
	})
}

// TestHandleListSpacesMore covers additional paths in handleListSpaces.
func TestHandleListSpacesMore(t *testing.T) {
	ctx := context.Background()

	t.Run("search with quotes", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cql := r.URL.Query().Get("cql")
			if !strings.Contains(cql, `\"quoted\"`) {
				t.Errorf("expected escaped quotes in CQL, got %s", cql)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()

		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
		handler := handleListSpaces(client)
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{"searchText": `a "quoted" word`},
			},
		}
		_, _ = handler(ctx, req)
	})

	t.Run("getArguments failure", func(t *testing.T) {
		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
		handler := handleListSpaces(client)
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: "invalid",
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for invalid arguments")
		}
	})
}

// TestHandleGetContentMore covers additional paths in handleGetContent.
func TestHandleGetContentMore(t *testing.T) {
	ctx := context.Background()
	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
	handler := handleGetContent(client)

	t.Run("getArguments failure", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: "invalid",
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for invalid arguments")
		}
	})
}

// TestHandleSearchContentMore covers additional paths in handleSearchContent.
func TestHandleSearchContentMore(t *testing.T) {
	ctx := context.Background()
	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
	handler := handleSearchContent(client)

	t.Run("getArguments failure", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: "invalid",
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for invalid arguments")
		}
	})
}

// TestHandleCreateContentMissingArgs covers missing required arguments in handleCreateContent.
func TestHandleCreateContentMissingArgs(t *testing.T) {
	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
	handler := handleCreateContent(client)
	ctx := context.Background()

	t.Run("missing spaceKey", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{"title": "T", "content": "C"},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError || !strings.Contains(result.Content[0].(mcp.TextContent).Text, "spaceKey is required") {
			t.Error("expected spaceKey error")
		}
	})

	t.Run("missing content", func(t *testing.T) {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{"title": "T", "spaceKey": "S"},
			},
		}
		result, _ := handler(ctx, req)
		if !result.IsError || !strings.Contains(result.Content[0].(mcp.TextContent).Text, "content is required") {
			t.Error("expected content error")
		}
	})
}

// TestTransportErrors covers transport level errors in ConfluenceClient.
func TestTransportErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("executeRequest connection failure", func(t *testing.T) {
		// Use an invalid address to trigger connection failure
		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://invalid.local", Token: "t"})
		_, err := client.executeRequest(ctx, "GET", "/", nil, nil)
		if err == nil {
			t.Error("expected error for connection failure")
		}
	})

	t.Run("doRequest creation failure", func(t *testing.T) {
		client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
		// Control characters in method or path can cause NewRequest to fail
		_, err := client.executeRequest(ctx, "IDK\x7f", "/", nil, nil)
		if err == nil {
			t.Error("expected error for invalid method")
		}
	})
}

// TestHandlerAPIErrors covers where handlers receive errors from the client.
func TestHandlerAPIErrors(t *testing.T) {
	ctx := context.Background()
	// A server that just closes the connection
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("webserver doesn't support hijacking")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})

	t.Run("handleSearchContent error", func(t *testing.T) {
		handler := handleSearchContent(client)
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"cql": "cql"}}}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for connection close")
		}
	})

	t.Run("handleCreateContent error", func(t *testing.T) {
		handler := handleCreateContent(client)
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"title": "T", "spaceKey": "S", "content": "C"}}}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for connection close")
		}
	})

	t.Run("handleUpdateContent error", func(t *testing.T) {
		handler := handleUpdateContent(client)
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"contentId": "123"}}}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for connection close")
		}
	})

	t.Run("handleListSpaces error", func(t *testing.T) {
		handler := handleListSpaces(client)
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{}}}
		result, _ := handler(ctx, req)
		if !result.IsError {
			t.Error("expected error for connection close")
		}
	})
}

// TestHandleUpdateContentPutError tests the case where GET succeeds but PUT fails.
func TestHandleUpdateContentPutError(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ConfluencePage{
				ID:      "123",
				Title:   "Old",
				Type:    "page",
				Version: &Version{Number: 1},
			})
			return
		}
		// PUT fails
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("put failed"))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	handler := handleUpdateContent(client)
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{"contentId": "123"},
		},
	}
	result, _ := handler(ctx, req)
	if !result.IsError || !strings.Contains(result.Content[0].(mcp.TextContent).Text, "error updating content") {
		t.Errorf("expected update error, got %v", result.Content)
	}
}

// TestSetupServer tests the setupServer function.
func TestSetupServer(t *testing.T) {
	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: "http://localhost", Token: "t"})
	s := setupServer(client)
	if s == nil {
		t.Fatal("setupServer returned nil")
	}
}

// TestDoRequestReadError tests io.Read error in doRequest.
func TestDoRequestReadError(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		// Don't write enough bytes, then close
		_, _ = w.Write([]byte("too short"))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	_, err := client.doRequest(ctx, "GET", "/", nil, nil)
	if err == nil {
		t.Error("expected error for truncated body")
	}
}

// TestRun tests the run function.
func TestRun(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Setenv("CONFLUENCE_API_TOKEN", "token")
		t.Setenv("CONFLUENCE_BASE_URL", "http://localhost")
		err := run(func(s *mcpserver.MCPServer) error {
			return nil // dummy serve
		})
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})

	t.Run("config error", func(t *testing.T) {
		t.Setenv("CONFLUENCE_API_TOKEN", "") // trigger error
		err := run(func(s *mcpserver.MCPServer) error {
			return nil
		})
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "configuration error") {
			t.Errorf("expected config error, got %v", err)
		}
	})

	t.Run("serve error", func(t *testing.T) {
		t.Setenv("CONFLUENCE_API_TOKEN", "token")
		t.Setenv("CONFLUENCE_BASE_URL", "http://localhost")
		err := run(func(s *mcpserver.MCPServer) error {
			return fmt.Errorf("serve failed")
		})
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "server error") {
			t.Errorf("expected serve error, got %v", err)
		}
	})
}

// TestParseRetryAfter tests parsing the retry-after header.
func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected int
	}{
		{"empty header", "", 0},
		{"valid seconds", "5", 5},
		{"with whitespace", "  10  ", 10},
		{"invalid value", "invalid", 0},
		{"zero seconds", "0", 0},
		{"large value", "120", 120},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				Header: http.Header{},
			}
			if tt.header != "" {
				resp.Header.Set("retry-after", tt.header)
			}
			got := parseRetryAfter(resp)
			if got != tt.expected {
				t.Errorf("parseRetryAfter() = %d, want %d", got, tt.expected)
			}
		})
	}
}

// TestRateLimitRetryAfter tests that doRequest retries on 429 with retry-after header.
func TestRateLimitRetryAfter(t *testing.T) {
	ctx := context.Background()
	attempt := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt < 2 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"123"}`))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	resp, err := client.doRequest(ctx, "GET", "/", nil, nil)
	if err != nil {
		t.Fatalf("doRequest failed: %v", err)
	}
	if attempt != 2 {
		t.Errorf("expected 2 attempts, got %d", attempt)
	}
	if !strings.Contains(string(resp), "123") {
		t.Errorf("expected response with id, got %s", string(resp))
	}
}

// TestRateLimitExhausted tests that doRequest fails after max retries.
func TestRateLimitExhausted(t *testing.T) {
	ctx := context.Background()
	attempt := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		w.Header().Set("retry-after", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	_, err := client.doRequest(ctx, "GET", "/", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "rate limited after") {
		t.Errorf("expected rate limit error, got %v", err)
	}
	if attempt != maxRetries+1 {
		t.Errorf("expected %d attempts, got %d", maxRetries+1, attempt)
	}
}

// TestGetJSONRateLimitRetry tests that getJSON retries on 429.
func TestGetJSONRateLimitRetry(t *testing.T) {
	ctx := context.Background()
	attempt := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt < 2 {
			w.Header().Set("retry-after", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"123","title":"Test"}`))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	var target map[string]any
	err := client.getJSON(ctx, "/", nil, &target)
	if err != nil {
		t.Fatalf("getJSON failed: %v", err)
	}
	if attempt != 2 {
		t.Errorf("expected 2 attempts, got %d", attempt)
	}
	if target["id"] != "123" {
		t.Errorf("expected id 123, got %v", target["id"])
	}
}

// TestGetJSONRateLimitExhausted tests that getJSON fails after max retries.
func TestGetJSONRateLimitExhausted(t *testing.T) {
	ctx := context.Background()
	attempt := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		w.Header().Set("retry-after", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	var target map[string]any
	err := client.getJSON(ctx, "/", nil, &target)
	if err == nil || !strings.Contains(err.Error(), "rate limited after") {
		t.Errorf("expected rate limit error, got %v", err)
	}
	if attempt != maxRetries+1 {
		t.Errorf("expected %d attempts, got %d", maxRetries+1, attempt)
	}
}

// TestRateLimitWithoutRetryAfter tests retry with missing retry-after header (defaults to 0).
func TestRateLimitWithoutRetryAfter(t *testing.T) {
	ctx := context.Background()
	attempt := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"123"}`))
	}))
	defer server.Close()

	client := NewConfluenceClient(&ConfluenceConfig{BaseURL: server.URL, Token: "t"})
	resp, err := client.doRequest(ctx, "GET", "/", nil, nil)
	if err != nil {
		t.Fatalf("doRequest failed: %v", err)
	}
	if attempt != 2 {
		t.Errorf("expected 2 attempts, got %d", attempt)
	}
	if !strings.Contains(string(resp), "123") {
		t.Errorf("expected successful response, got %s", string(resp))
	}
}
