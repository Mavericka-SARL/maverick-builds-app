package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The REST API accepts tokens only of the clients GATEWAY_TOKEN_CLIENTS
// lists, so a password grant through admin-cli was refused throughout a
// load test (2026-10-02): the grant now asks for the client -client names.
func TestPasswordGrantUsesTheNamedClient(t *testing.T) {
	var gotClient, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotClient, gotPath = r.PostForm.Get("client_id"), r.URL.Path
		_, _ = w.Write([]byte(`{"access_token":"tok"}`))
	}))
	defer srv.Close()
	c := config{keycloak: srv.URL, realm: "mavericks", username: "u", password: "p", tokenClient: "mavericks-loadtest"}
	tok, err := passwordGrant(context.Background(), srv.Client(), c)
	if err != nil || tok != "tok" {
		t.Fatalf("grant: %q, %v", tok, err)
	}
	if gotClient != "mavericks-loadtest" || gotPath != "/realms/mavericks/protocol/openid-connect/token" {
		t.Fatalf("asked %s for client %q, want mavericks-loadtest", gotPath, gotClient)
	}
}
