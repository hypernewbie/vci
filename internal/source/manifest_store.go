package source

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func (s BlobStore) LoadManifest(digest string) (Manifest, error) {
	if digest == "" {
		return Manifest{}, fmt.Errorf("manifest digest is empty")
	}
	data, err := os.ReadFile(filepath.Join(s.Layout.ManifestsDir(), digest+".json"))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.Digest != digest {
		return Manifest{}, fmt.Errorf("manifest digest mismatch: %s", digest)
	}
	return manifest, nil
}
