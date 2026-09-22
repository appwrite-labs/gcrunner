package function

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var githubClient = &http.Client{Timeout: 30 * time.Second}

// generateJITConfig creates a just-in-time runner configuration via the GitHub API.
// This replaces the need for config.sh on the VM — the returned blob contains all
// credentials and config needed to start the runner directly with ./run.sh --jitconfig.
func generateJITConfig(ctx context.Context, owner, repo, runnerName string, labels []string) (string, error) {
	installationToken, err := getInstallationToken(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("get installation token: %w", err)
	}

	body := struct {
		Name          string   `json:"name"`
		RunnerGroupID int      `json:"runner_group_id"`
		Labels        []string `json:"labels"`
		WorkFolder    string   `json:"work_folder"`
	}{
		Name:          runnerName,
		RunnerGroupID: 1,
		Labels:        labels,
		WorkFolder:    "_work",
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners/generate-jitconfig", owner, repo)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := githubClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.EncodedJITConfig, nil
}

// getRegistrationToken gets a runner registration token for the given repo.
func getRegistrationToken(ctx context.Context, owner, repo string) (string, error) {
	installationToken, err := getInstallationToken(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("get installation token: %w", err)
	}

	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners/registration-token", owner, repo)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := githubClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Token, nil
}

// getInstallationToken gets an installation access token for the GitHub App.
func getInstallationToken(ctx context.Context, owner string) (string, error) {
	appJWT, err := generateAppJWT(ctx)
	if err != nil {
		return "", fmt.Errorf("generate JWT: %w", err)
	}

	// First, find the installation ID for this owner
	installationID, err := getInstallationID(ctx, appJWT, owner)
	if err != nil {
		return "", fmt.Errorf("get installation ID: %w", err)
	}

	endpoint := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := githubClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Token, nil
}

// getInstallationID finds the installation ID for a given owner (org or user).
func getInstallationID(ctx context.Context, appJWT, owner string) (int64, error) {
	endpoint := fmt.Sprintf("https://api.github.com/users/%s/installation", owner)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := githubClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	return result.ID, nil
}

// generateAppJWT creates a JWT signed with the GitHub App's private key.
func generateAppJWT(ctx context.Context) (string, error) {
	appIDStr, err := loadSecret(ctx, "gcrunner-app-id")
	if err != nil {
		return "", fmt.Errorf("get app ID: %w", err)
	}

	keyPEM, err := loadSecret(ctx, "gcrunner-private-key")
	if err != nil {
		return "", fmt.Errorf("get private key: %w", err)
	}

	appID, err := strconv.ParseInt(strings.TrimSpace(appIDStr), 10, 64)
	if err != nil {
		return "", fmt.Errorf("parse app ID: %w", err)
	}

	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	return signJWT(appID, key)
}

func signJWT(appID int64, key *rsa.PrivateKey) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
		Issuer:    strconv.FormatInt(appID, 10),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(key)
}

