package integration

import (
	"fmt"
	"strconv"
	"strings"
)

// Response paths use a constrained JSONPath-like syntax — `$` root, `.name`
// object steps, `[N]` array index steps. No filters, wildcards, recursion,
// or scripting: a path is a pure lookup, statically validatable, with no
// execution semantics an attacker-controlled response could influence.
//
//	$.data.items      → obj["data"]["items"]
//	$.results[0].id   → obj["results"][0]["id"]
//	amount            → record["amount"] (leading `$.` optional for fields)

type pathStep struct {
	key   string
	index int  // used when isIdx
	isIdx bool
}

// ParsePath validates and compiles a path. maxDepth caps step count.
func ParsePath(raw string) ([]pathStep, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return nil, nil // root
	}
	var steps []pathStep
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return nil, fmt.Errorf("empty path segment in %q", raw)
		}
		// name, possibly followed by one or more [N]
		name := part
		for {
			idxStart := strings.Index(name, "[")
			if idxStart < 0 {
				if name != "" {
					steps = append(steps, pathStep{key: name})
				}
				break
			}
			if idxStart > 0 {
				steps = append(steps, pathStep{key: name[:idxStart]})
			}
			end := strings.Index(name[idxStart:], "]")
			if end < 0 {
				return nil, fmt.Errorf("unclosed [ in path %q", raw)
			}
			n, err := strconv.Atoi(name[idxStart+1 : idxStart+end])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("array index in %q must be a non-negative integer", raw)
			}
			steps = append(steps, pathStep{index: n, isIdx: true})
			name = name[idxStart+end+1:]
			if name == "" {
				break
			}
			if !strings.HasPrefix(name, "[") {
				return nil, fmt.Errorf("unexpected text after ] in path %q", raw)
			}
		}
		if len(steps) > 32 {
			return nil, fmt.Errorf("path %q exceeds 32 steps", raw)
		}
	}
	return steps, nil
}

// LookupPath walks a decoded JSON document. Missing steps return (nil, false).
func LookupPath(doc any, steps []pathStep) (any, bool) {
	cur := doc
	for _, st := range steps {
		if st.isIdx {
			arr, ok := cur.([]any)
			if !ok || st.index >= len(arr) {
				return nil, false
			}
			cur = arr[st.index]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[st.key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
