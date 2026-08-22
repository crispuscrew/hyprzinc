package schema

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The three forms have to survive a round trip, because zc re-writes a config every time it
// saves one. The earlier shape for this field failed exactly here: "not granted" was encoded
// as an absence, a nil slice marshalled as `[]`, and `[]` decoded back as "granted the
// session default" - so an ordinary save silently handed every app a microphone.
func TestAudioDevice_RoundTripsAllThreeForms(t *testing.T) {
	for _, testCase := range []struct {
		doc  string
		want AudioDevice
	}{
		{"Playback: none", AudioDevice{}},
		{"Playback: default", AudioDevice{Default: true}},
		{"Playback: [/dev/snd/controlC0, /dev/snd/pcmC0D0c]",
			AudioDevice{Devices: []string{"/dev/snd/controlC0", "/dev/snd/pcmC0D0c"}}},
	} {
		t.Run(testCase.doc, func(t *testing.T) {
			var first AudioMeta
			if err := yaml.Unmarshal([]byte(testCase.doc), &first); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if first.Playback.Default != testCase.want.Default ||
				strings.Join(first.Playback.Devices, ",") != strings.Join(testCase.want.Devices, ",") {
				t.Fatalf("decoded %+v, want %+v", first.Playback, testCase.want)
			}
			out, err := yaml.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			var second AudioMeta
			if err := yaml.Unmarshal(out, &second); err != nil {
				t.Fatalf("re-decoding what we wrote failed: %v\n%s", err, out)
			}
			if second.Playback.Default != first.Playback.Default ||
				strings.Join(second.Playback.Devices, ",") != strings.Join(first.Playback.Devices, ",") {
				t.Fatalf("round trip changed the grant: %+v -> %+v via %q",
					first.Playback, second.Playback, out)
			}
		})
	}
}

// An absent field means the same as an explicit `none`, so a hand-written config that omits a
// direction is not granted it.
func TestAudioDevice_AbsentIsNotGranted(t *testing.T) {
	var audio AudioMeta
	if err := yaml.Unmarshal([]byte("{}"), &audio); err != nil {
		t.Fatal(err)
	}
	if !audio.Playback.IsZero() || !audio.Microphone.IsZero() {
		t.Fatalf("an absent AudioMeta granted something: %+v", audio)
	}
}

// A save must never turn "not granted" into a grant. This is the property the previous shape
// broke, so it gets its own test rather than being implied by the round-trip table.
func TestAudioDevice_NotGrantedIsWrittenExplicitly(t *testing.T) {
	out, err := yaml.Marshal(AudioMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); !strings.Contains(got, "Playback: none") || !strings.Contains(got, "Microphone: none") {
		t.Fatalf("a denied grant should be written out as none, got:\n%s", got)
	}
}

// A typo has to be an error. Silently reading an unrecognised word as "not granted" would be
// the safe direction for the app and the wrong one for the person, who would be told nothing
// while their microphone setting did nothing.
func TestAudioDevice_UnknownScalarIsRefused(t *testing.T) {
	var audio AudioMeta
	err := yaml.Unmarshal([]byte("Microphone: yes"), &audio)
	if err == nil || !strings.Contains(err.Error(), "want none") {
		t.Fatalf("want a refusal naming the accepted forms, got: %v", err)
	}
}
