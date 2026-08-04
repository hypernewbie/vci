package cli

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRunWritesOneJSONResponse(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"unknown"}, &out, &errOut); code == 0 {
		t.Fatal("unknown command succeeded")
	}
	var got Response
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if got.OK || got.Error == nil || got.Error.Code != "unknown_command" {
		t.Fatalf("unexpected response: %+v", got)
	}
	if errOut.Len() != 0 {
		t.Fatalf("diagnostics contaminated stderr: %q", errOut.String())
	}
}
