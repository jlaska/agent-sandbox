package agentrun

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jlaska/agent-sandbox/openshell"
)

// zeroDigest marks an image that has not been published yet (see images.yaml).
var zeroDigest = regexp.MustCompile(`@sha256:0{64}$`)

// imagesFile mirrors the structure of openshell/images.yaml.
type imagesFile struct {
	Images map[string]struct {
		Ref string `yaml:"ref"`
	} `yaml:"images"`
}

// resolveImage returns the image ref for a harness: the --image override if
// given, otherwise the digest-pinned ref embedded in images.yaml.
func resolveImage(h *Harness, override string) (string, error) {
	return resolveImageFrom(openshell.ImagesYAML, h, override)
}

// resolveImageFrom is resolveImage with the images.yaml content injected.
func resolveImageFrom(data []byte, h *Harness, override string) (string, error) {
	if override != "" {
		if !strings.Contains(override, "@sha256:") {
			fmt.Printf("[agent-run] WARNING: --image %s is not digest-pinned (no @sha256:); the tag may move.\n", override)
		}
		return override, nil
	}

	var f imagesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("failed to parse images.yaml: %w", err)
	}
	entry, ok := f.Images[h.ImageKey]
	if !ok || entry.Ref == "" {
		return "", fmt.Errorf("no image configured for %s in images.yaml", h.Name)
	}
	if zeroDigest.MatchString(entry.Ref) {
		return "", fmt.Errorf("image for %s not yet published; build locally (make images) and pass --image <ref>", h.Name)
	}
	return entry.Ref, nil
}
