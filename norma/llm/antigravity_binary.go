package llm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	AgyDefaultBaseURL = "acp://antigravity-cli"
	AgyDefaultModel   = "gemini-4-carbon"

	// AgyOAuthClientID is Google's public installed-application OAuth client ID
	// used by `agy`'s consumer OAuth flow.
	AgyOAuthClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"

	agyManifestBaseURL   = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests"
	agyUpdateIntervalSec = 24 * 60 * 60
	agyTokenRelPath      = "antigravity-cli/antigravity-oauth-token"
)

// AgyCredential is the typed OAuth credential bundle staged for `agy acp`.
type AgyCredential struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	ExpiresMS int64  `json:"expires_ms"`
	Email     string `json:"email,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

// AgyCredentialEnv returns the environment overrides forcing `agy` to use
// file-backed auth instead of the OS keychain and disabling auto-updates.
func AgyCredentialEnv() []string {
	return []string{
		"SSH_TTY=artex-agy-file-auth",
		"AGY_CLI_DISABLE_AUTO_UPDATE=1",
	}
}

// AgyRootDir returns the isolated storage root for managed `agy` binaries and staged credentials.
func AgyRootDir() string {
	if custom := strings.TrimSpace(os.Getenv("ARTEX_AGY_HOME")); custom != "" {
		return custom
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".artex", "antigravity-cli")
}

// AgyGeminiDir returns the base directory for per-account `--gemini-dir` directories.
func AgyGeminiDir() string {
	return filepath.Join(AgyRootDir(), "gemini")
}

// AgyCredentialKey returns a stable, non-secret directory key isolating one Google account.
func AgyCredentialKey(cred AgyCredential) string {
	seed := strings.TrimSpace(cred.AccountID)
	if seed == "" {
		seed = strings.TrimSpace(cred.Email)
	}
	if seed == "" {
		seed = strings.TrimSpace(cred.Refresh)
	}
	if seed == "" {
		seed = strings.TrimSpace(cred.Access)
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:12])
}

// AgyCredentialVersion returns a non-secret fingerprint that changes when access or expiry rotates.
func AgyCredentialVersion(cred AgyCredential) string {
	h := sha256.New()
	h.Write([]byte(cred.Access))
	h.Write([]byte(":"))
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(cred.ExpiresMS))
	h.Write(b[:])
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// AgyPlatformKey returns the release manifest platform identifier for the host OS/arch.
func AgyPlatformKey() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "darwin_arm64", nil
	case "darwin/amd64":
		return "darwin_amd64", nil
	case "windows/amd64":
		return "windows_amd64", nil
	case "windows/arm64":
		return "windows_arm64", nil
	case "linux/amd64":
		if _, err := os.Stat("/lib/libc.musl-x86_64.so.1"); err == nil {
			return "linux_amd64_musl", nil
		}
		return "linux_amd64", nil
	case "linux/arm64":
		if _, err := os.Stat("/lib/libc.musl-aarch64.so.1"); err == nil {
			return "linux_arm64_musl", nil
		}
		return "linux_arm64", nil
	default:
		return "", fmt.Errorf("Antigravity CLI has no release for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// HasNCOnPath reports whether `nc` (netcat) is available on PATH.
func HasNCOnPath() bool {
	if _, err := exec.LookPath("nc"); err == nil {
		return true
	}
	if _, err := os.Stat("/usr/bin/nc"); err == nil {
		return true
	}
	return false
}

// AgyAuthURLParser incrementally scans `agy acp --login` output for the official Google OAuth URL.
type AgyAuthURLParser struct {
	pending string
}

// Push feeds a chunk of stdout/stderr output and returns the validated OAuth URL once complete.
func (p *AgyAuthURLParser) Push(chunk string, end bool) (string, error) {
	p.pending += chunk
	if len(p.pending) > 64*1024 {
		return "", errors.New("Antigravity CLI sign-in output exceeded its limit")
	}
	search := p.pending
	if end {
		search += "\n"
	}
	if idx := strings.Index(search, "https://accounts.google.com/"); idx >= 0 {
		rest := search[idx:]
		endIdx := strings.IndexFunc(rest, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '\x1b'
		})
		if endIdx > 0 {
			raw := rest[:endIdx]
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" {
				return "", errors.New("Antigravity CLI returned an invalid authorization URL")
			}
			if u.Query().Get("client_id") != AgyOAuthClientID {
				return "", errors.New("Antigravity CLI returned an unexpected OAuth client")
			}
			return u.String(), nil
		}
	}
	if nl := strings.LastIndexByte(p.pending, '\n'); nl >= 0 {
		p.pending = p.pending[nl+1:]
	}
	return "", nil
}

// AgyAuthorizationCode extracts an OAuth authorization code from a raw code or callback URL.
func AgyAuthorizationCode(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	code := trimmed
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		if u, err := url.Parse(trimmed); err == nil {
			code = strings.TrimSpace(u.Query().Get("code"))
		} else {
			code = ""
		}
	}
	if code == "" || strings.ContainsAny(code, "\r\n") {
		return "", errors.New("paste the authorization code or a callback URL containing a code")
	}
	return code, nil
}

// ParseAgyStoredCredentials parses `agy`'s consumer token JSON file.
func ParseAgyStoredCredentials(data []byte) (AgyCredential, error) {
	var raw struct {
		AuthMethod   string `json:"auth_method"`
		IDToken      string `json:"id_token"`
		IDTokenEmail string `json:"id_token_email"`
		Token        *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Expiry       string `json:"expiry"`
		} `json:"token"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return AgyCredential{}, errors.New("Antigravity CLI token file is not valid JSON")
	}
	if raw.AuthMethod != "consumer" {
		return AgyCredential{}, errors.New("Antigravity CLI requires a consumer OAuth login; sign in with Google again")
	}
	if raw.Token == nil {
		return AgyCredential{}, errors.New("Antigravity CLI did not export a token object")
	}
	access := strings.TrimSpace(raw.Token.AccessToken)
	refresh := strings.TrimSpace(raw.Token.RefreshToken)
	if access == "" {
		return AgyCredential{}, errors.New("Antigravity CLI did not export a valid access_token")
	}
	if refresh == "" {
		return AgyCredential{}, errors.New("Antigravity CLI did not export a refresh_token")
	}
	expTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.Token.Expiry))
	if err != nil {
		expTime, err = time.Parse(time.RFC3339, strings.TrimSpace(raw.Token.Expiry))
		if err != nil {
			return AgyCredential{}, errors.New("Antigravity CLI exported an invalid token expiry")
		}
	}
	jwtEmail, jwtSub := extractAgyJWTIdentity(raw.IDToken)
	email := jwtEmail
	if email == "" {
		email = strings.TrimSpace(raw.IDTokenEmail)
	}
	return AgyCredential{
		Access:    access,
		Refresh:   refresh,
		ExpiresMS: expTime.UnixMilli(),
		Email:     email,
		AccountID: jwtSub,
	}, nil
}

