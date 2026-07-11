package source

import (
	"bufio"
	"errors"
	"fmt"
)

const (
	// MaxTranscriptBytes protects discovery and parsing while allowing very long
	// real-world agent sessions.
	MaxTranscriptBytes int64 = 4 << 30
	maxRecordBytes           = 64 << 20
)

// ReadJSONLRecord reads one bounded newline-delimited record.
func ReadJSONLRecord(reader *bufio.Reader) ([]byte, error) {
	return readJSONLRecord(reader, maxRecordBytes)
}

func readJSONLRecord(reader *bufio.Reader, limit int) ([]byte, error) {
	var record []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(record)+len(fragment) > limit {
			return nil, fmt.Errorf("JSONL record exceeds %d bytes", limit)
		}
		record = append(record, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return record, err
	}
}
