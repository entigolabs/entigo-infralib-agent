package model

type OCIManifest struct {
	Version string              `json:"version"`
	Modules []OCIManifestModule `json:"modules"`
}

type OCIManifestModule struct {
	Name       string                    `json:"name"`
	Type       string                    `json:"type"`
	Tag        string                    `json:"tag"`
	Registries map[string]OCIRegistryRef `json:"registries"`
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
