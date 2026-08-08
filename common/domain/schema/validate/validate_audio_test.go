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
