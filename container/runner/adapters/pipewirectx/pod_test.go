package pipewirectx

import (
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// The sizes here are not guesses: they are what the daemon itself sends. A Struct of two Ints
// arrives as a 40-byte payload (8 struct header + two 16-byte ints), which is exactly what a
// core Done event measures on the wire, so a change that breaks this framing breaks every
// message Zinc sends.
func TestStructOfTwoIntsMatchesTheDaemonsOwnFraming(t *testing.T) {
	got := structPod(intPod(1), intPod(2))
	if len(got) != 40 {
		t.Fatalf("Struct(Int,Int) is %d bytes, want 40", len(got))
	}
	if size := wire.Uint32(got[0:]); size != 32 {
		t.Errorf("struct body size = %d, want 32", size)
	}
	if typ := wire.Uint32(got[4:]); typ != podStruct {
		t.Errorf("struct type = %d, want %d", typ, podStruct)
	}
	// Each Int states a body of 4 and occupies 16: 8 of header and 8 of padded body.
	if size := wire.Uint32(got[8:]); size != 4 {
		t.Errorf("int body size = %d, want 4", size)
	}
}

// A string's stated length includes its NUL. Getting this wrong is what makes a daemon read
// the next field as part of the name.
func TestStringPodCountsItsTerminator(t *testing.T) {
	got := stringPod("abc")
	if size := wire.Uint32(got[0:]); size != 4 {
		t.Errorf("size = %d, want 4 (three bytes and a NUL)", size)
	}
	if len(got) != 16 {
		t.Errorf("total = %d, want 16 (8 header, 4 body, 4 padding)", len(got))
	}
}

func TestRoundTripOfADict(t *testing.T) {
	body := structPod(dictPod("a", "1", "b", "2"))
	fields, err := (&reader{buf: body}).structure()
	if err != nil {
		t.Fatal(err)
	}
	props, err := fields.dict()
	if err != nil {
		t.Fatal(err)
	}
	if props["a"] != "1" || props["b"] != "2" {
		t.Fatalf("round trip lost the dict: %v", props)
	}
}

// An empty dict is what a caller sends when it has nothing to say, and it must still be a
// well-formed struct rather than nothing at all.
func TestEmptyDictIsAStructWithAZeroCount(t *testing.T) {
	got := dictPod()
	if len(got) != 24 {
		t.Fatalf("empty dict is %d bytes, want 24", len(got))
	}
	fields, err := (&reader{buf: structPod(got)}).structure()
	if err != nil {
		t.Fatal(err)
	}
	props, err := fields.dict()
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 0 {
		t.Fatalf("want an empty dict, got %v", props)
	}
}

// A truncated POD must be an error rather than a short read: these bodies decide what an app
// is allowed to reach, and a half-parsed permission set is worse than none.
func TestTruncatedPodIsRefused(t *testing.T) {
	full := structPod(intPod(7), stringPod("x"))
	for cut := 1; cut < len(full); cut += 4 {
		fields, err := (&reader{buf: full[:len(full)-cut]}).structure()
		if err != nil {
			continue // refused at the outer struct, which is the point
		}
		if _, err := fields.int32(); err != nil {
			continue
		}
		if _, err := fields.string(); err == nil {
			t.Fatalf("a POD truncated by %d bytes parsed as whole", cut)
		}
	}
}

// The message header is what frames everything else: id, opcode and size share two words, and
// a reader that gets the split wrong desynchronises for the rest of the connection.
func TestMessageFramingRoundTrips(t *testing.T) {
	payload := structPod(intPod(9))
	head := make([]byte, headerSize)
	wire.PutUint32(head[0:], 42)
	wire.PutUint32(head[4:], uint32(7)<<24|uint32(len(payload)))
	frame := append(head, payload...)

	msg, size, ok := parse(frame)
	if !ok {
		t.Fatal("a whole message did not parse")
	}
	if size != len(frame) {
		t.Errorf("consumed %d bytes, want %d", size, len(frame))
	}
	if msg.id != 42 || msg.opcode != 7 {
		t.Errorf("id/opcode = %d/%d, want 42/7", msg.id, msg.opcode)
	}
	// One byte short is not a message yet, which is what makes the read loop correct on a
	// stream that splits wherever the kernel felt like it.
	if _, _, ok := parse(frame[:len(frame)-1]); ok {
		t.Error("a truncated frame parsed as whole")
	}
}

// Applies decides whether an app gets a socket at all. A device-list grant must not: those
// nodes are passed with --device and enforced by the kernel, which is the stronger answer.
func TestAppliesOnlyForSessionAudio(t *testing.T) {
	cases := []struct {
		name string
		meta func() (playback, microphone, monitor bool)
		want bool
	}{
		{"nothing granted", func() (bool, bool, bool) { return false, false, false }, false},
		{"playback only", func() (bool, bool, bool) { return true, false, false }, true},
		{"microphone only", func() (bool, bool, bool) { return false, true, false }, true},
		{"monitor only", func() (bool, bool, bool) { return false, false, true }, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			playback, microphone, monitor := testCase.meta()
			cfg := audioConfig(playback, microphone, monitor)
			if got := Applies(cfg); got != testCase.want {
				t.Fatalf("Applies = %v, want %v", got, testCase.want)
			}
		})
	}
}

// The holder is told only whether capture was granted, because that is the only half the
// permissions depend on.
func TestGrantReadsTheMicrophoneDirection(t *testing.T) {
	if grantOf(audioConfig(true, false, false)).capture {
		t.Error("playback alone must not grant capture")
	}
	if !grantOf(audioConfig(false, true, false)).capture {
		t.Error("a microphone grant must reach the holder")
	}
}

// audioConfig builds the one config shape these tests care about.
func audioConfig(playback, microphone, monitor bool) schema.AppConfig {
	return schema.AppConfig{AudioMeta: schema.AudioMeta{
		Playback:   schema.AudioDevice{Default: playback},
		Microphone: schema.AudioDevice{Default: microphone},
		Monitor:    schema.AudioDevice{Default: monitor},
	}}
}
