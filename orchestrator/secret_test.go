package function

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A Secret Manager client that could not be created used to be remembered as
// created: the first call returned the error, and every call after it
// dereferenced the nil client.
func TestSecretClientFailureIsReportedOnEveryCall(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentials)

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := getSecret(context.Background(), "gcrunner-webhook-secret"); err == nil {
			t.Fatalf("attempt %d: got a secret without credentials", attempt)
		}
	}
}
