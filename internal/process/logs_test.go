package process

import (
	"bytes"
	"testing"
)

func TestLimitWriterRetainsPrefixAndReportsTruncation(t *testing.T) {
	var out bytes.Buffer
	writer := New(&out, 3)
	if _, err := writer.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "abc" || !writer.Truncated() {
		t.Fatalf("out=%q truncated=%v bytes=%d", out.String(), writer.Truncated(), 3)
	}
}
