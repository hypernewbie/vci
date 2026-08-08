package config

import (
	"errors"
	"strings"
	"testing"
)

// TestHostedFallbackAcceptsValidURLCommit pins the supported URL
// and commit shapes. Each entry is a normalized (scheme, host,
// path, userinfo) and a 40- or 64-char lowercase hex commit.
func TestHostedFallbackAcceptsValidURLCommit(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		commit   string
		wantURL  string
		wantHost string
	}{
		{"https_basic", "https://example.com/owner/repo.git", "a7d8c9b6a7d8c9b6a7d8c9b6a7d8c9b6a7d8c9b6", "https://example.com/owner/repo.git", "example.com"},
		{"https_no_dot_git", "https://example.com/owner/repo", "1234567890abcdef1234567890abcdef12345678", "https://example.com/owner/repo", "example.com"},
		{"ssh_user", "ssh://git@example.com/owner/repo.git", "abcdef1234567890abcdef1234567890abcdef12", "ssh://git@example.com/owner/repo.git", "example.com"},
		{"sha256_commit", "https://example.com/o/r.git", "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890", "https://example.com/o/r.git", "example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := HostedFallback{URL: tc.url, Commit: tc.commit}
			v, err := fb.Validate()
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if v.URL != tc.wantURL {
				t.Fatalf("URL: %q want %q", v.URL, tc.wantURL)
			}
			if v.Commit != tc.commit {
				t.Fatalf("commit: %q", v.Commit)
			}
		})
	}
}

// TestHostedFallbackRejectsInvalidURLCommit pins the negative
// inputs: empty, partial, scp-like, query/fragment, password, file
// scheme, branch/tag/HEAD, mixed-case, wrong-length, non-hex.
func TestHostedFallbackRejectsInvalidURLCommit(t *testing.T) {
	cases := []struct {
		name        string
		url         string
		commit      string
		wantErrType error
	}{
		{"empty_pair", "", "", ErrHostedFallbackNotConfigured},
		{"partial_url_only", "https://example.com/o/r.git", "", ErrHostedFallbackInvalid},
		{"partial_commit_only", "", "abc123", ErrHostedFallbackInvalid},
		{"scp_user_host_path", "git@example.com:owner/repo.git", "abc1234567890abcdef1234567890abcdef12345678", ErrHostedFallbackInvalid},
		{"file_scheme", "file:///tmp/repo", "abc1234567890abcdef1234567890abcdef12345678", ErrHostedFallbackInvalid},
		{"query", "https://example.com/o/r.git?query=1", "abc1234567890abcdef1234567890abcdef12345678", ErrHostedFallbackInvalid},
		{"fragment", "https://example.com/o/r.git#frag", "abc1234567890abcdef1234567890abcdef12345678", ErrHostedFallbackInvalid},
		{"https_with_userinfo", "https://user@example.com/o/r.git", "abc1234567890abcdef1234567890abcdef12345678", ErrHostedFallbackInvalid},
		{"whitespace_url", "https://example.com/o/r.git\n", "abc1234567890abcdef1234567890abcdef12345678", ErrHostedFallbackInvalid},
		{"whitespace_commit", "https://example.com/o/r.git", "abc1234567890abcdef1234567890abcdef12345678\n", ErrHostedFallbackInvalid},
		{"flag_like_url", "-ssh://example.com/o/r.git", "abc1234567890abcdef1234567890abcdef12345678", ErrHostedFallbackInvalid},
		{"branch_commit", "https://example.com/o/r.git", "main", ErrHostedFallbackInvalid},
		{"tag_commit", "https://example.com/o/r.git", "v1.0.0", ErrHostedFallbackInvalid},
		{"head_commit", "https://example.com/o/r.git", "HEAD", ErrHostedFallbackInvalid},
		{"refspec_commit", "https://example.com/o/r.git", "refs/heads/main", ErrHostedFallbackInvalid},
		{"uppercase_commit", "https://example.com/o/r.git", "ABCDEF1234567890ABCDEF1234567890ABCDEF12", ErrHostedFallbackInvalid},
		{"short_commit", "https://example.com/o/r.git", "abcdef12", ErrHostedFallbackInvalid},
		{"long_commit", "https://example.com/o/r.git", "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890ab", ErrHostedFallbackInvalid},
		{"non_hex_commit", "https://example.com/o/r.git", "g234567890123456789012345678901234567890", ErrHostedFallbackInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := HostedFallback{URL: tc.url, Commit: tc.commit}.Validate()
			if err == nil {
				t.Fatalf("expected error for %q + %q", tc.url, tc.commit)
			}
			if !errors.Is(err, tc.wantErrType) {
				t.Fatalf("error %v does not wrap %v", err, tc.wantErrType)
			}
		})
	}
}

