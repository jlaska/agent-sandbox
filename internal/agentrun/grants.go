package agentrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Grants are the caller's authorization for repository network requests.
// They come from outside the repository: per-run --allow-host flags and a
// grants file passed by path. A grant only caps a request; a grant that no
// request asks for adds no egress.

// Grants file limits.
const (
	maxGrantsBytes = 256 << 10
	maxGrants      = 200
	maxGrantPaths  = 32
	maxGrantPath   = 256
)

// grantMethods is the set of methods a grant may list, in the order rules
// are emitted.
var grantMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// Read-only defaults for a grant that lists no methods or paths, and for
// every --allow-host grant.
var (
	defaultGrantMethods = []string{"GET", "HEAD"}
	defaultGrantPaths   = []string{"/**"}
)

// GrantsFile is a parsed, validated grants file (version 1).
//
//	version: 1
//	grants:
//	  - host: docs.example.org
//	    repos: [example/other-repo]  # optional; omitted = any repo
//	    methods: [GET, HEAD]         # optional; default [GET, HEAD]
//	    paths: ["/**"]               # optional; default ["/**"]
type GrantsFile struct {
	Version int     `yaml:"version"`
	Grants  []Grant `yaml:"grants"`
}

// Grant authorizes HTTPS (port 443, protocol rest) access to one host, for
// the listed repos (all repos when empty), methods and paths.
type Grant struct {
	Host    string   `yaml:"host"`
	Repos   []string `yaml:"repos"`
	Methods []string `yaml:"methods"`
	Paths   []string `yaml:"paths"`
	// Source says where the grant came from ("--allow-host",
	// "grants.yaml grants[2]"), for diagnostics.
	Source string `yaml:"-"`
}

// ParseGrants strictly decodes a grants file (the same YAML restrictions as
// ParseAgentPolicy) and returns its grants with defaults applied and Source
// set from source (usually the file path). Hosts are lowercased: the file is
// trusted, unlike the repo manifest, which must already be lowercase.
func ParseGrants(data []byte, source string) ([]Grant, error) {
	var f GrantsFile
	if err := decodeStrictYAML(data, maxGrantsBytes, map[string]bool{"version": true, "grants": true}, &f); err != nil {
		return nil, fmt.Errorf("grants file %s: %w", source, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("grants file %s: unsupported version %d (want version: 1)", source, f.Version)
	}
	if len(f.Grants) > maxGrants {
		return nil, fmt.Errorf("grants file %s: too many grants: %d (limit %d)", source, len(f.Grants), maxGrants)
	}
	var errs []error
	out := make([]Grant, 0, len(f.Grants))
	for i, g := range f.Grants {
		g.Source = fmt.Sprintf("%s grants[%d]", source, i)
		g.Host = strings.ToLower(g.Host)
		if len(g.Methods) == 0 {
			g.Methods = slices.Clone(defaultGrantMethods)
		}
		if len(g.Paths) == 0 {
			g.Paths = slices.Clone(defaultGrantPaths)
		}
		if err := g.validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, g)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("grants file %s: %w", source, err)
	}
	return out, nil
}

// LoadGrantsFile reads and parses the grants file at path.
func LoadGrantsFile(path string) ([]Grant, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("grants file: %w", err)
	}
	defer func() { _ = f.Close() }()
	// Read one byte past the limit so an oversized file is reported as such.
	data, err := io.ReadAll(io.LimitReader(f, maxGrantsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("grants file %s: %w", path, err)
	}
	return ParseGrants(data, path)
}

// AllowHostGrants turns --allow-host values into grants. Each is read-only
// (GET, HEAD) on every path. They name no repos: the flag applies to one run.
func AllowHostGrants(hosts []string) ([]Grant, error) {
	var errs []error
	out := make([]Grant, 0, len(hosts))
	for _, h := range hosts {
		g := Grant{
			Host:    strings.ToLower(h),
			Methods: slices.Clone(defaultGrantMethods),
			Paths:   slices.Clone(defaultGrantPaths),
			Source:  "--allow-host " + h,
		}
		if err := g.validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, g)
	}
	return out, errors.Join(errs...)
}

// validate checks one grant after defaults are applied. Universal host
// restrictions are checked separately (CompilePolicy), against the baseline.
func (g *Grant) validate() error {
	var errs []error
	if err := validateDNSName(g.Host); err != nil {
		errs = append(errs, err)
	}
	for _, r := range g.Repos {
		if _, _, err := ParseRepo(r); err != nil {
			errs = append(errs, fmt.Errorf("repos: %w", err))
		}
	}
	if len(g.Methods) == 0 {
		errs = append(errs, fmt.Errorf("methods: must not be empty"))
	}
	for _, m := range g.Methods {
		if !slices.Contains(grantMethods, m) {
			errs = append(errs, fmt.Errorf("methods: %q is not one of %s", m, strings.Join(grantMethods, ", ")))
		}
	}
	if hasDuplicates(g.Methods) {
		errs = append(errs, fmt.Errorf("methods: duplicate entries"))
	}
	if len(g.Paths) == 0 || len(g.Paths) > maxGrantPaths {
		errs = append(errs, fmt.Errorf("paths: need 1-%d entries", maxGrantPaths))
	}
	for _, p := range g.Paths {
		if err := validateGrantPath(p); err != nil {
			errs = append(errs, fmt.Errorf("paths: %w", err))
		}
	}
	if hasDuplicates(g.Paths) {
		errs = append(errs, fmt.Errorf("paths: duplicate entries"))
	}
	if err := errors.Join(errs...); err != nil {
		// A grants file entry is named by index, so add its host; an
		// --allow-host source already names it.
		where := g.Source
		if !strings.HasSuffix(strings.ToLower(where), " "+strings.ToLower(g.Host)) {
			where = fmt.Sprintf("%s (host %q)", where, g.Host)
		}
		return fmt.Errorf("%s: %w", where, err)
	}
	return nil
}

// grantPathPattern is an absolute URL path made of unreserved, sub-delim and
// percent characters plus the policy globs (* and **). No query or fragment.
var grantPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`)

func validateGrantPath(p string) error {
	if len(p) > maxGrantPath || !grantPathPattern.MatchString(p) {
		return fmt.Errorf("%q is not an absolute path (letters, digits, URL path characters and * globs; at most %d bytes)", p, maxGrantPath)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%q contains a dot segment", p)
		}
	}
	return nil
}

// appliesTo reports whether g covers repo (owner/name, case-insensitive like
// GitHub).
func (g *Grant) appliesTo(repo string) bool {
	if len(g.Repos) == 0 {
		return true
	}
	for _, r := range g.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

func hasDuplicates(vals []string) bool {
	seen := map[string]bool{}
	for _, v := range vals {
		if seen[v] {
			return true
		}
		seen[v] = true
	}
	return false
}
