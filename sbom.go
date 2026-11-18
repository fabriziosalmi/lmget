package lmget

import (
	"encoding/json"
	"net/url"
	"path"
	"strings"
)

// CycloneDX writes the lock as a CycloneDX 1.6 bill of materials: one
// machine-learning-model component per model (name, commit, license, where
// it comes from) holding one file component per file with its sha256.
// The same lock gives the same bytes: no timestamp, no random serial number.

// spdxIDs maps the license names Hugging Face uses to SPDX identifiers;
// anything else is written as a license name, not an id.
var spdxIDs = map[string]string{
	"apache-2.0": "Apache-2.0", "mit": "MIT", "bsd-2-clause": "BSD-2-Clause", "bsd-3-clause": "BSD-3-Clause",
	"cc0-1.0": "CC0-1.0", "cc-by-3.0": "CC-BY-3.0", "cc-by-4.0": "CC-BY-4.0", "cc-by-sa-4.0": "CC-BY-SA-4.0",
	"cc-by-nc-4.0": "CC-BY-NC-4.0", "cc-by-nc-sa-4.0": "CC-BY-NC-SA-4.0", "cc-by-nc-nd-4.0": "CC-BY-NC-ND-4.0",
	"mpl-2.0": "MPL-2.0", "unlicense": "Unlicense", "bsl-1.0": "BSL-1.0", "artistic-2.0": "Artistic-2.0",
	"isc": "ISC", "zlib": "Zlib", "afl-3.0": "AFL-3.0", "ecl-2.0": "ECL-2.0", "epl-2.0": "EPL-2.0", "eupl-1.2": "EUPL-1.2",
	"wtfpl": "WTFPL",
}

type cdxHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type cdxLicense struct {
	License struct {
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"license"`
}

type cdxRef struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type cdxComponent struct {
	Type       string         `json:"type"`
	BOMRef     string         `json:"bom-ref"`
	Group      string         `json:"group,omitempty"`
	Name       string         `json:"name"`
	Version    string         `json:"version,omitempty"`
	PURL       string         `json:"purl,omitempty"`
	Hashes     []cdxHash      `json:"hashes,omitempty"`
	Licenses   []cdxLicense   `json:"licenses,omitempty"`
	External   []cdxRef       `json:"externalReferences,omitempty"`
	Components []cdxComponent `json:"components,omitempty"`
}

type cdxBOM struct {
	Format   string `json:"bomFormat"`
	Spec     string `json:"specVersion"`
	Version  int    `json:"version"`
	Metadata struct {
		Tools struct {
			Components []cdxComponent `json:"components"`
		} `json:"tools"`
	} `json:"metadata"`
	Components []cdxComponent `json:"components"`
}

// CycloneDX returns the bill of materials of the lock, as JSON. tool is the
// version of lmget that writes it.
func (l *Lock) CycloneDX(tool string) ([]byte, error) {
	b := cdxBOM{Format: "CycloneDX", Spec: "1.6", Version: 1}
	b.Metadata.Tools.Components = []cdxComponent{{Type: "application", BOMRef: "lmget", Name: "lmget", Version: tool}}
	b.Components = []cdxComponent{}
	for _, m := range l.Models {
		ref, err := ParseRef(m.Ref)
		if err != nil {
			return nil, err
		}
		c := cdxComponent{Type: "machine-learning-model", Version: m.Commit}
		switch ref.Source {
		case "hf":
			org, name, _ := strings.Cut(ref.Repo, "/")
			c.Group, c.Name = org, name
			c.PURL = "pkg:huggingface/" + url.PathEscape(org) + "/" + url.PathEscape(name) + "@" + strings.ToLower(m.Commit)
			c.BOMRef = c.PURL
			c.External = []cdxRef{{Type: "distribution", URL: "https://huggingface.co/" + ref.Repo + "/tree/" + m.Commit}}
		case "ollama":
			c.Name = strings.TrimPrefix(ref.String(), "ollama:")
			if rest, ok := hfOllamaRepo(ref.Repo); ok {
				c.External = []cdxRef{{Type: "distribution", URL: "https://huggingface.co/" + rest}}
			} else {
				c.External = []cdxRef{{Type: "distribution", URL: "https://ollama.com/" + ref.Repo + ":" + ref.Revision}}
			}
		case "url":
			r := ref.Redacted() // a signed URL's query never reaches a report
			c.Type, c.Name = "file", path.Base(strings.SplitN(r.Repo, "?", 2)[0])
			c.External = []cdxRef{{Type: "distribution", URL: r.String()}}
		default:
			c.Name = ref.String()
		}
		if c.BOMRef == "" {
			c.BOMRef = ref.Redacted().String() + "@" + m.Commit
		}
		if m.License != "" && m.License != LicenseNone {
			var lic cdxLicense
			if id, ok := spdxIDs[strings.ToLower(m.License)]; ok {
				lic.License.ID = id
			} else {
				lic.License.Name = m.License
			}
			c.Licenses = []cdxLicense{lic}
		}
		for _, f := range m.Files {
			c.Components = append(c.Components, cdxComponent{Type: "file", BOMRef: c.BOMRef + "#" + f.Path, Name: f.Path,
				Hashes: []cdxHash{{Alg: "SHA-256", Content: f.SHA256}}})
		}
		b.Components = append(b.Components, c)
	}
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
