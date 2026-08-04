package oci_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// authRegistry is a registry that demands a bearer token obtained from its own
// token endpoint, which is how real registries authenticate.
//
// Nothing in the suite covered this before: the existing auth test only checked
// that a client could be *constructed* with credentials in the environment, so
// the challenge/token exchange and every credential-resolution branch were
// untested.
type authRegistry struct {
	mu sync.Mutex
	// wantUser and wantPass are the credentials the token endpoint accepts.
	wantUser, wantPass string
	// issuedToken is handed out on a successful exchange.
	issuedToken string
	// tokenRequests counts exchanges, so tests can prove one happened.
	tokenRequests int
	// authorized records requests that arrived with the right bearer token.
	authorized int
	// rejectToken makes the API reject the previously issued token, standing in
	// for one that expired mid-session.
	rejectToken bool

	server *httptest.Server
}

func newAuthRegistry(t *testing.T, user, pass string) *authRegistry {
	t.Helper()

	a := &authRegistry{wantUser: user, wantPass: pass, issuedToken: "issued-test-token"}
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.tokenRequests++
		user, pass, hasBasic := r.BasicAuth()
		ok := hasBasic && user == a.wantUser && pass == a.wantPass
		token := a.issuedToken
		a.mu.Unlock()

		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token, "access_token": token})
	})

	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		expected := "Bearer " + a.issuedToken
		reject := a.rejectToken
		a.mu.Unlock()

		if reject || r.Header.Get("Authorization") != expected {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="registry"`, a.server.URL))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
			return
		}

		a.mu.Lock()
		a.authorized++
		a.mu.Unlock()

		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/tags/list") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "test-repo", "tags": []string{}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	a.server = httptest.NewServer(mux)
	t.Cleanup(a.server.Close)
	return a
}

func (a *authRegistry) repoRef() string {
	return strings.TrimPrefix(a.server.URL, "http://") + "/test-repo"
}

func (a *authRegistry) counts() (tokenRequests, authorized int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokenRequests, a.authorized
}

// clearCredentialEnv removes every credential the client might otherwise pick
// up, so each test controls exactly one resolution branch.
func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"OCI_BEARER_TOKEN", "OCI_TOKEN", "OCI_USERNAME", "OCI_PASSWORD"} {
		t.Setenv(key, "")
	}
	// Point the Docker config somewhere empty so a developer's real
	// ~/.docker/config.json cannot make the test pass or fail.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
}

func TestAuthUsernamePasswordCompletesTokenExchange(t *testing.T) {
	clearCredentialEnv(t)
	reg := newAuthRegistry(t, "alice", "s3cret")
	t.Setenv("OCI_USERNAME", "alice")
	t.Setenv("OCI_PASSWORD", "s3cret")

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
		t.Fatalf("authenticated request failed: %v", err)
	}

	tokenRequests, authorized := reg.counts()
	if tokenRequests == 0 {
		t.Error("the client never performed a token exchange")
	}
	if authorized == 0 {
		t.Error("no request reached the API with a valid bearer token")
	}
}

func TestAuthWrongPasswordIsReportedWithItsSource(t *testing.T) {
	clearCredentialEnv(t)
	reg := newAuthRegistry(t, "alice", "s3cret")
	t.Setenv("OCI_USERNAME", "alice")
	t.Setenv("OCI_PASSWORD", "wrong")

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.EnumerateTagRefs(context.Background())
	if err == nil {
		t.Fatal("expected an authentication failure")
	}
	// A bare 401 does not say which of several credential sources was used, and
	// the resolution order is internal, so the user cannot tell from outside.
	if !strings.Contains(err.Error(), "OCI_USERNAME") {
		t.Errorf("error should name the credential source that was used, got: %v", err)
	}
}

func TestAuthAnonymousFailureSaysHowToAuthenticate(t *testing.T) {
	clearCredentialEnv(t)
	reg := newAuthRegistry(t, "alice", "s3cret")

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.EnumerateTagRefs(context.Background())
	if err == nil {
		t.Fatal("expected an authentication failure")
	}
	if !strings.Contains(err.Error(), "anonymously") {
		t.Errorf("error should say the request was anonymous, got: %v", err)
	}
	if !strings.Contains(err.Error(), "docker login") {
		t.Errorf("error should suggest how to authenticate, got: %v", err)
	}
}

func TestAuthBearerTokenFromEnvIsUsedDirectly(t *testing.T) {
	clearCredentialEnv(t)
	reg := newAuthRegistry(t, "unused", "unused")
	t.Setenv("OCI_BEARER_TOKEN", reg.issuedToken)

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
		t.Fatalf("bearer token from the environment was not accepted: %v", err)
	}

	tokenRequests, authorized := reg.counts()
	if tokenRequests != 0 {
		t.Errorf("a token already in hand should not trigger an exchange, saw %d", tokenRequests)
	}
	if authorized == 0 {
		t.Error("no request reached the API with the supplied token")
	}
}

// TestAuthTokenExpiringMidSessionIsReportedClearly covers a credential that
// worked and then stopped working.
//
// 401s are deliberately not retried, so a token that expires part-way through a
// long push surfaces as a bare HTTP error with no hint that the cause is the
// credential rather than the request.
func TestAuthTokenExpiringMidSessionIsReportedClearly(t *testing.T) {
	clearCredentialEnv(t)
	reg := newAuthRegistry(t, "unused", "unused")
	t.Setenv("OCI_BEARER_TOKEN", reg.issuedToken)

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()
	if _, err := client.EnumerateTagRefs(ctx); err != nil {
		t.Fatalf("first request should succeed: %v", err)
	}

	reg.mu.Lock()
	reg.rejectToken = true
	reg.mu.Unlock()

	_, err = client.EnumerateTagRefs(ctx)
	if err == nil {
		t.Fatal("expected the expired token to be rejected")
	}
	if !strings.Contains(err.Error(), "OCI_BEARER_TOKEN") {
		t.Errorf("error should name the credential that stopped working, got: %v", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error should raise expiry as a possibility, got: %v", err)
	}

	// A credential that worked and then stopped is a different problem from one
	// that was never right, and the old message offered three guesses — wrong,
	// expired, or lacking access — for a token that had demonstrably just
	// worked. It must now say which of those it is.
	if !strings.Contains(err.Error(), "accepted earlier") {
		t.Errorf("error should say the token had been working, got: %v", err)
	}
	// And a static token is the one credential nothing can renew, so the advice
	// has to be "reissue it" rather than "re-authenticate".
	if !strings.Contains(err.Error(), "reissue") {
		t.Errorf("error should say a static token must be reissued, got: %v", err)
	}
	if strings.Contains(err.Error(), "never accepted") {
		t.Errorf("error blames a credential that had just worked: %v", err)
	}
}

// TestAuthCredentialNeverAcceptedSaysSo is the other side of the same
// distinction: nothing succeeded, so expiry is not the explanation and
// suggesting it would send someone to reissue a token that was never valid.
func TestAuthCredentialNeverAcceptedSaysSo(t *testing.T) {
	clearCredentialEnv(t)
	reg := newAuthRegistry(t, "unused", "unused")
	t.Setenv("OCI_BEARER_TOKEN", "not-the-issued-token")

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.EnumerateTagRefs(context.Background())
	if err == nil {
		t.Fatal("expected the bad token to be rejected")
	}
	if !strings.Contains(err.Error(), "never accepted") {
		t.Errorf("error should say the credential never worked, got: %v", err)
	}
	if strings.Contains(err.Error(), "accepted earlier") {
		t.Errorf("error claims a credential worked when none did: %v", err)
	}
}

// TestAuthDockerConfigCredentialsAreUsed covers the credential-store branch,
// which had no coverage at all.
func TestAuthDockerConfigCredentialsAreUsed(t *testing.T) {
	clearCredentialEnv(t)
	reg := newAuthRegistry(t, "bob", "hunter2")

	// The registry host is the key docker config is looked up under.
	host := strings.TrimPrefix(reg.server.URL, "http://")
	dockerDir := t.TempDir()
	cfg := map[string]any{
		"auths": map[string]any{
			host: map[string]any{
				"auth": base64.StdEncoding.EncodeToString([]byte("bob:hunter2")),
			},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal docker config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dockerDir, "config.json"), raw, 0600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerDir)

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
		t.Fatalf("docker config credentials were not used: %v", err)
	}

	if _, authorized := reg.counts(); authorized == 0 {
		t.Error("no request reached the API authenticated from the docker config")
	}
}

// TestAuthEnvOverridesDockerConfig pins the documented resolution order.
func TestAuthEnvOverridesDockerConfig(t *testing.T) {
	clearCredentialEnv(t)
	reg := newAuthRegistry(t, "alice", "s3cret")

	host := strings.TrimPrefix(reg.server.URL, "http://")
	dockerDir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{
		"auths": map[string]any{
			host: map[string]any{"auth": base64.StdEncoding.EncodeToString([]byte("wrong:wrong"))},
		},
	})
	if err := os.WriteFile(filepath.Join(dockerDir, "config.json"), raw, 0600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerDir)
	t.Setenv("OCI_USERNAME", "alice")
	t.Setenv("OCI_PASSWORD", "s3cret")

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
		t.Fatalf("environment credentials should win over the docker config: %v", err)
	}
}
