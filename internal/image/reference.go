package image

import (
	"fmt"
	"regexp"
	"strings"
)

// Reference is a parsed OCI image reference.
type Reference struct {
	Registry string
	Repository string
	Tag string
	Digest string
}

// Parse parses an OCI image reference string.
// Supported formats:
//   - registry.example.com/repo:tag
//   - registry.example.com/repo@sha256:digest
//   - repo:tag
//   - repo@sha256:digest
//   - repo (defaults to latest tag)
func Parse(ref string) (*Reference, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty image reference")
	}

	r := &Reference{}

	// Split off registry if present (contains a dot or colon before first slash)
	rest := ref
	if strings.Contains(ref, "/") && (strings.ContainsAny(ref[:strings.Index(ref, "/")], ".:") || strings.Contains(ref[:strings.Index(ref, "/")], ":")) {
		parts := strings.SplitN(ref, "/", 2)
		r.Registry = parts[0]
		rest = parts[1]
	}

	// Split off digest if present
	if idx := strings.Index(rest, "@"); idx >= 0 {
		r.Digest = rest[idx+1:]
		rest = rest[:idx]
	}

	// Split off tag if present
	if idx := strings.LastIndex(rest, ":"); idx >= 0 {
		// Make sure the colon is not part of an IPv6 address in registry
		afterColon := rest[idx+1:]
		if !strings.Contains(afterColon, "/") {
			r.Tag = afterColon
			rest = rest[:idx]
		}
	}

	r.Repository = rest

	// Default tag
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}

	if err := r.validate(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Reference) validate() error {
	if r.Repository == "" {
		return fmt.Errorf("repository is required in reference: %s", r.String())
	}
	if r.Digest != "" && !strings.HasPrefix(r.Digest, "sha256:") {
		return fmt.Errorf("unsupported digest algorithm: %s", r.Digest)
	}
	return nil
}

// String returns the string representation of the reference.
func (r *Reference) String() string {
	var sb strings.Builder
	if r.Registry != "" {
		sb.WriteString(r.Registry)
		sb.WriteString("/")
	}
	sb.WriteString(r.Repository)
	if r.Digest != "" {
		sb.WriteString("@")
		sb.WriteString(r.Digest)
	} else {
		sb.WriteString(":")
		sb.WriteString(r.Tag)
	}
	return sb.String()
}

// RemoteName returns the fully-qualified remote name for registry access.
func (r *Reference) RemoteName() string {
	if r.Registry != "" {
		return r.Registry + "/" + r.Repository
	}
	return "docker.io/library/" + r.Repository
}

// Domain returns the registry domain.
func (r *Reference) Domain() string {
	if r.Registry != "" {
		return r.Registry
	}
	return "docker.io"
}

// canonicalTag returns the tag to use, defaulting to latest.
func (r *Reference) canonicalTag() string {
	if r.Tag == "" {
		return "latest"
	}
	return r.Tag
}

var validReference = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)

// IsValid checks if the reference string is well-formed.
func IsValid(ref string) bool {
	_, err := Parse(ref)
	return err == nil
}
