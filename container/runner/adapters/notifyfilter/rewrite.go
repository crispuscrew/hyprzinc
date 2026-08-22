package notifyfilter

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// notifyInterface is the one interface this filter acts on.
const notifyInterface = "org.freedesktop.Notifications"

// The positions of Notify's arguments, whose signature is susssasa{sv}i:
//
//	app_name, replaces_id, app_icon, summary, body, actions, hints, expire_timeout
const (
	argSummary       = 3
	argBody          = 4
	argActions       = 5
	argExpireTimeout = 7
	argCount         = 8
)

// prolongedMs is the ceiling an app that was not granted AllowedProlonged is held to. A
// notification that never expires (-1) or sits for minutes is a notification that owns a corner
// of the screen, which is a thing to grant rather than assume. Ten seconds is the common
// server default, so an app that asked for nothing unusual is unaffected.
const prolongedMs int32 = 10000

// linkTag matches the anchor markup the specification allows in a body. Notification servers
// render it as a clickable link, so an app that was not granted links must not be able to put
// one where a person will click it.
var linkTag = regexp.MustCompile(`(?is)</?a\b[^>]*>`)

// verdict is what the filter decided about one call.
type verdict int

const (
	// verdictForward passes the call on, possibly rewritten.
	verdictForward verdict = iota
	// verdictSilence answers the app itself, without the call reaching the server.
	verdictSilence
	// verdictRefuse answers with an error, so the app is told rather than misled.
	verdictRefuse
)

// policy is NotificationMeta reduced to what the filter does with a call.
type policy struct {
	disabled bool
	silenced bool
	prefix   string
	actions  bool
	longer   bool
	links    bool
}

func policyOf(meta schema.NotificationMeta) policy {
	pol := policy{
		disabled: meta.Disabled,
		silenced: meta.Silenced,
		actions:  meta.AllowedActions,
		longer:   meta.AllowedProlonged,
		links:    meta.AllowedLinks,
	}
	if meta.UseCustomPrefix {
		pol.prefix = strings.TrimSpace(meta.CustomPrefix)
	}
	return pol
}

// applies reports whether this policy changes anything. A zero block is the default and must
// leave the app's traffic exactly as it was, which is also what keeps the filter out of the
// launch path for every app that does not ask for it.
func (pol policy) applies() bool {
	return pol != policy{actions: true, longer: true, links: true}
}

// decide applies the policy to one Notify call, returning the bytes to forward.
//
// The message is decoded and re-encoded rather than patched in place: D-Bus alignment is
// measured from the start of the message, so changing the length of the summary moves the
// padding inside the hints dictionary that follows it. Patching bytes would corrupt every
// notification carrying a hint.
func (pol policy) decide(raw []byte) (verdict, []byte, error) {
	if pol.disabled {
		return verdictRefuse, nil, nil
	}
	msg, err := dbus.DecodeMessage(bytes.NewReader(raw))
	if err != nil {
		// A call this filter cannot read is one it must not silently drop. Forwarding it
		// unchanged is what the relay would have done without a policy at all.
		return verdictForward, raw, nil
	}
	if len(msg.Body) != argCount {
		return verdictForward, raw, nil
	}
	if pol.silenced {
		return verdictSilence, nil, nil
	}

	changed := false
	if summary, ok := msg.Body[argSummary].(string); ok && pol.prefix != "" {
		msg.Body[argSummary] = pol.prefix + " " + summary
		changed = true
	}
	if body, ok := msg.Body[argBody].(string); ok && !pol.links {
		if stripped := linkTag.ReplaceAllString(body, ""); stripped != body {
			msg.Body[argBody] = stripped
			changed = true
		}
	}
	if actions, ok := msg.Body[argActions].([]string); ok && !pol.actions && len(actions) > 0 {
		msg.Body[argActions] = []string{}
		changed = true
	}
	if timeout, ok := msg.Body[argExpireTimeout].(int32); ok && !pol.longer {
		// -1 is "let the server decide", which is not a claim on the screen and is left alone.
		// 0 is "never expire", which is.
		if timeout == 0 || timeout > prolongedMs {
			msg.Body[argExpireTimeout] = prolongedMs
			changed = true
		}
	}
	if !changed {
		return verdictForward, raw, nil
	}

	var out bytes.Buffer
	if err := msg.EncodeTo(&out, binary.LittleEndian); err != nil {
		return verdictForward, raw, fmt.Errorf("re-encode a filtered notification: %w", err)
	}
	encoded := out.Bytes()
	// EncodeTo writes the serial godbus holds, which is not the app's. The relay is transparent
	// about identity: the call must reach the server under the serial the app chose, or the
	// reply the app is waiting for will never match.
	if len(encoded) >= fixedHeaderSize && len(raw) >= fixedHeaderSize {
		copy(encoded[8:12], raw[8:12])
	}
	return verdictForward, encoded, nil
}

// silenceReply builds the answer a silenced app gets: the same shape the server would have
// sent, so an app cannot tell that its notification went nowhere. That is the point of
// Silenced as distinct from Disabled, which is answered with an error.
func silenceReply(call header, raw []byte, id uint32) ([]byte, error) {
	return replyTo(raw, call, &dbus.Message{
		Type:    dbus.TypeMethodReply,
		Headers: map[dbus.HeaderField]dbus.Variant{},
		Body:    []interface{}{id},
	})
}

// refuseReply builds the error a disabled app gets. Named for what it is, so the app's own log
// says why rather than leaving a developer to guess at a silent failure.
func refuseReply(call header, raw []byte) ([]byte, error) {
	return replyTo(raw, call, &dbus.Message{
		Type: dbus.TypeError,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldErrorName: dbus.MakeVariant("org.freedesktop.DBus.Error.AccessDenied"),
		},
		Body: []interface{}{"notifications are disabled for this app by its Zinc config"},
	})
}

// replyTo finishes a synthesised reply: it carries the caller's serial, and its own serial is
// patched in afterwards because godbus keeps that field to itself.
func replyTo(raw []byte, call header, msg *dbus.Message) ([]byte, error) {
	serial := call.order.Uint32(raw[8:12])
	msg.Headers[dbus.FieldReplySerial] = dbus.MakeVariant(serial)
	if len(msg.Body) > 0 {
		msg.Headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(msg.Body...))
	}
	var out bytes.Buffer
	if err := msg.EncodeTo(&out, binary.LittleEndian); err != nil {
		return nil, fmt.Errorf("encode a reply to the app: %w", err)
	}
	encoded := out.Bytes()
	// Any non-zero serial will do for a reply the app never refers back to, and it must not be
	// zero, which the specification reserves.
	if len(encoded) >= fixedHeaderSize {
		binary.LittleEndian.PutUint32(encoded[8:12], serial)
	}
	return encoded, nil
}
