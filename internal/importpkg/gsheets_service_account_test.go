package importpkg

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// A fake Google: the token endpoint verifies the JWT bearer assertion
// against the account's public key, the Sheets API answers metadata and
// values for one spreadsheet shared with the account only.
func fakeGoogle(t *testing.T, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
			return
		}
		tok, err := jwt.Parse(r.Form.Get("assertion"), func(*jwt.Token) (any, error) { return pub, nil }, jwt.WithValidMethods([]string{"RS256"}))
		if err != nil || !tok.Valid {
			http.Error(w, `{"error":"invalid_grant","error_description":"bad signature"}`, 400)
			return
		}
		claims := tok.Claims.(jwt.MapClaims)
		if claims["iss"] != "robot@proj.iam.gserviceaccount.com" || claims["scope"] != "https://www.googleapis.com/auth/spreadsheets.readonly" {
			http.Error(w, `{"error":"invalid_grant","error_description":"claims"}`, 400)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ya29.fake","expires_in":3600,"token_type":"Bearer"}`))
	})
	mux.HandleFunc("/v4/spreadsheets/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.fake" {
			http.Error(w, `{"error":{"code":401}}`, 401)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/private-but-not-shared-xxxxxxxx"):
			http.Error(w, `{"error":{"code":403,"message":"The caller does not have permission"}}`, 403)
		case strings.HasSuffix(r.URL.Path, "/shared-with-robot-xxxxxxxxxxxx"):
			_, _ = w.Write([]byte(`{"sheets":[{"properties":{"sheetId":0,"title":"Plan"}},{"properties":{"sheetId":1234567,"title":"Q2 'raw'"}}]}`))
		case strings.Contains(r.URL.Path, "/values/"):
			title, _ := jsonUnescapePath(r.URL.Path)
			if strings.Contains(title, "Q2") {
				_, _ = w.Write([]byte(`{"values":[["region","amount"],["APAC","1,200"],["EU"]]}`))
			} else {
				_, _ = w.Write([]byte(`{"values":[["a","b"],["1","2"]]}`))
			}
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func jsonUnescapePath(p string) (string, error) {
	i := strings.LastIndex(p, "/values/")
	return p[i+len("/values/"):], nil
}

func testServiceAccount(t *testing.T) (ServiceAccount, *rsa.PublicKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	file, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "proj", "client_email": "robot@proj.iam.gserviceaccount.com",
		"private_key": pemText, "token_uri": "https://oauth2.googleapis.com/token",
	})
	sa, err := ParseServiceAccountKey(file)
	if err != nil {
		t.Fatal(err)
	}
	return sa, &key.PublicKey, file
}

func TestServiceAccountKeyParsing(t *testing.T) {
	_, _, file := testServiceAccount(t)
	if _, err := ParseServiceAccountKey([]byte(`{"type":"authorized_user"}`)); err == nil || !strings.Contains(err.Error(), "not a service-account") {
		t.Fatalf("user credentials accepted: %v", err)
	}
	var parsed map[string]string
	_ = json.Unmarshal(file, &parsed)
	parsed["private_key"] = "-----BEGIN PRIVATE KEY-----\nbm90IGEga2V5\n-----END PRIVATE KEY-----\n"
	bad, _ := json.Marshal(parsed)
	if _, err := ParseServiceAccountKey(bad); err == nil {
		t.Fatal("a key file with an unparseable private key was accepted")
	}
	foreign := strings.Replace(string(file), "https://oauth2.googleapis.com/token", "https://evil.example/token", 1)
	if _, err := ParseServiceAccountKey([]byte(foreign)); err == nil || !strings.Contains(err.Error(), "token_uri") {
		t.Fatalf("a foreign token endpoint was accepted: %v", err)
	}
}

func TestFetchCSVWithServiceAccount(t *testing.T) {
	sa, pub, _ := testServiceAccount(t)
	google := fakeGoogle(t, pub)
	sa.TokenURL = google.URL + "/token"
	sa.APIBase = google.URL
	f := &SheetFetcher{}
	ctx := context.Background()

	t.Run("first worksheet by default, CSV as the export would give it", func(t *testing.T) {
		out, err := f.FetchCSVWithServiceAccount(ctx, sa, "shared-with-robot-xxxxxxxxxxxx", "")
		if err != nil || string(out) != "a,b\n1,2\n" {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("gid selects the worksheet; ragged rows are padded; quotes in titles survive", func(t *testing.T) {
		out, err := f.FetchCSVWithServiceAccount(ctx, sa, "shared-with-robot-xxxxxxxxxxxx", "1234567")
		if err != nil || string(out) != "region,amount\nAPAC,\"1,200\"\nEU,\n" {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("unknown gid is the user's problem", func(t *testing.T) {
		if _, err := f.FetchCSVWithServiceAccount(ctx, sa, "shared-with-robot-xxxxxxxxxxxx", "99"); err == nil || !strings.Contains(err.Error(), "no worksheet with gid") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("not shared with the account names the address to share with", func(t *testing.T) {
		_, err := f.FetchCSVWithServiceAccount(ctx, sa, "private-but-not-shared-xxxxxxxx", "")
		if err == nil || !strings.Contains(err.Error(), "robot@proj.iam.gserviceaccount.com") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("a wrong key is refused by the token endpoint", func(t *testing.T) {
		other, _, _ := testServiceAccount(t)
		other.TokenURL, other.APIBase = sa.TokenURL, sa.APIBase
		if _, err := f.FetchCSVWithServiceAccount(ctx, other, "shared-with-robot-xxxxxxxxxxxx", ""); err == nil || !strings.Contains(err.Error(), "refused the service account") {
			t.Fatalf("err=%v", err)
		}
	})
}
