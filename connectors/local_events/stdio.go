package local_events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const DefaultMaxStdioEventBytes = 1 << 20

var ErrStdioEventTooLarge = errors.New("stdio local event exceeds the configured limit")

// StdioDecoder decodes one strict Event JSON object per line. Line framing
// keeps memory bounded and lets plugins recover cleanly between events.
type StdioDecoder struct {
	MaxEventBytes int
}

func (d StdioDecoder) Decode(reader io.Reader, emit func(Event) error) error {
	if reader == nil {
		return errors.New("stdio local event reader is required")
	}
	if emit == nil {
		return errors.New("stdio local event callback is required")
	}
	limit := d.MaxEventBytes
	if limit == 0 {
		limit = DefaultMaxStdioEventBytes
	}
	if limit < 1 || limit > 1<<30 {
		return errors.New("stdio local event limit must be between 1 byte and 1 GiB")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), limit+1)
	line := 0
	for scanner.Scan() {
		line++
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		if len(data) > limit {
			return fmt.Errorf("line %d: %w", line, ErrStdioEventTooLarge)
		}
		var event Event
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decode stdio local event on line %d: %w", line, err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return fmt.Errorf("decode stdio local event on line %d: expected exactly one JSON object", line)
		}
		if err := event.Validate(); err != nil {
			return fmt.Errorf("validate stdio local event on line %d: %w", line, err)
		}
		if len(event.Data) == 0 {
			event.Data = json.RawMessage(`{}`)
		}
		if err := emit(event); err != nil {
			return fmt.Errorf("emit stdio local event on line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("%w: %v", ErrStdioEventTooLarge, err)
		}
		return err
	}
	return nil
}
