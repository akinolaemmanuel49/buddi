package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Client is a connected MCP server, named.
//
// It adds the protocol's two operations to the raw connection: the handshake and
// tool discovery. Everything above this deals in tool names and arguments, never
// in JSON-RPC.
type Client struct {
	name     string
	conn     *Conn
	identity implementation

	// tools is the discovered list, keyed by name. Held so a caller can ask what a
	// server offers without a round trip, and so CallTool can refuse an unknown
	// name before spending a round trip on it.
	tools map[string]Tool
}

// ClientOptions configures a client.
type ClientOptions struct {
	// Name identifies the server in logs and errors.
	Name string
	// Transport carries the messages.
	Transport Transport
	// ClientName and ClientVersion identify this process to the server.
	ClientName    string
	ClientVersion string
}

// Connect performs the handshake and discovers the server's tools.
//
// The handshake is not optional in this implementation. A server's tool list can
// mean different things under different protocol versions, and the alternative —
// discovering tools and finding out later — is a call whose meaning was never
// agreed.
func Connect(ctx context.Context, opts ClientOptions) (*Client, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("mcp: client name is required")
	}

	if opts.Transport == nil {
		return nil, fmt.Errorf("mcp: %s has no transport", opts.Name)
	}

	conn := Dial(opts.Transport)

	identity := implementation{Name: opts.ClientName, Version: opts.ClientVersion}
	if identity.Name == "" {
		identity = implementation{Name: "buddi-api", Version: "dev"}
	}

	client := &Client{name: opts.Name, conn: conn, identity: identity}

	var result initializeResult
	if err := conn.Call(ctx, MethodInitialize, initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      identity,
	}, &result); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("mcp: %s handshake failed: %w", opts.Name, err)
	}

	if err := conn.Notify(MethodInitialized, map[string]any{}); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("mcp: %s could not confirm handshake: %w", opts.Name, err)
	}

	if err := client.discover(ctx); err != nil {
		_ = conn.Close()

		return nil, err
	}

	return client, nil
}

// discover loads the tool list, following pagination to the end.
func (c *Client) discover(ctx context.Context) error {
	c.tools = make(map[string]Tool)

	var cursor string

	// Bounded so a server that keeps handing back the same cursor cannot spin
	// here forever. A thousand pages is far past any real tool list and turns a
	// broken server into a reported error instead of a hang.
	for range 1000 {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}

		var result ToolListResult
		if err := c.conn.Call(ctx, MethodToolsList, params, &result); err != nil {
			return fmt.Errorf("mcp: %s could not list tools: %w", c.name, err)
		}

		for _, tool := range result.Tools {
			if tool.Name == "" {
				return fmt.Errorf("mcp: %s listed a tool with no name", c.name)
			}

			c.tools[tool.Name] = tool
		}

		if result.NextCursor == "" {
			return nil
		}

		cursor = result.NextCursor
	}

	return fmt.Errorf("mcp: %s paginated its tool list without ending", c.name)
}

// Name returns the server's name.
func (c *Client) Name() string { return c.name }

// Tools returns the server's tools, sorted by name.
func (c *Client) Tools() []Tool {
	tools := make([]Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		tools = append(tools, tool)
	}

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	return tools
}

// Has reports whether the server offers a tool.
func (c *Client) Has(name string) bool {
	_, ok := c.tools[name]
	return ok
}

// CallTool invokes a tool and returns its result as raw JSON.
//
// A refusal comes back as a *ToolError rather than an error return: the call
// reached the server and the tool declined, which is an ordinary outcome for a
// connector and must not read as the connector being broken.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	if !c.Has(name) {
		return nil, fmt.Errorf("mcp: %s has no tool named %q", c.name, name)
	}

	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}

	var result CallToolResult
	if err := c.conn.Call(ctx, MethodToolsCall, CallToolParams{Name: name, Arguments: arguments}, &result); err != nil {
		return nil, fmt.Errorf("mcp: %s could not call %s: %w", c.name, name, err)
	}

	text, err := joinContent(result.Content)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s returned an unreadable result from %s: %w", c.name, name, err)
	}

	if result.IsError {
		return nil, &ToolError{Server: c.name, Tool: name, Reason: text}
	}

	return json.RawMessage(text), nil
}

// joinContent flattens a result's content parts into one string.
//
// Buddi's tools return JSON, and a text part holding JSON is the shape this client
// expects. Binary parts are refused rather than passed through: nothing in the
// current tool set returns an image, so accepting one silently would mean
// smuggling base64 into a JSON payload the caller then tries to parse.
func joinContent(content []Content) (string, error) {
	var builder strings.Builder

	for _, part := range content {
		if part.Type != "text" {
			return "", fmt.Errorf("content of type %q is not supported", part.Type)
		}

		if builder.Len() > 0 {
			builder.WriteString("\n")
		}

		builder.WriteString(part.Text)
	}

	if builder.Len() == 0 {
		return "", fmt.Errorf("result had no text content")
	}

	return builder.String(), nil
}

// Close ends the connection.
func (c *Client) Close() error { return c.conn.Close() }

// ToolError reports that a tool was reached and refused.
//
// It is distinct from a transport failure so a caller can tell a connector saying
// "not authorised" from a connector that is not answering.
type ToolError struct {
	Server string
	Tool   string
	Reason string
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("mcp: %s refused %s: %s", e.Server, e.Tool, e.Reason)
}
