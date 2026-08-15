package statemove

import (
	"encoding/json"
	"fmt"
	"os"
)

// StateAddresses reads the managed-resource addresses present in a local state
// file, in the same form Plan moves use ("type.name", plus "module.<name>" for
// any module-scoped resource so a whole-module move can be matched). Data
// sources carry no state moves and are excluded. A missing file yields an
// empty set.
func StateAddresses(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	var raw struct {
		Resources []struct {
			Module string `json:"module"`
			Mode   string `json:"mode"`
			Type   string `json:"type"`
			Name   string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	out := map[string]bool{}
	for _, r := range raw.Resources {
		if r.Mode == "data" {
			continue
		}
		if r.Module != "" {
			out[r.Module] = true
			continue
		}
		out[r.Type+"."+r.Name] = true
	}
	return out, nil
}
