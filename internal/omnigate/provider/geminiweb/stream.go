package geminiweb

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// webStream turns a StreamGenerate response body into provider.Events. The
// upstream streams a growing JSON array of wrb.fr frames whose text is
// cumulative, so every frame is diffed against what was already emitted and only
// the new tail is sent.
type webStream struct {
	rc io.ReadCloser
	br *bufio.Reader
	// buf holds bytes not yet consumed by the frame scanner (a frame may arrive
	// split across reads).
	buf string

	emittedText  string
	emittedThink string
	lastText     string
	convID       string
	done         bool
	readErr      error

	pending []provider.Event
	// unusable 保存「解析成功但产生不了事件」的帧原文（最多 MaxSample 字节），
	// 供整条流 0 帧时诊断。
	unusable []byte
}

func newWebStream(rc io.ReadCloser) *webStream {
	return &webStream{rc: rc, br: bufio.NewReaderSize(rc, 64*1024)}
}

// Close releases the upstream body.
func (s *webStream) Close() error { return s.rc.Close() }

// ConvID returns the upstream conversation id seen so far (empty if none).
func (s *webStream) ConvID() string { return s.convID }

// UpstreamSample implements provider.Sampler: a bounded sample of the frames
// that produced no usable event, read only when the stream yielded 0 frames.
func (s *webStream) UpstreamSample() []byte { return s.unusable }

// Recv returns the next normalized event, or io.EOF when the stream ends.
func (s *webStream) Recv() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			e := s.pending[0]
			s.pending = s.pending[1:]
			return e, nil
		}
		if s.done {
			return provider.Event{}, io.EOF
		}
		frames, consumed := scanCompleteWrbFrames(s.buf)
		if consumed > 0 {
			s.buf = s.buf[consumed:]
		}
		if len(frames) > 0 {
			s.handleFrames(frames)
			continue
		}
		if err := s.fill(); err != nil {
			s.finalize()
		}
	}
}

// fill reads one more chunk into the buffer. It returns a non-nil error only
// once the reader is exhausted (or failed) with nothing new to process.
func (s *webStream) fill() error {
	if s.readErr != nil {
		return s.readErr
	}
	buf := make([]byte, 32*1024)
	n, err := s.br.Read(buf)
	if n > 0 {
		s.buf += string(buf[:n])
	}
	if err != nil {
		s.readErr = err
		if n > 0 {
			return nil
		}
		return err
	}
	return nil
}

// handleFrames diffs each frame against what was already emitted.
func (s *webStream) handleFrames(frames []any) {
	for _, f := range frames {
		text, hasText, conv, thoughts := extractTextFromWrb(f)
		if conv != "" {
			s.convID = conv
		}
		if thoughts != "" && len(thoughts) > len(s.emittedThink) && strings.HasPrefix(thoughts, s.emittedThink) {
			delta := thoughts[len(s.emittedThink):]
			s.emittedThink = thoughts
			if delta != "" {
				s.pending = append(s.pending, provider.Event{Type: provider.EventReasoning, Text: delta})
			}
		}
		if !hasText {
			if thoughts == "" && conv == "" {
				s.noteFrame(f)
			}
			continue
		}
		s.lastText = text
		// Only append-only growth is emitted; a frame that is not a prefix
		// extension (reordering) is held and corrected by the tail flush.
		safe := stablePlaceholderPrefix(text)
		if len(safe) > len(s.emittedText) && strings.HasPrefix(safe, s.emittedText) {
			delta := safe[len(s.emittedText):]
			s.emittedText = safe
			if delta != "" {
				s.pending = append(s.pending, provider.Event{Type: provider.EventText, Text: delta})
			}
		}
	}
}

// finalize flushes any tail the diff held back and emits the terminal event.
func (s *webStream) finalize() {
	if s.done {
		return
	}
	s.done = true
	safe := stablePlaceholderPrefix(s.lastText)
	if len(safe) > len(s.emittedText) && strings.HasPrefix(safe, s.emittedText) {
		if delta := safe[len(s.emittedText):]; delta != "" {
			s.pending = append(s.pending, provider.Event{Type: provider.EventText, Text: delta})
		}
		s.emittedText = safe
	}
	if s.readErr != nil && s.readErr != io.EOF {
		s.pending = append(s.pending, provider.Event{Type: provider.EventError, Text: s.readErr.Error()})
		return
	}
	s.pending = append(s.pending, provider.Event{Type: provider.EventFinish, Finish: "stop"})
}

// noteFrame appends one raw frame to the unusable sample, capped at MaxSample.
func (s *webStream) noteFrame(v any) {
	if len(s.unusable) >= provider.MaxSample {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.unusable = append(s.unusable, b...)
	if len(s.unusable) < provider.MaxSample {
		s.unusable = append(s.unusable, '\n')
	}
	if len(s.unusable) > provider.MaxSample {
		s.unusable = s.unusable[:provider.MaxSample]
	}
}
