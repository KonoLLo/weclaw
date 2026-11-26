package ilink

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// FlexibleID accepts message IDs encoded as either a JSON string or number.
type FlexibleID string

func (id *FlexibleID) UnmarshalJSON(data []byte) error {
	raw := bytes.TrimSpace(data)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*id = ""
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		*id = FlexibleID(s)
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return err
	}
	switch n := v.(type) {
	case json.Number:
		*id = FlexibleID(n.String())
		return nil
	case string:
		*id = FlexibleID(n)
		return nil
	default:
		return fmt.Errorf("message_id: unsupported JSON type %T", v)
	}
}

func (id FlexibleID) String() string {
	return string(id)
}
