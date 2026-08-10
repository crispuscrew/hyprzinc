package validate

import "github.com/crispuscrew/zinc/common/domain/schema"

// checkDisplay screens DisplayMeta.
//
// The two security-context flags are opposites: one says "run me without one", the other says
// "refuse to run me without one". A config asserting both has not narrowed anything, it has
// said two incompatible things, and picking a winner would mean one of them silently doing
// nothing. Refuse instead, the same way the schema refuses every other ambiguous pairing.
func checkDisplay(cfg schema.AppConfig, add addFunc) {
	display := cfg.DisplayMeta
	if display.DisableSecurityContext && display.RequireSecurityContext {
		add("DisplayMeta: DisableSecurityContext and RequireSecurityContext are opposites - one runs the app without a security context, the other refuses to run it without one; set at most one")
	}
}
