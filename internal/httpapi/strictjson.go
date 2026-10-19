package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// errDuplicateKey is returned for a JSON object that names a key twice.
var errDuplicateKey = errors.New("duplicate key")

// checkNoDuplicateKeys walks the JSON document once and fails when any object
// repeats a key. Different parsers keep the first or the last of a repeated
// key, so a body that says two things is refused instead of being read one way
// here and another way by a proxy, a signature check or a log.
func checkNoDuplicateKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		keys  map[string]struct{} // nil for an array
		inKey bool                // the next string in an object is a key
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return nil // a syntax error is reported by the real decode
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{keys: map[string]struct{}{}, inKey: true})
			case '[':
				stack = append(stack, frame{})
			default: // } or ]
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1].keys != nil {
					stack[n-1].inKey = true // the value that just ended was a member's
				}
			}
			continue
		case string:
			if n := len(stack); n > 0 && stack[n-1].keys != nil && stack[n-1].inKey {
				if _, dup := stack[n-1].keys[t]; dup {
					return fmt.Errorf("%w %q", errDuplicateKey, t)
				}
				stack[n-1].keys[t] = struct{}{}
				stack[n-1].inKey = false
				continue
			}
		}
		// A scalar value ends a member of the enclosing object.
		if n := len(stack); n > 0 && stack[n-1].keys != nil {
			stack[n-1].inKey = true
		}
	}
}

// describeDecodeError turns a decoder error into a message in the API's terms:
// which field is wrong and how, without the Go type and package names the
// decoder uses.
func describeDecodeError(err error) string {
	var typeErr *json.UnmarshalTypeError
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return fmt.Sprintf("the field %q has the wrong type (expected %s)", typeErr.Field, jsonKind(typeErr.Type.String()))
		}
		return "the request body must be a JSON object"
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return "the request body is not valid JSON"
	case errors.Is(err, errDuplicateKey):
		return "the request body has a duplicate key (" + strings.TrimPrefix(err.Error(), "duplicate key ") + ")"
	}
	if rest, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return "unknown field " + rest
	}
	return "the request body is not valid for this endpoint"
}

// jsonKind names a Go kind the way JSON would.
func jsonKind(goType string) string {
	switch {
	case strings.HasPrefix(goType, "int"), strings.HasPrefix(goType, "uint"), strings.HasPrefix(goType, "float"):
		return "a number"
	case goType == "string":
		return "a string"
	case goType == "bool":
		return "true or false"
	case strings.HasPrefix(goType, "[]"):
		return "a list"
	case strings.HasPrefix(goType, "map["), strings.HasPrefix(goType, "struct"):
		return "an object"
	}
	return "another kind of value"
}
