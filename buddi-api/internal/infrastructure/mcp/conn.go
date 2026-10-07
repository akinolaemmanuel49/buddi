package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
)

// Transport carries newline-delimited JSON-RPC messages in both directions.
//
// It is deliberately tiny: a reader, a writer, and a closer. Everything else —
// request correlation, the handshake, error mapping — is built on top of it, which
// is what lets an in-memory pipe and a subprocess's stdio be the same code path.
type Transport interface {
	io.Reader
	io.Writer
	io.Closer
}

// Conn is a multiplexed connection to one MCP server.
//
// It is safe for concurrent use. Calls must be, because the agent holds one
// connection per server and a plan can propose steps in parallel; the write side is
// serialised so two goroutines cannot interleave halves of a message, and replies
// are matched to calls by id rather than by arrival order.
type Conn struct {
	transport Transport

	// writeMu keeps two concurrent calls from interleaving halves of a message.
	// Each write is one line, and a partial line is a parse error on the far side
	// that reads as a broken server.
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan *Response

	// readErr is closed once the read loop has stopped for any reason, so a call
	// blocked on a reply fails instead of hanging forever when the server dies.
	readErr  chan struct{}
	readOnce sync.Once
	// readErrValue records why reading stopped, for the error a blocked call
	// returns.
	readErrValue error
}

// Dial wraps a transport in a connection and starts its read loop.
//
// The read loop runs for the life of the connection. It stops when the transport
// reaches EOF or fails, and closing the connection stops it.
func Dial(transport Transport) *Conn {
	conn := &Conn{
		transport: transport,
		pending:   make(map[string]chan *Response),
		readErr:   make(chan struct{}),
	}

	go conn.read()

	return conn
}

func (c *Conn) read() {
	scanner := newLineScanner(c.transport)

	for {
		line, err := scanner.next()
		if err != nil {
			c.fail(fmt.Errorf("mcp: connection closed: %w", err))

			return
		}

		var response Response
		if err := json.Unmarshal(line, &response); err != nil {
			// A single malformed message must not end the session: the next one may
			// be perfectly good, and the alternative is one bad line taking down a
			// working connector.
			continue
		}

		c.deliver(&response)
	}
}

func (c *Conn) deliver(response *Response) {
	if len(response.ID) == 0 {
		// A server-initiated notification or request. Nothing in this
		// implementation sends those, so there is nothing to do with it.
		return
	}

	c.pendingMu.Lock()

	ch, ok := c.pending[string(response.ID)]
	if ok {
		delete(c.pending, string(response.ID))
	}

	c.pendingMu.Unlock()

	if ok {
		ch <- response
	}
}

// fail unblocks every caller waiting on a reply.
func (c *Conn) fail(err error) {
	c.readOnce.Do(func() {
		c.readErrValue = err
		close(c.readErr)

		c.pendingMu.Lock()
		pending := c.pending
		c.pending = make(map[string]chan *Response)
		c.pendingMu.Unlock()

		for _, ch := range pending {
			ch <- &Response{
				JSONRPC: Version,
				Error:   &RPCError{Code: CodeInternalError, Message: err.Error()},
			}
		}
	})
}

// Close releases the transport and unblocks any in-flight call.
func (c *Conn) Close() error {
	err := c.transport.Close()
	c.fail(fmt.Errorf("mcp: connection closed"))

	return err
}

// Call sends a request and waits for its reply.
//
// A server-side failure is returned as an *RPCError so a caller can tell a refused
// call from a broken connection: the first is the connector's answer, the second
// is the connector being unreachable.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("mcp: could not encode %s params: %w", method, err)
	}

	// A UUID rather than a counter: ids only have to be unique within one
	// connection, and a string id keeps the matching in the read loop obvious.
	id := json.RawMessage(fmt.Sprintf("%q", uuid.NewString()))

	ch := make(chan *Response, 1)

	c.pendingMu.Lock()

	select {
	case <-c.readErr:
		c.pendingMu.Unlock()

		return c.readErrValue
	default:
	}

	c.pending[string(id)] = ch
	c.pendingMu.Unlock()

	request := Request{JSONRPC: Version, ID: &id, Method: method, Params: encoded}

	if err := c.write(request); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, string(id))
		c.pendingMu.Unlock()

		return err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, string(id))
		c.pendingMu.Unlock()

		return fmt.Errorf("mcp: %s: %w", method, ctx.Err())

	case <-c.readErr:
		return c.readErrValue

	case response := <-ch:
		if response.Error != nil {
			return &CallError{Method: method, Err: response.Error}
		}

		if result == nil {
			return nil
		}

		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("mcp: could not decode %s result: %w", method, err)
		}

		return nil
	}
}

// Notify sends a message that expects no reply.
func (c *Conn) Notify(method string, params any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("mcp: could not encode %s params: %w", method, err)
	}

	return c.write(Request{JSONRPC: Version, Method: method, Params: encoded})
}

func (c *Conn) write(v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mcp: could not encode message: %w", err)
	}

	// Serialised so two concurrent calls cannot interleave: each write is one
	// line, and a partial line would be a parse error on the far side that looks
	// like a broken server.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if _, err := c.transport.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("mcp: could not write message: %w", err)
	}

	return nil
}

// CallError reports that a call reached the server and was refused.
type CallError struct {
	Method string
	Err    *RPCError
}

func (e *CallError) Error() string {
	return fmt.Sprintf("mcp: %s failed: %s", e.Method, e.Err.Message)
}

func (e *CallError) Unwrap() error { return e.Err }
