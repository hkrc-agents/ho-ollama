//go:build harmonyos

package ui

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/app/store"
)

func TestHarmonyBuildChatRequestThink(t *testing.T) {
	tests := []struct {
		name     string
		think    any
		wantJSON string // Empty means Think must be nil and omitted from JSON.
	}{
		{name: "explicit_false", think: false, wantJSON: "false"},
		{name: "default_nil"},
		{name: "explicit_true", think: true, wantJSON: "true"},
		{name: "string_low", think: "low", wantJSON: `"low"`},
		{name: "string_medium", think: "medium", wantJSON: `"medium"`},
		{name: "string_high", think: "high", wantJSON: `"high"`},
		{name: "string_other", think: "custom", wantJSON: `"custom"`},
		{name: "string_empty", think: ""},
		{name: "string_none", think: "none"},
		{name: "unsupported_type", think: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			chat := &store.Chat{Messages: []store.Message{{Role: "user", Content: "hello"}}}
			req, err := s.buildChatRequest(chat, "test-model", tt.think, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantJSON == "" {
				if req.Think != nil {
					t.Fatalf("Think = %#v, want nil (model default)", req.Think)
				}
			} else if req.Think == nil {
				t.Fatalf("Think is nil, want explicit %s", tt.wantJSON)
			} else if req.Think.Value != tt.think {
				t.Fatalf("Think.Value = %#v, want %#v", req.Think.Value, tt.think)
			}
			encoded, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			value, present := fields["think"]
			if present != (tt.wantJSON != "") || string(value) != tt.wantJSON {
				t.Fatalf("serialized think = %s (present=%t), want %q; request: %s", value, present, tt.wantJSON, encoded)
			}
		})
	}
}
