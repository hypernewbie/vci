package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// HostedFallback is the optional project-owned immutable source
// declaration for `vci build --hosted <project>`. The URL is
// restricted to https:// or ssh:// schemes with a required host and
// path; the commit is a full lowercase SHA-1 (40) or SHA-256 (64)
// object ID. Neither field is set or overridden by a client root;
// the snapshot records only the validated URL and the pinned commit.
type HostedFallback struct {
	URL    string `toml:"url" json:"url"`
	Commit string `toml:"commit" json:"commit"`
}

// ValidatedHosted returns a copy of the fallback with the URL and
// commit normalized and validated. The zero value is rejected with
// hosted_fallback_not_configured; absent fields produce
// hosted_fallback_invalid.
//
// The validator is the single source of truth for the URL and
// commit shape. The coordinator checkout path (Plan 12 Phase 1)
// consumes only the validated value and never re-parses raw TOML
// or CLI strings.
func (h HostedFallback) Validate() (ValidatedHosted, error) {
	if h.URL == "" && h.Commit == "" {
		return ValidatedHosted{}, ErrHostedFallbackNotConfigured
	}
	if h.URL == "" || h.Commit == "" {
		return ValidatedHosted{}, fmt.Errorf("%w: url and commit must be set together", ErrHostedFallbackInvalid)
	}
	url, err := validateHostedURL(h.URL)
	if err != nil {
		return ValidatedHosted{}, err
	}
	commit, err := validateHostedCommit(h.Commit)
	if err != nil {
		return ValidatedHosted{}, err
	}
	return ValidatedHosted{URL: url, Commit: commit}, nil
}

// ValidatedHosted is the immutable provenance snapshot for a hosted
// build. The URL is normalized to a lowercase scheme + host while
// preserving the original path and userinfo (where the user is
// permitted). The commit is the lowercase full hex object ID.
type ValidatedHosted struct {
	URL    string
	Commit string
}

// ErrHostedFallbackNotConfigured is the typed sentinel for a
// `build --hosted <project>` invocation against a project that
// does not declare the optional hosted fallback. Class
// configuration, retryable false.
var ErrHostedFallbackNotConfigured = fmt.Errorf("hosted fallback not configured")

// ErrHostedFallbackInvalid is the typed sentinel for a coordinator
// or setup attempt that produces a malformed URL or commit. Class
// configuration, retryable false.
var ErrHostedFallbackInvalid = fmt.Errorf("hosted fallback invalid")

// ErrHostedSourceUnavailable is the typed sentinel for a checkout
// that cannot reach the remote, fetch the pinned commit, or run
// git. Class infrastructure, retryable true.
var ErrHostedSourceUnavailable = fmt.Errorf("hosted source unavailable")

// ErrHostedSourceIntegrityFailed is the typed sentinel for a
// checkout whose actual HEAD does not match the pinned commit.
// Class infrastructure, retryable false.
var ErrHostedSourceIntegrityFailed = fmt.Errorf("hosted source integrity failed")

// hostedURLRe matches the canonical "https://host[:port]/path" or
// "ssh://[user@]host[:port]/path" shape after lowercasing the
// scheme and host. The path is required; the port and userinfo are
// optional. Whitespace, control characters, fragments, and queries
// are rejected at the pattern level. The exact userinfo alphabet
// excludes `:`, so a password-bearing userinfo is rejected by the
// pattern alone; an explicit pre-check reinforces that contract so
// a future regex change cannot regress it.
var hostedURLRe = regexp.MustCompile(`^(https|ssh)://([A-Za-z0-9._-]+@)?([A-Za-z0-9._-]+)(:[0-9]+)?(/[^?#\s]*)$`)

func validateHostedURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: url is empty", ErrHostedFallbackInvalid)
	}
	if strings.ContainsAny(raw, " \t\r\n\v\f\x00") {
		return "", fmt.Errorf("%w: url contains whitespace or control characters", ErrHostedFallbackInvalid)
	}
	if strings.HasPrefix(raw, "-") {
		return "", fmt.Errorf("%w: url starts with a flag-like character", ErrHostedFallbackInvalid)
	}
	// Reject scp-like syntax (e.g. user@host:path) which is not
	// supported by the constrained checkout.
	if strings.Contains(raw, "://") {
		// Has explicit scheme; further checks below.
	} else if strings.Contains(raw, ":") && !strings.HasPrefix(raw, "https:") && !strings.HasPrefix(raw, "ssh:") {
		return "", fmt.Errorf("%w: scp-style urls are not supported", ErrHostedFallbackInvalid)
	}
	// Explicit password rejection. The userinfo alphabet
	// `[A-Za-z0-9._-]+` cannot contain `:`, so a regex match
	// already guarantees no password — but this explicit check
	// keeps the contract honest so a future regex change cannot
	// regress it.
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		userinfo := raw[strings.Index(raw, "://")+3 : at]
		if strings.Contains(userinfo, ":") {
			return "", fmt.Errorf("%w: userinfo must not include a password", ErrHostedFallbackInvalid)
		}
	}
	m := hostedURLRe.FindStringSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("%w: url must be https://host/path or ssh://[user@]host/path", ErrHostedFallbackInvalid)
	}
	scheme := strings.ToLower(m[1])
	host := strings.ToLower(m[3])
	user := m[2]
	port := m[4]
	if user != "" {
		// userinfo is permitted on ssh:// only; reject any
		// non-user userinfo on https://.
		if scheme == "https" {
			return "", fmt.Errorf("%w: https url must not include userinfo", ErrHostedFallbackInvalid)
		}
		// Trim the trailing "@" for the normalized form.
		user = strings.TrimSuffix(user, "@")
	}
	// Validate port numeric range when present. The regex
	// guarantees digits, but the allowed TCP port range is
	// 1..65535.
	if port != "" {
		num, err := strconv.Atoi(strings.TrimPrefix(port, ":"))
		if err != nil || num < 1 || num > 65535 {
			return "", fmt.Errorf("%w: port must be a numeric 1..65535", ErrHostedFallbackInvalid)
		}
	}
	path := m[5]
	normalized := scheme + "://"
	if user != "" {
		normalized += user + "@"
	}
	normalized += host
	normalized += port
	normalized += path
	return normalized, nil
}

// hostedCommitRe matches a full lowercase hex object ID. SHA-1 is
// 40 hex chars; SHA-256 is 64 hex chars. Branch, tag, ref, and
// HEAD expressions are rejected.
var hostedCommitRe = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

func validateHostedCommit(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: commit is empty", ErrHostedFallbackInvalid)
	}
	if strings.ContainsAny(raw, " \t\r\n\v\f\x00") {
		return "", fmt.Errorf("%w: commit contains whitespace or control characters", ErrHostedFallbackInvalid)
	}
	if !hostedCommitRe.MatchString(raw) {
		return "", fmt.Errorf("%w: commit must be a full lowercase 40- or 64-char hex sha", ErrHostedFallbackInvalid)
	}
	return raw, nil
}
