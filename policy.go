package lmget

import (
	"slices"
	"strings"
)

// A license policy names the licenses a model may come under, as the source
// names them (Hugging Face's license tag: apache-2.0, mit, llama3.2, ...),
// and "none" for a model that declares none. A project keeps one in
// lmget.lock ([policy] licenses); $LMGET_LICENSES sets one for a machine, a
// CI job or an agent. When both are set, a model must be allowed by both.
// The check runs on the plan, before anything is downloaded.

// ParseLicenses reads a list of license names separated by commas or
// spaces, lowercased.
func ParseLicenses(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		out = append(out, strings.ToLower(f))
	}
	return out
}

// licensePolicy refuses a plan whose license the policies do not allow.
func (c *Client) licensePolicy(plan *Plan, project []string) error {
	lic := strings.ToLower(plan.License)
	if lic == "" {
		lic = LicenseNone
	}
	for _, p := range []struct {
		allowed []string
		from    string
	}{{c.opt.Licenses, "LMGET_LICENSES"}, {project, "the lockfile's [policy]"}} {
		if len(p.allowed) == 0 || slices.Contains(p.allowed, lic) {
			continue
		}
		shown := plan.License
		if shown == "" {
			shown = "no license"
		}
		return newErr(ExitVerify, "policy", plan.Ref.Display()+" comes under "+quote(shown)+", which the license policy does not allow").
			why("%s allows %s", p.from, strings.Join(p.allowed, ", ")).
			fix("", "pick another model, or add "+lic+" to the policy once you have read the license")
	}
	return nil
}