func extractAgyJWTIdentity(jwt string) (email, sub string) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", ""
	}
	return strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Sub)
}

// ExportAgyCredentials reads and parses the token file written by `agy acp --login`.
func ExportAgyCredentials(geminiDir string) (AgyCredential, error) {
	path := filepath.Join(geminiDir, agyTokenRelPath)
	if _, err := os.Stat(path); err != nil {
		found := findAgyOAuthTokenFile(geminiDir)
		if found == "" {
			return AgyCredential{}, errors.New("Antigravity CLI did not write a token file during sign-in")
		}
		path = found
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return AgyCredential{}, errors.New("Antigravity CLI did not write a token file during sign-in")
	}
	return ParseAgyStoredCredentials(data)
}

func findAgyOAuthTokenFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if found := findAgyOAuthTokenFile(p); found != "" {
				return found
			}
		} else if strings.HasSuffix(e.Name(), "-oauth-token") {
			return p
		}
	}
	return ""
}

// StageAgyCredentials writes an isolated `agy` consumer token file with 0600 permissions.
func StageAgyCredentials(cred AgyCredential, geminiDir string) error {
	file := filepath.Join(geminiDir, agyTokenRelPath)
	parent := filepath.Dir(file)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create agy credential dir: %w", err)
	}
	_ = os.Chmod(geminiDir, 0o700)
	_ = os.Chmod(parent, 0o700)

	expiry := time.UnixMilli(cred.ExpiresMS).UTC().Format(time.RFC3339)
	payload := map[string]any{
		"token": map[string]any{
			"access_token":  cred.Access,
			"token_type":    "Bearer",
			"refresh_token": cred.Refresh,
			"expiry":        expiry,
		},
		"auth_method":    "consumer",
		"id_token_email": cred.Email,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("serialize agy credential: %w", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", file, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write agy credential: %w", err)
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("stage agy credential: %w", err)
	}
	return nil
}

type agyReleaseManifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"`
}

type agyInstallRecord struct {
	Version   string `json:"version"`
	CheckedAt int64  `json:"checked_at"`
}

func agyReleasePath(binDir, version string) string {
	name := "agy"
	if runtime.GOOS == "windows" {
		name = "agy.exe"
	}
	return filepath.Join(binDir, version, name)
}

