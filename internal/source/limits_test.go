package source

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadJSONLRecordHandlesFragmentsAndEnforcesLimit(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader("1234567\nrest\n"), 4)
	record, err := readJSONLRecord(reader, 8)
	if err != nil || string(record) != "1234567\n" {
		t.Fatalf("bounded record = %q, %v", record, err)
	}
	if _, err := readJSONLRecord(bufio.NewReaderSize(strings.NewReader("12345678\n"), 4), 8); err == nil {
		t.Fatal("oversized JSONL record was accepted")
	}
}
