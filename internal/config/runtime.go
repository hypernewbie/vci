package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ErrUnsupportedRuntime is the typed sentinel for a machine that
// declares a runtime value Vci does not yet execute. Class
// configuration, retryable false. The slice accepts only "" (bare
// host) and "docker"; future VM mode will be added in a follow-on
// plan.
var ErrUnsupportedRuntime = fmt.Errorf("unsupported runtime")

// ErrRuntimeImageRequired is the typed sentinel for a runtime that
// needs an image (docker) but none was supplied. Class
// configuration, retryable false.
var ErrRuntimeImageRequired = fmt.Errorf("runtime image required")

// ErrRuntimeImageInvalid is the typed sentinel for an image that
// fails the documented allow-list. The allow-list is strict on
// purpose: the value is passed straight to the system `docker`
// subprocess as a positional argument, so any shell metacharacter,
// whitespace, control char, or option-flag form is rejected.
// Class configuration, retryable false.
var ErrRuntimeImageInvalid = fmt.Errorf("runtime image invalid")

// allowedRuntimeImage matches the limited grammar the runtime
// runner accepts as a verbatim image reference: optional registry
// host with numeric port, repo path, optional tag, optional digest.
// No shell metacharacters, no whitespace, no control characters, no
// leading dash, no path separator, no scheme. The closure is
// deliberately strict because the value is interpreted by the
// system `docker` subprocess as a positional argument.
//
// The optional `host:port/` prefix accepts private registries such
// as `myregistry:5000/repo:tag` or
// `myregistry:5000/repo@sha256:...` without broadening the rule to
// allow shell metacharacters, paths, schemes, or flag-like values.
var allowedRuntimeImage = regexp.MustCompile(`^([A-Za-z0-9._-]+:[0-9]+/)?[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*(:[A-Za-z0-9._-]{1,128})?(@sha256:[0-9a-f]{64})?$`)

// ValidateMachineRuntime enforces the per-machine runtime rules:
//   - empty runtime ⇒ bare host; image and snapshot must be empty;
//   - docker runtime ⇒ image must be non-empty and pass the
//     allow-list; snapshot must be empty;
//   - vm runtime ⇒ snapshot must be non-empty and pass the
//     allow-list; image must be empty;
//   - any other runtime ⇒ ErrUnsupportedRuntime.
//
// The Machine's name is interpolated into the error so an operator
// can identify which inventory entry is broken.
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

// validateRuntimeImage enforces the documented allow-list for an
// image reference. The check is intentionally stricter than
// OCI/Docker's actual grammar because the value is forwarded to
// the system `docker` subprocess as a positional argument.
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
