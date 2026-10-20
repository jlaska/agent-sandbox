// Package openshell embeds the OpenShell configuration shipped with agent-run:
// the pinned harness image refs, the sandbox policy template and the provider
// profiles.
package openshell

import "embed"

// ImagesYAML is the raw images.yaml mapping each harness to its pinned image ref.
//
//go:embed images.yaml
var ImagesYAML []byte

// SandboxPolicyTemplate is the raw sandbox-policy.yaml template that agent-run
// renders for each repository.
//
//go:embed sandbox-policy.yaml
var SandboxPolicyTemplate []byte

// ProviderProfiles holds the vendored and local provider profile YAML files.
//
//go:embed providers/*.yaml github-agent-profile.yaml litellm-inference-profile.yaml
var ProviderProfiles embed.FS
