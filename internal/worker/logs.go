package worker

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	"foundry.dev/foundry/internal/builder"
	"foundry.dev/foundry/internal/store"
)

// logSink batches build log lines into the database (COPY) and wakes SSE
// listeners through NOTIFY. Lines are redacted before they leave the host.
type logSink struct {
	db      *store.DB
	buildID string
	stream  string

	mu      sync.Mutex
	partial []byte
	pending []string
	stop    chan struct{}
	done    chan struct{}
}

func newLogSink(db *store.DB, buildID, stream string) *logSink {
	s := &logSink{db: db, buildID: buildID, stream: stream, stop: make(chan struct{}), done: make(chan struct{})}
	go s.loop()
	return s
}

// Write implements io.Writer (line oriented; 64 KiB max per line).
func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			if len(s.partial) > 64<<10 {
				s.pending = append(s.pending, builder.Redact(string(s.partial)))
				s.partial = s.partial[:0]
			}
			break
		}
		line := strings.TrimRight(string(s.partial[:i]), "\r")
		s.pending = append(s.pending, builder.Redact(line))
		s.partial = s.partial[i+1:]
	}
	return len(p), nil
}

// Printf-style helper for stage messages.
func (s *logSink) Line(msg string) { s.Write([]byte(msg + "\n")) }

func (s *logSink) flush() {
	s.mu.Lock()
	lines := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.db.AppendLogs(ctx, s.buildID, s.stream, lines); err == nil {
		s.db.Notify(ctx, "foundry_build", s.buildID)
	}
}

func (s *logSink) loop() {
	defer close(s.done)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			s.mu.Lock()
			if len(s.partial) > 0 {
				s.pending = append(s.pending, builder.Redact(string(s.partial)))
				s.partial = nil
			}
			s.mu.Unlock()
			s.flush()
			return
		case <-t.C:
			s.flush()
		}
	}
}

// Close flushes remaining lines.
func (s *logSink) Close() error {
	close(s.stop)
	<-s.done
	return nil
}
