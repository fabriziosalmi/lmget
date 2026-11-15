package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/fabriziosalmi/lmget"
)

func clean(s string) string { return lmget.Printable(s) }

// listName: the repository for Hugging Face and URLs, ollama:name:tag for Ollama.
func listName(r lmget.Ref) string {
	if r.Source == "ollama" || r.Source == "oci" {
		return clean(r.Display())
	}
	return clean(r.Repo)
}

func writeJSON(w io.Writer, typ string, fields map[string]any) {
	fields["v"] = lmget.SchemaVersion
	fields["type"] = typ
	fields["time"] = time.Now().UTC()
	json.NewEncoder(w).Encode(fields)
}

func exportLabel(ex []lmget.ExportRecord) string {
	var parts []string
	for _, e := range ex {
		if e.Kind == "dir" {
			parts = append(parts, "dir "+tilde(e.Path))
		} else {
			parts = append(parts, e.Kind)
		}
	}
	if len(parts) == 0 {
		return "store only"
	}
	return strings.Join(parts, ", ")
}

// List renders lmget ls.
func List(mode Mode, w io.Writer, models []lmget.Installed, st lmget.StoreStats) {
	switch mode {
	case ModeJSON:
		writeJSON(w, "list", map[string]any{"models": models, "store": st})
		return
	case ModePlain:
		for _, m := range models {
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\n", listName(m.Ref), m.Commit, m.Bytes, m.Files, m.Missing, exportLabel(m.Exports))
		}
		return
	}
	var b strings.Builder
	b.WriteString("\n")
	nameW := 0
	for _, m := range models {
		nameW = max(nameW, min(48, lipgloss.Width(listName(m.Ref))))
	}
	for _, m := range models {
		state := sDim.Render(exportLabel(m.Exports))
		if m.Missing > 0 {
			state = sWarn.Render(fmt.Sprintf("%d files missing", m.Missing))
		}
		files := fmt.Sprintf("%d files", m.Files)
		if m.Files == 1 {
			files = "1 file"
		}
		fmt.Fprintf(&b, "  %s  %s  %s  %s  %s\n", pad(middleTrunc(listName(m.Ref), 48), nameW), sDim.Render(m.Commit[:7]),
			padLeft(hb(m.Bytes), 8), padLeft(files, 9), state)
	}
	if len(models) == 0 {
		fmt.Fprintf(&b, "  %s\n", sDim.Render("no models yet: lmget <org/name> downloads one"))
	}
	fmt.Fprintf(&b, "\n  %s%s  %s\n", sLabel.Render("store"), tilde(st.Root),
		sDim.Render(fmt.Sprintf("%s in %d blobs · %s free", hb(st.Bytes), st.Blobs, hb(st.DiskFree))))
	if st.Partial > 0 {
		fmt.Fprintf(&b, "  %s%s\n", sLabel.Render("partial"), sWarn.Render(fmt.Sprintf("%d interrupted downloads, %s", st.Partial, hb(st.PartialBytes))))
	}
	lipgloss.Fprint(w, b.String()+"\n")
}

// Removed renders lmget rm.
func Removed(mode Mode, w io.Writer, r *lmget.RemoveResult, lockNote string) {
	switch mode {
	case ModeJSON:
		writeJSON(w, "removed", map[string]any{"result": r})
		return
	case ModePlain:
		fmt.Fprintf(w, "lmget: removed %s links=%d blobs=%d freed=%d held=%d\n", strings.Join(r.Models, ","), r.Links, r.Blobs, r.Freed, r.Held)
		if lockNote != "" {
			fmt.Fprintf(w, "lmget: note: %s\n", lockNote)
		}
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %s removed %s\n", sAccent.Render(check), strings.Join(r.Models, ", "))
	fmt.Fprintf(&b, "    %s%s\n", sLabel.Render("freed"), hb(r.Freed))
	if r.Held > 0 {
		fmt.Fprintf(&b, "    %s%s %s\n", sLabel.Render("held"), hb(r.Held), sDim.Render("still used by hard links lmget did not create"))
	}
	fmt.Fprintf(&b, "    %s%s\n", sLabel.Render("details"), sDim.Render(fmt.Sprintf("%d export links, %d blobs", r.Links, r.Blobs)))
	if lockNote != "" {
		fmt.Fprintf(&b, "    %s%s\n", sLabel.Render("note"), sWarn.Render(lockNote))
	}
	lipgloss.Fprint(w, b.String()+"\n")
}

// Doctor renders lmget doctor and returns the exit code: 1 if any check failed.
func Doctor(mode Mode, w io.Writer, cs []lmget.Check) lmget.ExitCode {
	code := lmget.ExitOK
	for _, ch := range cs {
		if ch.Status == "fail" {
			code = lmget.ExitFailure
		}
	}
	switch mode {
	case ModeJSON:
		writeJSON(w, "doctor", map[string]any{"checks": cs})
		return code
	case ModePlain:
		for _, ch := range cs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ch.Status, ch.Name, clean(ch.Detail), clean(ch.Fix))
		}
		return code
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, ch := range cs {
		mark := sAccent.Render(check)
		switch ch.Status {
		case "warn":
			mark = sWarn.Render("!")
		case "fail":
			mark = sErr.Render("✗")
		}
		fmt.Fprintf(&b, "  %s %s%s\n", mark, sLabel.Render(ch.Name), clean(ch.Detail))
		if ch.Fix != "" && ch.Status != "ok" {
			fmt.Fprintf(&b, "    %s%s\n", sLabel.Render(""), sDim.Render(clean(ch.Fix)))
		}
	}
	lipgloss.Fprint(w, b.String()+"\n")
	return code
}
