package validate

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// The two flags are opposites. Accepting both would mean one of them silently doing nothing,
// which is the shape this schema refuses everywhere else.
func TestDisplay_TheTwoSecurityContextFlagsCannotBothBeSet(t *testing.T) {
	cfg := baseCfg()
	cfg.DisplayMeta.DisableSecurityContext = true
	cfg.DisplayMeta.RequireSecurityContext = true
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "opposites") {
		t.Fatalf("want a refusal for the contradictory pair, got: %v", err)
	}
}

func TestDisplay_EitherFlagAloneIsFine(t *testing.T) {
	for _, apply := range []func(*schema.AppConfig){
		func(cfg *schema.AppConfig) { cfg.DisplayMeta.DisableSecurityContext = true },
		func(cfg *schema.AppConfig) { cfg.DisplayMeta.RequireSecurityContext = true },
		func(cfg *schema.AppConfig) {},
	} {
		cfg := baseCfg()
		apply(&cfg)
		if err := Validate(cfg); err != nil {
			t.Errorf("DisplayMeta %+v was rejected: %v", cfg.DisplayMeta, err)
		}
	}
}

// A guest draws into a qemu window and never speaks the host compositor's protocol, so there
// is no security context to require. Refuse rather than accept a field that cannot apply.
func TestDisplay_RequireRefusedOnAVMApp(t *testing.T) {
	cfg := baseVM()
	cfg.DisplayMeta.RequireSecurityContext = true
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "RequireSecurityContext") {
		t.Fatalf("want a refusal on a VM app, got: %v", err)
	}
}
