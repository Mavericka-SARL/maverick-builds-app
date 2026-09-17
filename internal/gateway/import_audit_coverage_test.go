// Import (/api/import/...) was entirely unaudited.
package gateway

import (
	"net/http"
	"testing"
)

func TestImportMutationsAreAudited(t *testing.T) {
	f := setupDevAuthoringFixture(t)

	status, body := f.do(t, "POST", "/api/import/upload", map[string]string{
		"csv": "FixtureRevenue\n100\n", "revision_id": f.revID, "import_mode": "incremental",
	})
	if status != http.StatusOK {
		t.Fatalf("import upload: status=%d body=%v", status, body)
	}
	jobID, _ := body["job_id"].(string)
	if jobID == "" {
		t.Fatalf("expected job_id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "import.uploaded"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("import.uploaded application_id = %v, want %s", row.applicationID, f.appID)
	}

	status, _ = f.do(t, "DELETE", "/api/import/jobs/"+jobID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete import job: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "import.job_deleted"); row.resourceID != jobID {
		t.Errorf("import.job_deleted resource_id = %s, want %s", row.resourceID, jobID)
	}
}
