// .ragota.yaml — the optional file with which a repository describes itself
// to the indexer.
//
//	services:
//	  - name: orders
//	    root: services/orders
//	ignore:
//	  - "**/generated/**"
//
// The two sections are independent: a manifest may declare services, ignore
// patterns, or both, and one section being absent says nothing about the other.
//
// The file is content of the indexed repository, so it is trusted only to
// *narrow* what is indexed. Its ignore patterns are added to the server's and
// never subtracted from them — a repository cannot re-enable a path the
// operator excluded.
package repos

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestNames are the accepted file names, in the order they are tried.
var ManifestNames = []string{".ragota.yaml", ".ragota.yml"}

// ManifestService is a service the repository declares explicitly, overriding whatever
// the layout heuristics would have found.
type ManifestService struct {
	Name string `yaml:"name"`
	Root string `yaml:"root"` // repo-relative, "" for the repository root
}

// Manifest is a parsed .ragota.yaml.
type Manifest struct {
	Services []ManifestService `yaml:"services"`

	// Ignore holds glob patterns excluded from indexing, in the syntax of the
	// server's repos.ignore. Prefer the "**/dir/**" form: "dir/**" anchors at
	// the repository root and matches by string prefix, so it reaches neither
	// a nested node_modules nor only it.
	Ignore []string `yaml:"ignore"`

	// File is the name the manifest was read from, for diagnostics.
	File string `yaml:"-"`
}

// LoadManifest reads the repository's manifest. A repository without one is not an
// error and yields (nil, nil). A malformed one is an error rather than a
// silent fallback: the file now decides what gets indexed, and a typo that
// quietly dropped its ignore patterns would index the tree it excludes.
func LoadManifest(repoPath string) (*Manifest, error) {
	for _, name := range ManifestNames {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		var m Manifest
		if err := yaml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		m.File = name
		m.normalize()
		return &m, nil
	}
	return nil, nil
}

// normalize drops entries that cannot mean anything and puts roots in the slash
// form the rest of the code compares against.
func (m *Manifest) normalize() {
	svcs := m.Services[:0]
	for _, s := range m.Services {
		s.Name = strings.TrimSpace(s.Name)
		if s.Name == "" {
			continue // a service with no name cannot be referenced or linked
		}
		s.Root = filepath.ToSlash(strings.Trim(strings.TrimSpace(s.Root), "/"))
		svcs = append(svcs, s)
	}
	m.Services = svcs

	pats := m.Ignore[:0]
	for _, p := range m.Ignore {
		if p = strings.TrimSpace(p); p != "" {
			pats = append(pats, p)
		}
	}
	m.Ignore = pats
}
