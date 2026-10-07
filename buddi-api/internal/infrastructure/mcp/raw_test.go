package mcp_test

import (
	"bufio"
	"encoding/json"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/mcp"
)

// rawCall issues one JSON-RPC request outside the client, so a test can reach a
// method the typed client does not expose.
func rawCall(transport mcp.Transport, method string) error {
	id := json.RawMessage(`"raw-1"`)

	request, err := json.Marshal(mcp.Request{
		JSONRPC: mcp.Version,
		ID:      &id,
		Method:  method,
	})
	if err != nil {
		return err
	}

	if _, err := transport.Write(append(request, '\n')); err != nil {
		return err
	}

	line, err := bufio.NewReader(transport).ReadBytes('\n')
	if err != nil {
		return err
	}

	var response mcp.Response
	if err := json.Unmarshal(line, &response); err != nil {
		return err
	}

	if response.Error == nil {
		return nil
	}

	return &mcp.CallError{Method: method, Err: response.Error}
}
