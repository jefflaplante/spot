package main

import (
	"encoding/json"
	"os"
)

// emit dispatches output between machine-readable JSON and human-readable text
// based on the global --json flag.
//
// When jsonOutput is set, v is encoded as JSON (one object/array per line) and
// the text callback is skipped. Otherwise, the text callback runs and v is
// ignored. The text callback is responsible for any tabwriter flushing it
// needs; emit returns its error, if any.
//
// Callers should construct v as a small typed struct (or slice) so the JSON
// shape is stable. Use json struct tags to control field names.
func emit(v any, text func() error) error {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}
	if text == nil {
		return nil
	}
	return text()
}
