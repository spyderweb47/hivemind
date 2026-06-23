package runner

import (
	"bytes"
	"encoding/json"
)

// CaptureResult runs one real claude turn to completion and returns the
// assistant's final text. Unlike Send (fire-and-forget, output to a log), this is
// for synchronous planning where the reply is needed in-process — e.g. asking the
// supervisor to decompose an instruction into a routed task plan. Always uses the
// real ClaudeRunner (planning is meaningless under the fake).
func CaptureResult(spec PromptSpec) (string, error) {
	c := &ClaudeRunner{}
	if err := c.Available(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := c.Send(spec, &buf); err != nil {
		return "", err
	}
	// With --output-format json, claude prints one JSON object with a "result".
	var out struct {
		Result string `json:"result"`
	}
	trimmed := bytes.TrimSpace(buf.Bytes())
	if err := json.Unmarshal(trimmed, &out); err == nil && out.Result != "" {
		return out.Result, nil
	}
	return string(trimmed), nil // defensive fallback: raw output
}