// ResolveAgyBinary returns the path to `agy`, honoring ARTEX_AGY_PATH or
// installing/reusing the SHA-512 verified release from storage.googleapis.com.
func ResolveAgyBinary(ctx context.Context, client *http.Client) (string, error) {
	if override := strings.TrimSpace(os.Getenv("ARTEX_AGY_PATH")); override != "" {
		return override, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 300 * time.Second}
	}
	binDir := filepath.Join(AgyRootDir(), "bin")
	if raw, err := os.ReadFile(filepath.Join(binDir, "install.json")); err == nil {
		var rec agyInstallRecord
		if json.Unmarshal(raw, &rec) == nil && rec.Version != "" {
			binary := agyReleasePath(binDir, rec.Version)
			if fi, err := os.Stat(binary); err == nil && !fi.IsDir() {
				if time.Now().Unix()-rec.CheckedAt >= agyUpdateIntervalSec {
					go func() {
						bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer cancel()
						_, _ = installLatestAgy(bgCtx, client, binDir)
					}()
				}
				return binary, nil
			}
		}
	}
	return installLatestAgy(ctx, client, binDir)
}

func installLatestAgy(ctx context.Context, client *http.Client, binDir string) (string, error) {
	platform, err := AgyPlatformKey()
	if err != nil {
		return "", err
	}
	manifestURL := fmt.Sprintf("%s/%s.json", agyManifestBaseURL, platform)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Antigravity CLI manifest request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Antigravity CLI manifest request failed: HTTP %d", resp.StatusCode)
	}
	var manifest agyReleaseManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest); err != nil {
		return "", fmt.Errorf("malformed Antigravity CLI release manifest: %w", err)
	}
	if manifest.Version == "" || strings.Contains(manifest.Version, "/") || strings.Contains(manifest.Version, "..") {
		return "", errors.New("malformed Antigravity CLI version in manifest")
	}
	parsedURL, err := url.Parse(manifest.URL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Hostname() != "storage.googleapis.com" {
		return "", errors.New("Antigravity CLI manifest did not reference Google's HTTPS release storage")
	}

	binary := agyReleasePath(binDir, manifest.Version)
	if fi, err := os.Stat(binary); err != nil || fi.IsDir() {
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			return "", fmt.Errorf("create agy release dir: %w", err)
		}
		dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
		if err != nil {
			return "", err
		}
		dlResp, err := client.Do(dlReq)
		if err != nil {
			return "", fmt.Errorf("Antigravity CLI download failed: %w", err)
		}
		defer dlResp.Body.Close()
		if dlResp.StatusCode < 200 || dlResp.StatusCode >= 300 {
			return "", fmt.Errorf("Antigravity CLI download failed: HTTP %d", dlResp.StatusCode)
		}
		payload, err := io.ReadAll(io.LimitReader(dlResp.Body, 512<<20))
		if err != nil {
			return "", fmt.Errorf("Antigravity CLI download body failed: %w", err)
		}
		sum := sha512.Sum512(payload)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(manifest.SHA512)) {
			return "", errors.New("Antigravity CLI SHA-512 verification failed")
		}

		staging := fmt.Sprintf("%s.tmp-%d", binary, time.Now().UnixNano())
		if strings.HasSuffix(parsedURL.Path, ".tar.gz") {
			if err := extractAgyFromTarGz(payload, staging); err != nil {
				return "", err
			}
		} else {
			if err := os.WriteFile(staging, payload, 0o755); err != nil {
				return "", fmt.Errorf("stage agy binary: %w", err)
			}
		}
		_ = os.Chmod(staging, 0o755)
		if err := os.Rename(staging, binary); err != nil {
			_ = os.Remove(staging)
			return "", fmt.Errorf("finalize agy binary: %w", err)
		}
	}

	rec := agyInstallRecord{Version: manifest.Version, CheckedAt: time.Now().Unix()}
	if data, err := json.Marshal(rec); err == nil {
		recTmp := filepath.Join(binDir, fmt.Sprintf("install.%d.tmp", time.Now().UnixNano()))
		if os.WriteFile(recTmp, data, 0o644) == nil {
			_ = os.Rename(recTmp, filepath.Join(binDir, "install.json"))
		}
	}
	return binary, nil
}

func extractAgyFromTarGz(archive []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("open agy tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read agy tar.gz: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if base == "antigravity" || base == "agy" || base == "antigravity.exe" || base == "agy.exe" {
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return fmt.Errorf("create staged agy binary: %w", err)
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				_ = os.Remove(dest)
				return fmt.Errorf("extract agy binary: %w", copyErr)
			}
			if closeErr != nil {
				_ = os.Remove(dest)
				return closeErr
			}
			return nil
		}
	}
	return errors.New("Antigravity CLI archive has no antigravity executable")
}
