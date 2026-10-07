package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// errReadDone marks a clean end of the stream.
var errReadDone = errors.New("mcp: read complete")

// maxMessageBytes bounds one JSON-RPC message.
//
// A tool result can be a plan or a note's worth of text in one line, and the default
// 64KiB scanner limit is small enough that a large result would be truncated into
// an invalid message — which surfaces as a disconnected session rather than as the
// actual problem.
const maxMessageBytes = 8 * 1024 * 1024

// lineScanner reads newline-delimited messages.
//
// The client connection and the server use different concurrency shapes — the
// client has a background reader while the server reads on its own goroutine — so
// this is a thin wrapper rather than a shared implementation with a lock.
type lineScanner struct {
	scanner *bufio.Scanner
}

func newLineScanner(r io.Reader) *lineScanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMessageBytes)

	return &lineScanner{scanner: scanner}
}

func (l *lineScanner) next() ([]byte, error) {
	for {
		if !l.scanner.Scan() {
			if err := l.scanner.Err(); err != nil {
				return nil, fmt.Errorf("mcp: could not read message: %w", err)
			}

			return nil, errReadDone
		}

		line := l.scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		return line, nil
	}
}

// pipeTransport is an in-memory bidirectional transport.
//
// Used by tests and by an embedded server in the same process. The buffer is
// bounded so a reader that stops reading cannot make a writer block forever: the
// connection then reports a write error, which is what a real dead peer does.
type pipeTransport struct {
	reader *io.PipeReader
	writer *io.PipeWriter

	closeOnce sync.Once
}

// NewPipeTransport returns the two ends of one in-memory connection.
func NewPipeTransport() (client, server Transport) {
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	return &pipeTransport{reader: clientReader, writer: clientWriter},
		&pipeTransport{reader: serverReader, writer: serverWriter}
}

func (p *pipeTransport) Read(b []byte) (int, error)  { return p.reader.Read(b) }
func (p *pipeTransport) Write(b []byte) (int, error) { return p.writer.Write(b) }

func (p *pipeTransport) Close() error {
	p.closeOnce.Do(func() {
		_ = p.reader.Close()
		_ = p.writer.Close()
	})

	return nil
}

// NewStdioTransport returns the process's own standard streams as a transport,
// which is how an MCP server run as its own program is reached.
func NewStdioTransport() Transport {
	return stdioTransport{in: os.Stdin, out: os.Stdout}
}

// stdioTransport carries messages over a process's standard input and output.
//
// Errors go to standard error rather than to the stream: the stream is the
// protocol channel, and a log line written into it is a malformed message to the
// client rather than a diagnostic.
type stdioTransport struct {
	in  io.Reader
	out io.Writer
}

func (s stdioTransport) Read(b []byte) (int, error)  { return s.in.Read(b) }
func (s stdioTransport) Write(b []byte) (int, error) { return s.out.Write(b) }

// Close closes the output side only. Closing the input would end the reader, and
// closing standard error would take down logging for the rest of the process.
func (s stdioTransport) Close() error { return nil }
