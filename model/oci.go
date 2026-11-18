package model

type OCIManifest struct {
	Version string              `json:"version"`
	Modules []OCIManifestModule `json:"modules"`
	Images  []OCIManifestImage  `json:"images"`
}

type OCIManifestModule struct {
	Name       string                    `json:"name"`
	Type       string                    `json:"type"`
	Tag        string                    `json:"tag"`
	Registries map[string]OCIRegistryRef `json:"registries"`
}

type OCIManifestImage struct {
	Name       string                         `json:"name"`
	Tag        string                         `json:"tag"`
	Registries map[string]OCIImageRegistryRef `json:"registries"`
}

// OCIImageRegistryRef holds a multi-arch image's index digest; SBOMs are per platform.
type OCIImageRegistryRef struct {
	Repository string            `json:"repository"`
	Digest     string            `json:"digest"`
	Sbom       map[string]string `json:"sbom"`
}

type OCIRegistryRef struct {
	Repository string `json:"repository"`
	Package    string `json:"package"`
	Sbom       string `json:"sbom"`
}

// OCIImageResolver resolves the digest-pinned reference of a source's index
// image for a release, e.g. ghcr.io/entigolabs/entigo-infralib-release/index@sha256:...
type OCIImageResolver interface {
	GetImageReference(release string) (string, error)
}

// GetModule finds a module by its source path (type/name).
func (m *OCIManifest) GetModule(sourcePath string) (OCIManifestModule, bool) {
	for _, module := range m.Modules {
		if module.Type+"/"+module.Name == sourcePath {
			return module, true
		}
	}
	return OCIManifestModule{}, false
}
