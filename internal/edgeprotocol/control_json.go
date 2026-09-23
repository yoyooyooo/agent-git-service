package edgeprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// encoding/json otherwise accepts repeated (including case-folded) struct
// keys. Reject those before a control envelope can be interpreted differently
// by a future adapter. Nesting and total bytes are independently bounded.
func uniqueControlFields(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return errors.New("control envelope is too deeply nested")
		}
		token, err := d.Token()
		if err != nil {
			return errors.New("invalid control JSON")
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				name, ok := token.(string)
				if err != nil || !ok {
					return errors.New("invalid control key")
				}
				name = strings.ToLower(name)
				if seen[name] {
					return errors.New("duplicate control key")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid control object")
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid control array")
			}
		default:
			return errors.New("invalid control delimiter")
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing control JSON")
	}
	return nil
}
