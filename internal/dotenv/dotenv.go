// Package dotenv reads the env files demonolith writes beside carved roots
// for backend credentials. The files are shell-sourceable (`export KEY='v'`
// lines), and this parser accepts that shape as well as bare KEY=value.
package dotenv

import (
	"os"
	"strings"
)

// Load parses path into a key/value map. A missing file is an empty map.
// Lines are KEY=value with an optional `export ` prefix and optional single
// or double quoting around the value; blank lines and #-comments are ignored.
func Load(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.Index(line, "=")
		if eq < 1 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := line[eq+1:]
		if len(val) >= 2 {
			if val[0] == '\'' && val[len(val)-1] == '\'' {
				val = strings.ReplaceAll(val[1:len(val)-1], `'\''`, `'`)
			} else if val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	return out, nil
}

// Apply sets the variables into the process environment and returns a restore
// function reinstating the previous values. Module steps run sequentially, so
// scoping credentials to one step this way is race-free.
func Apply(env map[string]string) (restore func(), err error) {
	type prev struct {
		key, val string
		had      bool
	}
	var prevs []prev
	for k, v := range env {
		old, had := os.LookupEnv(k)
		prevs = append(prevs, prev{k, old, had})
		if err := os.Setenv(k, v); err != nil {
			return nil, err
		}
	}
	return func() {
		for _, p := range prevs {
			if p.had {
				_ = os.Setenv(p.key, p.val)
			} else {
				_ = os.Unsetenv(p.key)
			}
		}
	}, nil
}
