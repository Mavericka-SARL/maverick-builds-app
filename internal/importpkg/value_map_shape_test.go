package importpkg

import (
	"encoding/json"
	"strings"
	"testing"
)

// A flat value_map names its shape; the nested one reads as before.
func TestValueMapShape(t *testing.T) {
	var r Reshape
	err := json.Unmarshal([]byte(`{"value_map":{"SaaS commercial launch month":"saas_commercial_launch_month"}}`), &r)
	if err == nil || !strings.Contains(err.Error(), `{"Driver": {"SaaS commercial launch month": "saas_commercial_launch_month"`) {
		t.Fatalf("flat value_map: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"value_map":{"Driver":{"SaaS commercial launch month":"saas_commercial_launch_month"}}}`), &r); err != nil ||
		r.ValueMap["Driver"]["SaaS commercial launch month"] != "saas_commercial_launch_month" {
		t.Fatalf("nested value_map: %+v %v", r.ValueMap, err)
	}
}