// TestHostedFallbackRejectsNoSchemeUserinfo pins that malformed
// no-scheme values containing "@" are rejected with
// ErrHostedFallbackInvalid instead of panicking. The password
// pre-check slices the userinfo between "://" and "@"; when no
// scheme separator is present (or the "@" precedes it) those slice
// bounds are invalid, so the guard must let these values fall
// through to the pattern check.
func TestHostedFallbackRejectsNoSchemeUserinfo(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"bare_user_at_host", "a@b"},
		{"user_at_host_with_path", "a@b/c"},
		{"git_style_no_scheme", "git@github.com"},
		{"at_before_scheme", "x@y://z"},
		{"bare_at", "@"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := HostedFallback{
				URL:    tc.url,
				Commit: "abc1234567890abcdef1234567890abcdef12345",
			}.Validate()
			if !errors.Is(err, ErrHostedFallbackInvalid) {
				t.Fatalf("url %q: want ErrHostedFallbackInvalid, got %v", tc.url, err)
			}
		})
	}
}

// TestHostedFallbackRejectsConfigMutationByClient pins that a
// client root cannot declare hosted_fallback. Client authority is
// the orchestrator selector; everything else is coordinator-owned.
func TestHostedFallbackRejectsConfigMutationByClient(t *testing.T) {
	body := `schema_version = 1
orchestrator = "builder"

[projects.Vci]
machines = ["mac-local"]
command = ["go", "test", "./..."]

[projects.Vci.hosted_fallback]
url = "https://example.com/o/r.git"
commit = "abc1234567890abcdef1234567890abcdef12345678"
`
	_, err := Decode([]byte(body))
	if err == nil {
		t.Fatal("client root must not declare hosted_fallback")
	}
}

// TestHostedFallbackPreservesOriginalPathCase pins that the
// validator does not alter the path. The host is lowercased and
// the scheme is lowercased, but the path is preserved verbatim.
func TestHostedFallbackPreservesOriginalPathCase(t *testing.T) {
	url := "https://Example.COM/Owner/Mixed-Case-Repo.git"
	v, err := HostedFallback{URL: url, Commit: "abc1234567890abcdef1234567890abcdef12345"}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.URL, "Mixed-Case-Repo.git") {
		t.Fatalf("URL: %q", v.URL)
	}
	if !strings.Contains(v.URL, "example.com") {
		t.Fatalf("host not lowercased: %q", v.URL)
	}
	if !strings.HasPrefix(v.URL, "https://") {
		t.Fatalf("scheme not lowercased: %q", v.URL)
	}
}

// TestHostedFallbackRejectsPasswordUserinfo pins that an HTTPS URL
// with `user:pass@` is rejected. The current regex rejects it
// only by accident (the userinfo class does not include `:`); the
// hardened validator must reject it explicitly so a future regex
// change cannot let it slip through.
func TestHostedFallbackRejectsPasswordUserinfo(t *testing.T) {
	_, err := HostedFallback{
		URL:    "https://user:pass@example.com/owner/repo.git",
		Commit: "abc1234567890abcdef1234567890abcdef12345",
	}.Validate()
	if !errors.Is(err, ErrHostedFallbackInvalid) {
		t.Fatalf("password userinfo must be rejected: %v", err)
	}
}

// TestHostedFallbackRejectsNonNumericPort pins that a port segment
// with non-numeric characters is rejected. The current regex
// accepts `[A-Za-z0-9._:-]+` for the host portion, which leaks
// `host:abc` as a valid host.
func TestHostedFallbackRejectsNonNumericPort(t *testing.T) {
	_, err := HostedFallback{
		URL:    "https://example.com:abc/owner/repo.git",
		Commit: "abc1234567890abcdef1234567890abcdef12345",
	}.Validate()
	if !errors.Is(err, ErrHostedFallbackInvalid) {
		t.Fatalf("non-numeric port must be rejected: %v", err)
	}
}

// TestHostedFallbackRejectsHostWithoutPath pins that an HTTPS URL
// with no path is rejected. The current regex's `/[^?#\s]*` group
// is optional, so `https://example.com` is accepted.
func TestHostedFallbackRejectsHostWithoutPath(t *testing.T) {
	_, err := HostedFallback{
		URL:    "https://example.com",
		Commit: "abc1234567890abcdef1234567890abcdef12345",
	}.Validate()
	if !errors.Is(err, ErrHostedFallbackInvalid) {
		t.Fatalf("host without path must be rejected: %v", err)
	}
}

// TestHostedFallbackRejectsOutOfRangePort pins that a port greater
// than 65535 is rejected. The current regex accepts any number
// embedded in the host portion.
func TestHostedFallbackRejectsOutOfRangePort(t *testing.T) {
	_, err := HostedFallback{
		URL:    "https://example.com:99999/owner/repo.git",
		Commit: "abc1234567890abcdef1234567890abcdef12345",
	}.Validate()
	if !errors.Is(err, ErrHostedFallbackInvalid) {
		t.Fatalf("out-of-range port must be rejected: %v", err)
	}
}
