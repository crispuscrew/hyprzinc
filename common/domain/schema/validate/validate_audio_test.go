package validate

import (
	"strings"
	"testing"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// Device entries become --device arguments, so they get the same screening as every other
// value that reaches a command line, plus a rule that they actually name a sound device.
func TestAudio_DeviceNodesAreScreened(t *testing.T) {
	for _, testCase := range []struct{ device, want string }{
		{"/etc/shadow", "ALSA device node"},
		{"/dev/dri/renderD128", "ALSA device node"},
		{"/dev/snd/../dri/renderD128", "'..'"},
		{"/dev/snd/controlC0,file=x", "':', ','"},
		{"/dev/snd/control C0", "':', ','"},
		{"", "must not be empty"},
	} {
		cfg := baseCfg()
		cfg.AudioMeta.Microphone = schema.AudioDevice{Devices: []string{testCase.device}}
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("device %q: want an error mentioning %q, got: %v", testCase.device, testCase.want, err)
		}
	}
}

func TestAudio_RealDeviceNodesPass(t *testing.T) {
	cfg := baseCfg()
	cfg.AudioMeta.Playback = schema.AudioDevice{Default: true}
	cfg.AudioMeta.Microphone = schema.AudioDevice{
		Devices: []string{"/dev/snd/controlC0", "/dev/snd/pcmC0D0c"},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("a real device pair was rejected: %v", err)
	}
}

// A guest cannot be handed a host character device, so pairing a device list with a VM app is
// refused rather than silently dropped on the way to qemu.
func TestAudio_DeviceListRefusedOnAVMApp(t *testing.T) {
	cfg := baseVM()
	cfg.AudioMeta.Microphone = schema.AudioDevice{Devices: []string{"/dev/snd/controlC0"}}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "AudioMeta device lists") {
		t.Fatalf("want a refusal for a device list on a VM app, got: %v", err)
	}
	// `default` is the form a guest can actually be given, and stays allowed.
	cfg.AudioMeta.Microphone = schema.AudioDevice{Default: true}
	if err := Validate(cfg); err != nil {
		t.Fatalf("default audio should be allowed on a VM app: %v", err)
	}
}

// The container runtime cannot yet keep "output only": mounting the PipeWire socket grants
// capture whatever the config says. The warning is the whole reason the two directions are
// separate fields, so it is worth pinning.
func TestAudio_ContainerPlaybackOnlyWarnsThatCaptureIsNotDenied(t *testing.T) {
	cfg := baseCfg()
	cfg.AudioMeta.Playback = schema.AudioDevice{Default: true}
	joined := strings.Join(Warnings(cfg), "\n")
	if !strings.Contains(joined, "microphone capture") {
		t.Errorf("playback-only should warn that capture is not denied, got: %v", Warnings(cfg))
	}

	// Naming exact nodes has no such gap: the kernel enforces it.
	cfg.AudioMeta.Playback = schema.AudioDevice{Devices: []string{"/dev/snd/controlC1", "/dev/snd/pcmC1D3p"}}
	if joined := strings.Join(Warnings(cfg), "\n"); strings.Contains(joined, "microphone capture") {
		t.Errorf("a device list should not carry the socket caveat, got: %v", Warnings(cfg))
	}

	// Asking for both is not a broken promise, so it is not warned about.
	cfg.AudioMeta.Playback = schema.AudioDevice{Default: true}
	cfg.AudioMeta.Microphone = schema.AudioDevice{Default: true}
	if joined := strings.Join(Warnings(cfg), "\n"); strings.Contains(joined, "microphone capture") {
		t.Errorf("granting both directions should not warn, got: %v", Warnings(cfg))
	}
}

// A sink's .monitor source is a readable tap on everything played through it, so the socket
// grants "record what other apps are playing" as well as "record the room". Asking for a
// microphone is not asking for that, so granting one must not silence the warning.
func TestAudio_MonitorAccessIsWarnedEvenWithAMicrophoneGrant(t *testing.T) {
	cfg := baseCfg()
	cfg.AudioMeta.Playback = schema.AudioDevice{Default: true}
	cfg.AudioMeta.Microphone = schema.AudioDevice{Default: true}
	joined := strings.Join(Warnings(cfg), "\n")
	if !strings.Contains(joined, ".monitor") {
		t.Errorf("granting a microphone should not hide the monitor-source grant, got: %v", Warnings(cfg))
	}
	if strings.Contains(joined, "Microphone: none is not yet enforced") {
		t.Errorf("an app that asked to listen should not be told its Microphone: none is unenforced: %v", Warnings(cfg))
	}

	// Declaring Monitor is an honest description of what the socket grants, so it should not
	// then be warned about. The field earns its place precisely by being sayable.
	cfg.AudioMeta.Monitor = schema.AudioDevice{Default: true}
	if joined := strings.Join(Warnings(cfg), "\n"); strings.Contains(joined, ".monitor") {
		t.Errorf("an app that declared Monitor should not be warned about it: %v", Warnings(cfg))
	}
	cfg.AudioMeta.Monitor = schema.AudioDevice{}

	// A device list is the enforceable form and carries neither caveat.
	cfg.AudioMeta.Playback = schema.AudioDevice{Devices: []string{"/dev/snd/controlC1", "/dev/snd/pcmC1D3p"}}
	cfg.AudioMeta.Microphone = schema.AudioDevice{Devices: []string{"/dev/snd/controlC0", "/dev/snd/pcmC0D0c"}}
	if joined := strings.Join(Warnings(cfg), "\n"); strings.Contains(joined, ".monitor") {
		t.Errorf("ALSA nodes carry no other app's stream, so they should not warn: %v", Warnings(cfg))
	}
}

// A monitor source lives in PipeWire's graph, not on a card, so no /dev/snd node can grant or
// deny one. Accepting a list would let a config look like it had narrowed this to one device
// while doing nothing at all.
func TestAudio_MonitorRejectsADeviceList(t *testing.T) {
	cfg := baseCfg()
	cfg.AudioMeta.Monitor = schema.AudioDevice{Devices: []string{"/dev/snd/controlC0"}}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "means nothing here") {
		t.Fatalf("want a refusal for a Monitor device list, got: %v", err)
	}
}

// A guest sees an emulated sound card, not the host's PipeWire graph, so there is nothing for
// it to monitor and the field is refused rather than silently ignored on the way to qemu.
func TestAudio_MonitorRefusedOnAVMApp(t *testing.T) {
	cfg := baseVM()
	cfg.AudioMeta.Monitor = schema.AudioDevice{Default: true}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "AudioMeta.Monitor") {
		t.Fatalf("want a refusal for Monitor on a VM app, got: %v", err)
	}
}

// Monitor alone still puts the socket in the container: it is a session-graph capability, so
// it is the socket that delivers it, with or without a playback grant.
func TestAudio_MonitorAloneIsASessionGrant(t *testing.T) {
	cfg := baseCfg()
	cfg.AudioMeta.Monitor = schema.AudioDevice{Default: true}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Monitor on its own should validate: %v", err)
	}
	joined := strings.Join(Warnings(cfg), "\n")
	if !strings.Contains(joined, "Microphone: none is not yet enforced") {
		t.Errorf("the socket is mounted for Monitor too, so the capture caveat applies: %v", Warnings(cfg))
	}
}
