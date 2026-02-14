package ssh

import (
	"io"
	"os"
)

// readWriter combines separate read and write halves into an io.ReadWriter.
type readWriter struct {
	io.Reader
	io.Writer
}

// pipePair returns an os.Pipe pair suitable for backing a term.Terminal in tests.
// The caller writes to w (terminal output) and reads from r.
func pipePair() (*os.File, *os.File, error) {
	return os.Pipe()
}
