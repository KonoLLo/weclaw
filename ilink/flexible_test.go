package ilink

import (
	"encoding/json"
	"testing"
)

func TestSendMessageResponseFlexibleID(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string", raw: `{"ret":0,"message_id":"abc-123"}`, want: "abc-123"},
		{name: "number", raw: `{"ret":0,"message_id":1234567890123456789}`, want: "1234567890123456789"},
		{name: "zero", raw: `{"ret":0,"message_id":0}`, want: "0"},
		{name: "null", raw: `{"ret":0,"message_id":null}`, want: ""},
		{name: "missing", raw: `{"ret":0}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp SendMessageResponse
			if err := json.Unmarshal([]byte(tc.raw), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := resp.MessageID.String(); got != tc.want {
				t.Fatalf("message_id = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSendMessageResponseRejectsCompositeID(t *testing.T) {
	var resp SendMessageResponse
	if err := json.Unmarshal([]byte(`{"ret":0,"message_id":{"id":1}}`), &resp); err == nil {
		t.Fatal("expected error for object message_id")
	}
}
