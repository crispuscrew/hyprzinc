package validate

import (
	"strings"
	"testing"
)

// A name lands in `-e NAME=VALUE`, so anything with '=' or whitespace in it shifts what
// podman reads as the value.
func TestEnv_NamesAreScreened(t *testing.T) {
	for _, name := range []string{"BAD NAME", "BAD=NAME", "1LEADING", "has-dash", ""} {
		cfg := baseCfg()
		cfg.Env = map[string]string{name: "x"}
		if err := Validate(cfg); err == nil {
			t.Errorf("Env name %q was accepted", name)
		}
	}
	cfg := baseCfg()
	cfg.Env = map[string]string{"LANG": "en_US.UTF-8", "_UNDERSCORE": "1", "A1": "2"}
	if err := Validate(cfg); err != nil {
		t.Errorf("ordinary env names were rejected: %v", err)
	}
}

// Zinc's own variables describe what the runner constructed. A config overriding one cannot
// make its version true; it can only point the app somewhere there is nothing, and the failure
// surfaces inside the app with nothing pointing back at the config.
func TestEnv_ReservedNamesAreRefused(t *testing.T) {
	for _, name := range []string{"XDG_RUNTIME_DIR", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS"} {
		cfg := baseCfg()
		cfg.Env = map[string]string{name: "/tmp/anything"}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "cannot be set here") {
			t.Errorf("Env %q: want a refusal, got: %v", name, err)
		}
	}
}

func TestEnv_ValuesMustBeOneCleanLine(t *testing.T) {
	for _, value := range []string{"two\nlines", "bell\x07"} {
		cfg := baseCfg()
		cfg.Env = map[string]string{"VAR": value}
		if err := Validate(cfg); err == nil {
			t.Errorf("Env value %q was accepted", value)
		}
	}
}
