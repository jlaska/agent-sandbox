package openshell

import (
	"io/fs"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProviderProfilesNonEmpty(t *testing.T) {
	n := 0
	err := fs.WalkDir(ProviderProfiles, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		b, err := ProviderProfiles.ReadFile(path)
		if err != nil {
			return err
		}
		if len(b) == 0 {
			t.Errorf("embedded file %s is empty", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Errorf("expected at least 4 embedded profiles, got %d", n)
	}
}

func TestImagesYAML(t *testing.T) {
	if len(ImagesYAML) == 0 {
		t.Fatal("ImagesYAML is empty")
	}
	var doc struct {
		Images map[string]struct {
			Ref string `yaml:"ref"`
		} `yaml:"images"`
	}
	if err := yaml.Unmarshal(ImagesYAML, &doc); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"claude", "pi", "opencode"} {
		img, ok := doc.Images[h]
		if !ok {
			t.Errorf("harness %s missing from images.yaml", h)
			continue
		}
		re := regexp.MustCompile(`^ghcr\.io/jlaska/agent-` + h + `:\d+\.\d+\.\d+@sha256:[0-9a-f]{64}$`)
		if !re.MatchString(img.Ref) {
			t.Errorf("%s ref %q does not match expected format", h, img.Ref)
		}
	}
}
