package validate

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// dbusApp is a valid container app asking for bus access, with the KeepUserID a filtered bus
// requires already set - so each test below changes exactly the one thing it is about.
func dbusApp() schema.AppConfig {
	cfg := schema.AppConfig{
		SchemaVersion:    3,
		Type:             schema.ZincContainer,
		AppNameID:        "notes",
		ImageMeta:        schema.ImageMeta{Image: "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)},
		InternalUserMeta: schema.InternalUserMeta{KeepUserID: true},
		DBusMeta:         schema.DBusMeta{Talk: []string{"org.freedesktop.portal.Desktop"}},
	}
	return cfg
}

// errText joins Validate's output so a test can look for its own rule's message without
// tripping over unrelated errors from the same config.
func errText(t *testing.T, cfg schema.AppConfig) string {
	t.Helper()
	if err := Validate(cfg); err != nil {
		return err.Error()
	}
	return ""
}

// The baseline must pass, or every negative test below proves nothing.
func TestDBus_ValidConfigPasses(t *testing.T) {
	if err := Validate(dbusApp()); err != nil {
		t.Fatalf("a valid bus config was rejected: %v", err)
	}
}

// An empty DBusMeta is the fail-closed default and must not require anything - notably not
// KeepUserID, which the vast majority of apps do not set.
func TestDBus_EmptyNeedsNothing(t *testing.T) {
	cfg := dbusApp()
	cfg.DBusMeta = schema.DBusMeta{}
	cfg.InternalUserMeta = schema.InternalUserMeta{}
	if err := Validate(cfg); err != nil {
		t.Fatalf("an app without DBusMeta was rejected: %v", err)
	}
}

// A filtered bus is a uid agreement: the proxy serves the socket as the host user, so an app
// in its own user namespace cannot connect. Refused explicitly rather than silently switching
// who the app runs as.
func TestDBus_RequiresKeepUserID(t *testing.T) {
	cfg := dbusApp()
	cfg.InternalUserMeta.KeepUserID = false
	if got := errText(t, cfg); !strings.Contains(got, "KeepUserID") {
		t.Errorf("a bus grant without KeepUserID was accepted or misreported: %q", got)
	}
}

// Bus names become --talk/--own arguments to the proxy, so anything that could splice an extra
// option into the filter has to be refused before it gets there.
func TestDBus_RejectsMalformedNames(t *testing.T) {
	for _, name := range []string{
		"",                        // empty
		"nodots",                  // a well-known name needs at least two elements
		"org.foo bar",             // a space would split the argument
		"org.foo --own=org.evil",  // a smuggled second option
		"org.1foo.Bar",            // element starting with a digit
		" org.foo.Bar",            // leading whitespace
		"org.foo.Bar\n--own=x",    // control character
		strings.Repeat("a.b", 99), // over the 255-character limit
	} {
		cfg := dbusApp()
		cfg.DBusMeta = schema.DBusMeta{Talk: []string{name}}
		if err := Validate(cfg); err == nil {
			t.Errorf("Talk name %q was accepted", name)
		}
	}
}

// A subtree wildcard is a meaningful thing to be allowed to call and meaningless to own, since
// a process claims one concrete name or none.
func TestDBus_WildcardTalkOnlyNotOwn(t *testing.T) {
	cfg := dbusApp()
	cfg.DBusMeta = schema.DBusMeta{Talk: []string{"org.freedesktop.portal.*"}}
	if err := Validate(cfg); err != nil {
		t.Errorf("a wildcard Talk was rejected: %v", err)
	}

	cfg.DBusMeta = schema.DBusMeta{Own: []string{"org.mpris.MediaPlayer2.*"}}
	if got := errText(t, cfg); !strings.Contains(got, "wildcard") {
		t.Errorf("a wildcard Own was accepted or misreported: %q", got)
	}
}

// A VM has no way to take a bind-mounted unix socket, so DBusMeta on a guest must be an error
// rather than a field that looks configured and does nothing.
func TestDBus_RejectedOnVMApp(t *testing.T) {
	cfg := schema.AppConfig{
		SchemaVersion: 3,
		Type:          schema.ZincVirtualization,
		AppNameID:     "guest",
		ImageMeta:     schema.ImageMeta{Image: "/var/lib/zinc/base.qcow2"},
		DBusMeta:      schema.DBusMeta{Talk: []string{"org.freedesktop.portal.Desktop"}},
	}
	if got := errText(t, cfg); !strings.Contains(got, "DBusMeta") {
		t.Errorf("DBusMeta on a VM app was accepted or misreported: %q", got)
	}
}

// A wildcard grants everything under its base, now and in future. With a two-element base
// that is a whole vendor namespace, and org.freedesktop.* alone reaches systemd1, whose
// StartTransientUnit runs an arbitrary command as the user outside the container. The line
// reads like a modest portal grant, which is exactly why it has to be refused rather than
// left to a reviewer to notice.
func TestDBus_VendorWideWildcardRefused(t *testing.T) {
	for _, name := range []string{"org.freedesktop.*", "org.gnome.*", "com.example.*"} {
		cfg := dbusApp()
		cfg.DBusMeta = schema.DBusMeta{Talk: []string{name}}
		if got := errText(t, cfg); !strings.Contains(got, "entire vendor namespace") {
			t.Errorf("Talk %q was accepted or misreported: %q", name, got)
		}
	}
	// Deeper wildcards stay allowed: this is a rule about breadth, not about wildcards.
	cfg := dbusApp()
	cfg.DBusMeta = schema.DBusMeta{Talk: []string{"org.freedesktop.portal.*"}}
	if err := Validate(cfg); err != nil {
		t.Errorf("a three-element wildcard should still be allowed, got: %v", err)
	}
}

// A grant that is legal and much wider than it reads should say so at authoring time.
func TestDBus_BroadGrantsWarn(t *testing.T) {
	cfg := dbusApp()
	cfg.DBusMeta = schema.DBusMeta{Talk: []string{"org.freedesktop.systemd1"}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("a concrete bus name should still validate: %v", err)
	}
	warns := Warnings(cfg)
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, "\n"), "way out of the sandbox") {
		t.Errorf("talking to systemd1 should warn, got %v", warns)
	}
}
