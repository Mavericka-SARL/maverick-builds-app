package modeltransfer

import (
	"encoding/json"
	"testing"
)

// A package never carries the model a link reads: export and import clear
// it, keep the rest of the link, and tell the importer it was a link.
func TestDetachModelLink(t *testing.T) {
	link := json.RawMessage(`{"kind":"rest_api/v1","protocol":"model","model":{"model_id":"22222222-2222-2222-2222-222222222222","grid":"Sales","metrics":["revenue"]},"target_id":"t"}`)
	out, isLink := detachModelLink(link)
	if !isLink {
		t.Fatal("a model link was not recognised")
	}
	var doc struct {
		Protocol string `json:"protocol"`
		TargetID string `json:"target_id"`
		Model    struct {
			ModelID string   `json:"model_id"`
			Grid    string   `json:"grid"`
			Metrics []string `json:"metrics"`
		} `json:"model"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Model.ModelID != "" || doc.Model.Grid != "Sales" || len(doc.Model.Metrics) != 1 || doc.Protocol != "model" || doc.TargetID != "t" {
		t.Fatalf("detached link: %s", out)
	}
	for _, other := range []string{`{"kind":"rest_api/v1","protocol":"sftp","model":{"model_id":"x"}}`, `{"column_map":{}}`, ``} {
		got, isLink := detachModelLink(json.RawMessage(other))
		if isLink || string(got) != other {
			t.Errorf("%q: changed to %q (link %v)", other, got, isLink)
		}
	}
}
