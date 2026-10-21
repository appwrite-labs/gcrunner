package function

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// useClock makes the installation token cache read the time from now.
func useClock(t *testing.T, now *time.Time) {
	t.Helper()
	installationTokens.nowFunc = func() time.Time { return *now }
	t.Cleanup(func() { installationTokens.nowFunc = time.Now })
}

func TestInstallationTokenIsReusedUntilShortlyBeforeItExpires(t *testing.T) {
	now := jobStart
	gh := &github{tokenExpiry: now.Add(time.Hour)}
	useGitHub(t, gh)
	useClock(t, &now)

	for name, c := range map[string]struct {
		after  time.Duration
		tokens int32
	}{
		"just issued":                   {0, 1},
		"half way through its life":     {30 * time.Minute, 1},
		"within five minutes of expiry": {56 * time.Minute, 2},
	} {
		t.Run(name, func(t *testing.T) {
			installationTokens.tokens = map[string]cachedToken{}
			gh.tokens.Store(0)
			now = jobStart
			if _, err := getInstallationToken(context.Background(), "appwrite-labs"); err != nil {
				t.Fatal(err)
			}
			now = jobStart.Add(c.after)
			token, err := getInstallationToken(context.Background(), "appwrite-labs")
			if err != nil || token != "ghs_test" {
				t.Fatalf("token=%q err=%v", token, err)
			}
			if got := gh.tokens.Load(); got != c.tokens {
				t.Errorf("GitHub issued %d tokens, want %d", got, c.tokens)
			}
		})
	}
}

func TestAFailedInstallationTokenIsRequestedAgain(t *testing.T) {
	now := jobStart
	gh := &github{tokenExpiry: now.Add(time.Hour), tokenStatus: http.StatusInternalServerError}
	useGitHub(t, gh)
	useClock(t, &now)

	if _, err := getInstallationToken(context.Background(), "appwrite-labs"); err == nil {
		t.Fatal("a failed token request should be returned")
	}
	gh.tokenStatus = 0
	if token, err := getInstallationToken(context.Background(), "appwrite-labs"); err != nil || token != "ghs_test" {
		t.Fatalf("after GitHub recovers: token=%q err=%v", token, err)
	}
	if got := gh.tokens.Load(); got != 2 {
		t.Errorf("GitHub was asked for %d tokens, want 2", got)
	}
}

func TestConcurrentJobsShareOneInstallationTokenRequest(t *testing.T) {
	gh := &github{tokenExpiry: time.Now().Add(time.Hour), tokenRelease: make(chan struct{})}
	useGitHub(t, gh)

	var wg sync.WaitGroup
	request := func(ctx context.Context) {
		defer wg.Done()
		if token, err := getInstallationToken(ctx, "appwrite-labs"); err != nil || token != "ghs_test" {
			t.Errorf("token=%q err=%v", token, err)
		}
	}

	// The first job to ask gives up while GitHub is still answering. It
	// returns at once, and the jobs waiting alongside it still get the token.
	first, cancel := context.WithCancel(context.Background())
	abandoned := make(chan error)
	go func() {
		_, err := getInstallationToken(first, "appwrite-labs")
		abandoned <- err
	}()
	for gh.tokens.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-abandoned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the job that gave up got err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the job that gave up kept waiting for GitHub")
	}
	for i := 0; i < 19; i++ {
		wg.Add(1)
		go request(context.Background())
	}
	time.Sleep(20 * time.Millisecond)
	close(gh.tokenRelease)
	wg.Wait()
	if got := gh.tokens.Load(); got != 1 {
		t.Errorf("twenty jobs asked GitHub for %d tokens, want one", got)
	}
}
