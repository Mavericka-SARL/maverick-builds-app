package aiassistant

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// UnknownParamError is a step's param key its tool does not read. Decoded
// leniently, such a key was dropped without a word and the step did less than
// it said: create_grid's "metrics" built every grid empty, create_metric's
// "value": 9 left a setting blank, "scale" inside "unpivot" scaled nothing.
type UnknownParamError struct {
	Key   string
	Known []string // the top-level keys the tool reads
}

func (e *UnknownParamError) Error() string {
	return fmt.Sprintf("this tool does not read %q — its keys are %s; send only those (a key left out keeps its value or takes its default)",
		e.Key, strings.Join(e.Known, ", "))
}

// isUnknownParam reports an UnknownParamError.
func isUnknownParam(err error) bool {
	var u *UnknownParamError
	return errors.As(err, &u)
}

// decodeParams decodes a write step's params into p (a pointer to a struct),
// refusing any key the struct does not declare, at any depth its structs
// reach. "revision_id" is the exception: the session decides the revision,
// so one sent to a tool that has no use for it is dropped.
func decodeParams(raw json.RawMessage, p any) error {
	known := jsonKeys(p)
	if !containsString(known, "revision_id") {
		raw = withoutKey(raw, "revision_id")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(p)
	if err == nil {
		return nil
	}
	if key, ok := unknownField(err); ok {
		return &UnknownParamError{Key: key, Known: known}
	}
	return err
}

// unknownField reads the key out of encoding/json's DisallowUnknownFields
// error, `json: unknown field "x"`.
func unknownField(err error) (string, bool) {
	const prefix = `json: unknown field "`
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(msg, prefix), `"`), true
}

// withoutKey drops one top-level key from a JSON object; anything else is
// returned as given.
func withoutKey(raw json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	if _, ok := m[key]; !ok {
		return raw
	}
	delete(m, key)
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// jsonKeys lists the JSON keys of the struct p points to, sorted.
func jsonKeys(p any) []string {
	t := reflect.TypeOf(p)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	var keys []string
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if f.Anonymous && tag == "" {
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft)
				}
				continue
			}
			if !f.IsExported() || tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if name == "" {
				name = f.Name
			}
			keys = append(keys, name)
		}
	}
	walk(t)
	sort.Strings(keys)
	return keys
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
