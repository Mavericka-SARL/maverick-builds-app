package importpkg

// Private sheets, through a tenant's own Google service account (decided
// 2026-09-21: per tenant, not one deployment-wide account — every tenant's
// Google Cloud project is its own). The tenant admin pastes the account's
// JSON key once (stored through internal/secretbox); the sheet's owner
// shares the sheet with the account's e-mail address, as with any other
// collaborator; and from then on the fetch is the Sheets API, authenticated
// by a JWT bearer grant signed with the account's private key — the
// standard server-to-server flow, no consent screen, no refresh tokens.
//
// The result is the same CSV bytes the link-shared export produces, so
// everything after the fetch (RawRow → ResolveRows → CommitImport, the
// wizard's column mapping) is untouched by how the sheet was reached.

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ServiceAccount is the part of a Google service-account key file the
// fetch needs. TokenURL and APIBase exist for tests only; empty means
// Google's own.
type ServiceAccount struct {
	ClientEmail string
	PrivateKey  string // PEM, as in the key file's "private_key"
	ProjectID   string
	TokenURL    string
	APIBase     string
}

// ParseServiceAccountKey reads a service-account key file's JSON — the
// file Google Cloud's "Create key" downloads — keeping only what the fetch
// needs and checking that the private key actually parses, so a pasted
// fragment is refused at save time rather than at the first import.
func ParseServiceAccountKey(raw []byte) (ServiceAccount, error) {
	var f struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		ProjectID   string `json:"project_id"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return ServiceAccount{}, fmt.Errorf("not a service-account key file: %w", err)
	}
	if f.Type != "service_account" {
		return ServiceAccount{}, fmt.Errorf("not a service-account key file (type %q) — create one under IAM › Service accounts › Keys", f.Type)
	}
	if f.ClientEmail == "" || f.PrivateKey == "" {
		return ServiceAccount{}, errors.New("the key file has no client_email or private_key")
	}
	if _, err := parseRSAKey(f.PrivateKey); err != nil {
		return ServiceAccount{}, err
	}
	sa := ServiceAccount{ClientEmail: f.ClientEmail, PrivateKey: f.PrivateKey, ProjectID: f.ProjectID}
	if f.TokenURI != "" && f.TokenURI != "https://oauth2.googleapis.com/token" {
		// A key file names Google's token endpoint; anything else is not a
		// Google key, and would send the signed assertion elsewhere.
		return ServiceAccount{}, fmt.Errorf("the key file's token_uri is %q, not Google's", f.TokenURI)
	}
	return sa, nil
}

func parseRSAKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("the private_key is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("the private_key is not an RSA key")
	}
	if rk, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return rk, nil
	}
	return nil, errors.New("the private_key could not be parsed")
}

// AccessToken performs the JWT bearer grant: a short-lived assertion signed
// with the account's key, exchanged for an access token scoped to reading
// spreadsheets. Also what a "Test" button calls — a token proves the key.
func (sa ServiceAccount) AccessToken(ctx context.Context, client *http.Client) (string, error) {
	key, err := parseRSAKey(sa.PrivateKey)
	if err != nil {
		return "", err
	}
	tokenURL := sa.TokenURL
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/spreadsheets.readonly",
		"aud":   tokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(10 * time.Minute).Unix(),
	}
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign assertion: %w", err)
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("google token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google refused the service account (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" {
		return "", errors.New("google's token response carried no access_token")
	}
	return tok.AccessToken, nil
}

// FetchCSVWithServiceAccount reads the sheet through the Sheets API as the
// service account and returns it as CSV: the worksheet named by gid (the
// first one when gid is empty), every row, cells as the sheet shows them.
// A sheet not shared with the account wraps ErrSheetNotAccessible, naming
// the address to share it with.
func (f *SheetFetcher) FetchCSVWithServiceAccount(ctx context.Context, sa ServiceAccount, spreadsheetID, gid string) ([]byte, error) {
	if !sheetIDPattern.MatchString(spreadsheetID) {
		return nil, fmt.Errorf("invalid spreadsheet ID %q", spreadsheetID)
	}
	if gid != "" && !sheetGidPattern.MatchString(gid) {
		return nil, fmt.Errorf("invalid worksheet gid %q", gid)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	token, err := sa.AccessToken(ctx, client)
	if err != nil {
		return nil, err
	}
	apiBase := sa.APIBase
	if apiBase == "" {
		apiBase = "https://sheets.googleapis.com"
	}
	get := func(path string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("sheets api: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxSheetBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read sheets api response: %w", err)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			if len(body) > maxSheetBytes {
				return nil, fmt.Errorf("sheet exceeds the %d MB import limit", maxSheetBytes>>20)
			}
			return body, nil
		case http.StatusNotFound:
			return nil, fmt.Errorf("spreadsheet not found — check the URL: %w", ErrSheetNotAccessible)
		case http.StatusForbidden, http.StatusUnauthorized:
			return nil, fmt.Errorf("the sheet is not shared with this tenant's service account — in Google Sheets, Share it with %s (Viewer): %w", sa.ClientEmail, ErrSheetNotAccessible)
		default:
			return nil, fmt.Errorf("google returned HTTP %d from the sheets api", resp.StatusCode)
		}
	}

	// Which worksheet: the gid in the URL names a sheetId; values are
	// addressed by title.
	meta, err := get("/v4/spreadsheets/" + spreadsheetID + "?fields=sheets.properties(sheetId,title)")
	if err != nil {
		return nil, err
	}
	var m struct {
		Sheets []struct {
			Properties struct {
				SheetID json.Number `json:"sheetId"`
				Title   string      `json:"title"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := json.Unmarshal(meta, &m); err != nil || len(m.Sheets) == 0 {
		return nil, errors.New("the spreadsheet has no worksheets")
	}
	title := m.Sheets[0].Properties.Title
	if gid != "" {
		found := false
		for _, s := range m.Sheets {
			if s.Properties.SheetID.String() == gid {
				title, found = s.Properties.Title, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("the spreadsheet has no worksheet with gid %s: %w", gid, ErrSheetNotAccessible)
		}
	}
	// FORMATTED_VALUE: what the sheet shows, exactly as the CSV export
	// would carry it, so both paths classify columns the same way.
	values, err := get("/v4/spreadsheets/" + spreadsheetID + "/values/" + url.PathEscape("'"+strings.ReplaceAll(title, "'", "''")+"'") + "?valueRenderOption=FORMATTED_VALUE")
	if err != nil {
		return nil, err
	}
	var v struct {
		Values [][]any `json:"values"`
	}
	if err := json.Unmarshal(values, &v); err != nil {
		return nil, fmt.Errorf("sheets api values: %w", err)
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	width := 0
	for _, row := range v.Values {
		if len(row) > width {
			width = len(row)
		}
	}
	for _, row := range v.Values {
		rec := make([]string, width)
		for i, cell := range row {
			rec[i] = fmt.Sprint(cell)
		}
		if err := w.Write(rec); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}
