package flyio

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// captureTransport answers every request with an empty app list and keeps
// the URL the client asked for.
type captureTransport struct{ url string }

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.url = req.URL.String()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"apps":[]}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestDefaultBaseURL_ComposesWithVersionedPaths pins the seam between the
// server URL this package defaults to and the paths the generated client
// builds, through New the way production reaches it: NewClient appends a
// trailing slash to the server, and against that slash a "./v1/apps" path
// resolves under whatever the server already ends in. Fly's document puts
// /v1 on every operation path, so a default that still ended in /v1 would
// send every production request to /v1/v1/... while every fake-backed test,
// which sets its own base URL, stayed green.
func TestDefaultBaseURL_ComposesWithVersionedPaths(t *testing.T) {
	t.Parallel()
	ct := &captureTransport{}
	c, err := New("x", WithToken("t"), WithHTTPClient(&http.Client{Transport: ct}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListApps(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := "https://api.machines.dev/v1/apps?org_slug=x"; ct.url != want {
		t.Errorf("default request URL = %q, want %q", ct.url, want)
	}
}
