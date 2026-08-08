package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// HostedFallback is optional `vci build --hosted` metadata.
// It stores only the validated URL and pinned commit.
type HostedFallback struct {
	URL    string `toml:"url" json:"url"`
	Commit string `toml:"commit" json:"commit"`
}

// Validate returns the fallback URL and commit after normalization.
// Missing or invalid fields return hosted fallback validation errors.
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

// ValidatedHosted is the normalized immutable hosted snapshot.
// URL has lowercase scheme/host; commit is the validated lowercase object ID.
type ValidatedHosted struct {
	URL    string
	Commit string
}

// ErrHostedFallbackNotConfigured means no hosted fallback is configured.
var ErrHostedFallbackNotConfigured = fmt.Errorf("hosted fallback not configured")

// ErrHostedFallbackInvalid means the hosted URL or commit is malformed.
var ErrHostedFallbackInvalid = fmt.Errorf("hosted fallback invalid")

// ErrHostedSourceUnavailable means hosted checkout could not reach
// remote, fetch commit, or run git.
var ErrHostedSourceUnavailable = fmt.Errorf("hosted source unavailable")

// ErrHostedSourceIntegrityFailed means checkout HEAD mismatched
// the pinned commit.
var ErrHostedSourceIntegrityFailed = fmt.Errorf("hosted source integrity failed")

// hostedURLRe matches `https://` and `ssh://` URLs with required
// host+path and optional userinfo/port, rejecting query, fragment,
// whitespace, and control characters.
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
	// Reject scp-style user@host:path URLs; only explicit-scheme URLs pass.
	if !strings.Contains(raw, "://") && strings.Contains(raw, ":") && !strings.HasPrefix(raw, "https:") && !strings.HasPrefix(raw, "ssh:") {
		return "", fmt.Errorf("%w: scp-style urls are not supported", ErrHostedFallbackInvalid)
	}
	// Passwords in userinfo are disallowed; this check guards against
	// future regex drift.
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		// Only parse userinfo when @ follows a valid scheme delimiter.
		if schemeAt := strings.Index(raw, "://"); schemeAt >= 0 && at > schemeAt+3 {
			userinfo := raw[schemeAt+3 : at]
			if strings.Contains(userinfo, ":") {
				return "", fmt.Errorf("%w: userinfo must not include a password", ErrHostedFallbackInvalid)
			}
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
		// Userinfo is only allowed for ssh://.
		if scheme == "https" {
			return "", fmt.Errorf("%w: https url must not include userinfo", ErrHostedFallbackInvalid)
		}
		// Trim the trailing "@" for the normalized form.
		user = strings.TrimSuffix(user, "@")
	}
	// If present, validate TCP port range (1..65535).
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

// hostedCommitRe matches full lowercase SHA-1 (40) or SHA-256 (64)
// hexadecimal object IDs.
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

// ErrUnsupportedRuntime means the machine runtime is not supported.
// Supported values: "", "docker", "vm".
var ErrUnsupportedRuntime = fmt.Errorf("unsupported runtime")

// ErrRuntimeImageRequired means docker runtime needs an image.
var ErrRuntimeImageRequired = fmt.Errorf("runtime image required")

// ErrRuntimeImageInvalid means the image string fails validation.
var ErrRuntimeImageInvalid = fmt.Errorf("runtime image invalid")

// allowedRuntimeImage is a strict docker-image allow-list used
// for positional image arguments.
var allowedRuntimeImage = regexp.MustCompile(`^([A-Za-z0-9._-]+:[0-9]+/)?[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*(:[A-Za-z0-9._-]{1,128})?(@sha256:[0-9a-f]{64})?$`)

// ValidateMachineRuntime validates machine runtimes.
// empty: local host; docker requires image; vm requires snapshot.
func ValidateMachineRuntime(name string, m Machine) error {
	switch m.Runtime {
	case "":
		if m.Image != "" {
			return fmt.Errorf("machine %q has empty runtime but image %q", name, m.Image)
		}
		if m.Snapshot != "" {
			return fmt.Errorf("machine %q has empty runtime but snapshot %q", name, m.Snapshot)
		}
		return nil
	case "docker":
		if m.Image == "" {
			return fmt.Errorf("%w: machine %q runtime=docker requires image", ErrRuntimeImageRequired, name)
		}
		if m.Snapshot != "" {
			return fmt.Errorf("machine %q runtime=docker must not set snapshot", name)
		}
		if err := validateRuntimeImage(m.Image); err != nil {
			return fmt.Errorf("machine %q: %w", name, err)
		}
		return nil
	case "vm":
		if m.Image != "" {
			return fmt.Errorf("machine %q runtime=vm must not set image", name)
		}
		if m.Snapshot == "" {
			return fmt.Errorf("%w: machine %q runtime=vm requires snapshot", ErrRuntimeImageRequired, name)
		}
		if err := validateRuntimeImage(m.Snapshot); err != nil {
			return fmt.Errorf("machine %q snapshot: %w", name, err)
		}
		return nil
	default:
		return fmt.Errorf("%w: machine %q runtime=%q is not supported", ErrUnsupportedRuntime, name, m.Runtime)
	}
}

// validateRuntimeImage applies a strict allow-list for positional image args.
func validateRuntimeImage(image string) error {
	if image == "" {
		return fmt.Errorf("%w: image is empty", ErrRuntimeImageInvalid)
	}
	if strings.ContainsAny(image, " \t\r\n\v\f\x00") {
		return fmt.Errorf("%w: image contains whitespace or control characters", ErrRuntimeImageInvalid)
	}
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("%w: image starts with a flag-like character", ErrRuntimeImageInvalid)
	}
	if strings.Contains(image, "://") {
		return fmt.Errorf("%w: image must not use a scheme", ErrRuntimeImageInvalid)
	}
	if strings.HasPrefix(image, "/") || strings.HasPrefix(image, "./") || strings.Contains(image, "../") {
		return fmt.Errorf("%w: image must not be a path", ErrRuntimeImageInvalid)
	}
	if !allowedRuntimeImage.MatchString(image) {
		return fmt.Errorf("%w: image %q does not match the verbatim allow-list", ErrRuntimeImageInvalid, image)
	}
	return nil
}
