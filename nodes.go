package lmget

import (
	"os"
	"strings"
	"time"
)

// The nodes of the signed index are the last origin of a file: lmget asks
// them only when the source (and, for Hugging Face, ModelScope) failed. A
// node serves a file by its sha256, and the bytes are verified as always.
// When the source cannot be reached at all, the plan itself comes from the
// signed index: the file list and hashes are signed by the project key, so
// they are trusted where a lockfile alone is not.

// NodeOrigin prefixes File.From for bytes that came from an index node.
const NodeOrigin = "node:"

// noOrigin is the error of a file of a plan built from the index (no source
// URL) when no node served it either.
func noOrigin() error {
	return newErr(ExitNetwork, "no_origin", "the source of this file is unreachable and no index node served it")
}

// sourceDown reports a failure that the signed index may stand in for: the
// source unreachable or failing, or the model gone from it. Access refusals
// (gated, private) never qualify.
func sourceDown(err error) bool {
	e := AsError(err)
	return (e.Code == ExitNetwork && e.Kind != "changed") || e.Code == ExitNotFound
}

// indexEntry returns the verified cached index and its entry for ref at
// commit, or the last entry listed for ref when commit is empty. Without a
// valid cached index it returns nils: the index is optional.
func (c *Client) indexEntry(ref Ref, commit string) (*Index, *IndexModel) {
	ix, err := c.Index()
	if err != nil {
		return nil, nil
	}
	var hit *IndexModel
	for i := range ix.Models {
		r, err := ParseRef(ix.Models[i].Ref)
		if err == nil && r.String() == ref.String() && (commit == "" || ix.Models[i].Commit == commit) {
			hit = &ix.Models[i] // the index lists commits oldest first: the last one wins
		}
	}
	return ix, hit
}

// planFromIndex builds the plan of an indexed model without its source.
func (c *Client) planFromIndex(ref Ref, m *IndexModel, sel Selection) (*Plan, error) {
	p := &Plan{Ref: ref, Commit: m.Commit, License: m.License, FromIndex: true}
	for _, f := range m.Files {
		p.Files = append(p.Files, File{Path: f.Path, Size: f.Size, SHA256: f.SHA256, GitSHA1: f.GitSHA1})
	}
	if err := p.apply(sel, c.store, c.store.Root()); err != nil {
		return nil, err
	}
	if ref.Source == "ollama" || ref.Source == "oci" {
		for i := range p.Files { // the manifest always comes along
			if p.Files[i].Path == OllamaManifestFile {
				p.Files[i].Skipped = ""
			}
		}
		p.recount(c.store)
	}
	return p, nil
}

// nodeFiles returns f as served by each node of the index, when the index
// lists this file for ref at commit with the same size and hash.
func (c *Client) nodeFiles(ref Ref, commit string, f File) []File {
	ix, m := c.indexEntry(ref, commit)
	if m == nil {
		return nil
	}
	var lf *LockFile
	for i := range m.Files {
		if m.Files[i].Path == f.Path {
			lf = &m.Files[i].LockFile
		}
	}
	switch {
	case lf == nil || lf.Size != f.Size:
		return nil
	case f.SHA256 != "" && f.SHA256 != lf.SHA256:
		return nil
	case f.GitSHA1 != "" && lf.GitSHA1 != "" && f.GitSHA1 != lf.GitSHA1:
		return nil
	}
	var out []File
	for _, n := range ix.Nodes {
		nf := f
		nf.SHA256 = lf.SHA256 // signed by the project key: verified on arrival
		nf.url = strings.TrimRight(n, "/") + "/blobs/sha256/" + lf.SHA256
		nf.ifRange, nf.noRanges, nf.ociRepo, nf.data = "", false, "", nil
		nf.From = NodeOrigin + hostOf(n)
		out = append(out, nf)
	}
	return out
}

// revokedBlobs is the revocation list of the cached index, verified, even
// once it expired: bytes do not become safe when an index ages, and a newer
// index that withdraws a revocation replaces it.
func (c *Client) revokedBlobs() map[string]string {
	p, ps := c.indexPaths()
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	sig, err := os.ReadFile(ps)
	if err != nil {
		return nil
	}
	ix, err := c.openIndexAt(data, string(sig), time.Now(), true)
	if err != nil {
		return nil
	}
	return ix.RevokedBlobs()
}

// revokedErr refuses a file whose bytes the index revoked.
func revokedErr(revoked map[string]string, path, sha string) error {
	reason, ok := revoked[sha]
	if sha == "" || !ok {
		return nil
	}
	return newErr(ExitVerify, "revoked", path+" is revoked by the lmget index").
		why("%s (sha256 %s)", printable(reason), short(sha)).
		fix("", "the project withdrew these bytes: do not use them, whatever source serves them")
}

// nodeDownFor is how long a node that failed is skipped by the same client:
// retrying a dead node costs every following file its full retry sequence.
const nodeDownFor = 10 * time.Minute

func (c *Client) markNodeDown(origin string) {
	c.nodeMu.Lock()
	defer c.nodeMu.Unlock()
	if c.nodeDown == nil {
		c.nodeDown = map[string]time.Time{}
	}
	c.nodeDown[origin] = time.Now().Add(nodeDownFor)
}

func (c *Client) nodeIsDown(origin string) bool {
	c.nodeMu.Lock()
	defer c.nodeMu.Unlock()
	return time.Now().Before(c.nodeDown[origin])
}
