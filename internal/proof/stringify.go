package proof

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// stringify renders a plan output value the way Snap CD passes it downstream:
// scalars become their bare string form; composite values (list/map/object)
// become compact JSON. Matching this coercion matters — if the proof stringifies
// differently from the real deploy, validation could pass while deploy differs.
func stringify(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// JSON numbers decode as float64; render integers without a trailing .0.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		// list/map/object: compact JSON.
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}
