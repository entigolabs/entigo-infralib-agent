package agentstream

import (
	"bytes"
	"io"
	"log"
	"sync"
)

const maxBufferedLines = 1000

// LogTee copies the standard logger's output, which slog's default handler also
// writes to, into the backend stream line by line. Lines written before Attach
// are buffered so output logged before the stream connects isn't lost.
type LogTee struct {
	out io.Writer

	mu       sync.Mutex
	partial  []byte
	buffered []string
	sink     func(string)
	detached bool
}

func InstallLogTee() *LogTee {
	t := &LogTee{out: log.Writer()}
	log.SetOutput(t)
	return t
}

// Unwrap returns the writer under a LogTee, for output that must not reach the
// agent's stream.
func Unwrap(w io.Writer) io.Writer {
	if t, ok := w.(*LogTee); ok {
		return t.out
	}
	return w
}

func (t *LogTee) Write(p []byte) (int, error) {
	n, err := t.out.Write(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.detached {
		return n, err
	}
	t.partial = append(t.partial, p...)
	for {
		i := bytes.IndexByte(t.partial, '\n')
		if i < 0 {
			break
		}
		t.emit(string(bytes.TrimSuffix(t.partial[:i], []byte{'\r'})))
		t.partial = t.partial[i+1:]
	}
	return n, err
}

func (t *LogTee) emit(line string) {
	if t.sink != nil {
		t.sink(line)
		return
	}
	if len(t.buffered) < maxBufferedLines {
		t.buffered = append(t.buffered, line)
	}
}

// Attach flushes the buffered lines into sink and forwards later lines directly.
// sink must not block.
func (t *LogTee) Attach(sink func(string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, line := range t.buffered {
		sink(line)
	}
	t.buffered = nil
	t.sink = sink
}

// Uninstall restores the original logger output and drops anything not yet forwarded.
func (t *LogTee) Uninstall() {
	if log.Writer() == t {
		log.SetOutput(t.out)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.detached = true
	t.sink = nil
	t.buffered = nil
	t.partial = nil
}
