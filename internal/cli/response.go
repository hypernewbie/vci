package cli

import (
	"encoding/json"
	"io"

	"github.com/hypernewbie/vci/internal/model"
)

type Response struct {
	SchemaVersion int             `json:"schema_version"`
	Command       string          `json:"command"`
	OK            bool            `json:"ok"`
	Data          any             `json:"data"`
	Error         *model.VciError `json:"error"`
}

func Success(command string, data any) Response {
	if data == nil {
		data = map[string]any{}
	}
	return Response{SchemaVersion: model.SchemaVersion, Command: command, OK: true, Data: data}
}

func Failure(command string, err *model.VciError) Response {
	return Response{SchemaVersion: model.SchemaVersion, Command: command, OK: false, Data: map[string]any{}, Error: err}
}

func Write(w io.Writer, response Response) error {
	return json.NewEncoder(w).Encode(response)
}