// fetchRepositoryContents returns a file from the repository at ref, or from
// the default branch when ref is "", or nil when the repository has no such
// file.
func fetchRepositoryContents(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	installationToken, err := getInstallationToken(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("get installation token: %w", err)
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, path)
	if ref != "" {
		endpoint += "?" + url.Values{"ref": {ref}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	resp, err := githubClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return io.ReadAll(resp.Body)
	case http.StatusNotFound:
		return nil, nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return nil, &githubError{Status: resp.StatusCode, Body: string(respBody)}
}

// githubError is a non-2xx answer from the GitHub API.
type githubError struct {
	Status int
	Body   string
}

func (e *githubError) Error() string {
	return fmt.Sprintf("GitHub returned %d: %s", e.Status, e.Body)
}

// isForbidden reports whether GitHub refused the request for lack of a
// permission on the App installation.
func isForbidden(err error) bool {
	var ghErr *githubError
	return errors.As(err, &ghErr) && ghErr.Status == http.StatusForbidden
}

type runnerRecord struct {
	ID   int64 `json:"id"`
	Busy bool  `json:"busy"`
}

// findRunner returns the registered runner with this name, or nil.
func findRunner(ctx context.Context, owner, repo, name string) (*runnerRecord, error) {
	installationToken, err := getInstallationToken(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("get installation token: %w", err)
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners?name=%s", owner, repo, name)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := githubClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(respBody))
	}
	var result struct {
		Runners []runnerRecord `json:"runners"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Runners) == 0 {
		return nil, nil
	}
	return &result.Runners[0], nil
}

// runnerBusy reports whether the runner with this name is registered and running a job.
func runnerBusy(ctx context.Context, owner, repo, name string) (bool, error) {
	runner, err := findRunner(ctx, owner, repo, name)
	if err != nil {
		return false, err
	}
	return runner != nil && runner.Busy, nil
}

// removeIdleRunner deletes a registered runner with this name unless it is busy.
// A JIT registration outlives a failed VM creation, and GitHub refuses a second
// registration under the same name, so retries must clear it first.
func removeIdleRunner(ctx context.Context, owner, repo, name string) error {
	runner, err := findRunner(ctx, owner, repo, name)
	if err != nil || runner == nil {
		return err
	}
	if runner.Busy {
		return fmt.Errorf("runner %s is busy", name)
	}
	installationToken, err := getInstallationToken(ctx, owner)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners/%d", owner, repo, runner.ID)
	req, err := http.NewRequestWithContext(ctx, "DELETE", endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := githubClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(respBody))
	}
	log.Printf("Removed stale runner registration %s", name)
	return nil
}

// rerunWorkflowJob re-queues a job and its dependents as a new attempt of its
// run. GitHub refuses a rerun while the run is busy, when an earlier call
// already started it, and when the App lacks actions: write. Only a rerun
// that took is done, shown by this job being on a later attempt; anything
// else is an error so the task comes back later.
func rerunWorkflowJob(ctx context.Context, owner, repo string, job WorkflowJob) error {
	installationToken, err := getInstallationToken(ctx, owner)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/jobs/%d/rerun", owner, repo, job.ID)
	resp, err := githubRequest(ctx, "POST", endpoint, installationToken)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	refused := &githubError{Status: resp.StatusCode, Body: string(respBody)}
	attempt, err := latestAttempt(ctx, owner, repo, job, installationToken)
	if err != nil {
		return errors.Join(refused, err)
	}
	if attempt > job.RunAttempt {
		log.Printf("Job %d: rerun already started as attempt %d", job.ID, attempt)
		return nil
	}
	return refused
}

// latestAttempt returns the earliest attempt among the run's latest jobs of
// this job's name, or zero when there is none. A rerun gives the job a new
// id, so the name is the only link across attempts, and jobs can share one,
// so the rerun counts as started only once every job of that name has moved on.
func latestAttempt(ctx context.Context, owner, repo string, job WorkflowJob, installationToken string) (int, error) {
	const perPage = 100
	earliest := 0
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=%d&page=%d", owner, repo, job.RunID, perPage, page)
		resp, err := githubRequest(ctx, "GET", endpoint, installationToken)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return 0, &githubError{Status: resp.StatusCode, Body: string(respBody)}
		}
		var result struct {
			Jobs []WorkflowJob `json:"jobs"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return 0, fmt.Errorf("decode jobs of run %d: %w", job.RunID, err)
		}
		for _, candidate := range result.Jobs {
			if candidate.Name == job.Name && (earliest == 0 || candidate.RunAttempt < earliest) {
				earliest = candidate.RunAttempt
			}
		}
		if len(result.Jobs) < perPage {
			return earliest, nil
		}
	}
}

func githubRequest(ctx context.Context, method, endpoint, installationToken string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	return githubClient.Do(req)
}

// failRun reports a job that can never start: a failed check on its commit
// carrying the reason, then a cancel of the run so it does not wait a day.
func failRun(ctx context.Context, owner, repo string, job WorkflowJob, reason error) error {
	installationToken, err := getInstallationToken(ctx, owner)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}
	check, err := json.Marshal(map[string]any{
		"name":       "gcrunner",
		"head_sha":   job.HeadSHA,
		"status":     "completed",
		"conclusion": "failure",
		"output": map[string]string{
			"title":   job.Name + " cannot start",
			"summary": reason.Error() + ". Fix " + configPath + " or the runs-on label and re-run the workflow.",
		},
	})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/check-runs", owner, repo)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(check))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := githubClient.Do(req)
	if err := answered(resp, err, http.StatusCreated); err != nil {
		return fmt.Errorf("create check run: %w", err)
	}
	endpoint = fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%d/cancel", owner, repo, job.RunID)
	resp, err = githubRequest(ctx, "POST", endpoint, installationToken)
	if err := answered(resp, err, http.StatusAccepted, http.StatusConflict); err != nil {
		return fmt.Errorf("cancel run %d: %w", job.RunID, err)
	}
	return nil
}

// answered closes the response and returns a githubError unless its status is
// one of those given.
func answered(resp *http.Response, err error, statuses ...int) error {
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, status := range statuses {
		if resp.StatusCode == status {
			return nil
		}
	}
	respBody, _ := io.ReadAll(resp.Body)
	return &githubError{Status: resp.StatusCode, Body: string(respBody)}
}
