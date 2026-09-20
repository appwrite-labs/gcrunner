package function

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every webhook starts by reading the signing secret. When the Secret Manager
// client could not be created, the first webhook used to get a 500 and every
// one after it a panic, because the failed client was remembered as created.
func TestWebhooksKeepFailingCleanlyWithoutSecretManager(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentials)

	for attempt := 1; attempt <= 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		request.Header.Set("X-GitHub-Event", "workflow_job")
		response := httptest.NewRecorder()
		HandleWebhook(response, request)
		if response.Code != http.StatusInternalServerError {
			t.Errorf("webhook %d: status %d, want 500", attempt, response.Code)
		}
	}
}
