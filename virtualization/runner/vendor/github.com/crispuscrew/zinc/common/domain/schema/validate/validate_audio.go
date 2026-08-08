package validate

import (
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// alsaRoot is the only directory an audio device node may come from. Every ALSA character
// device the kernel exposes lives here, so anything outside it is not a sound device and has
// no business being handed to an app through a field labelled Playback or Microphone.
const alsaRoot = "/dev/snd/"

// checkAudio screens both directions of AudioMeta.
//
// The device-list form is the enforceable one: each entry becomes a `--device` argument, so
// the kernel decides what the app can open. That makes the entries argv, and they get the
// same treatment as every other value that reaches a command line.
func checkAudio(cfg schema.AppConfig, add addFunc) {
	checkAudioDevice("Playback", cfg.AudioMeta.Playback, add)
	checkAudioDevice("Microphone", cfg.AudioMeta.Microphone, add)
}

func checkAudioDevice(field string, dev schema.AudioDevice, add addFunc) {
	if dev.Default && len(dev.Devices) > 0 {
		// Unreachable through YAML, where the two forms are a scalar and a list, but a
		// config built in code could set both and they mean different things.
		add("AudioMeta.%s: cannot be both %q and a device list - pick the session default or exact nodes", field, "default")
		return
	}
	for index, device := range dev.Devices {
		switch {
		case strings.TrimSpace(device) == "":
			add("AudioMeta.%s[%d]: must not be empty", field, index)
		case hasUnsafe(device) || strings.ContainsAny(device, ":,"):
			add("AudioMeta.%s[%d] %q: must not contain ':', ',', or whitespace (it becomes a --device argument)", field, index, device)
		case !strings.HasPrefix(device, alsaRoot):
			add("AudioMeta.%s[%d] %q: must name an ALSA device node under %s (for example %scontrolC0 and %spcmC0D0c)",
				field, index, device, alsaRoot, alsaRoot, alsaRoot)
		case hasDotDot(device):
			add("AudioMeta.%s[%d] %q: must not contain a '..' segment - the node that gets passed should be the node that was reviewed", field, index, device)
		}
	}
}

// audioWarnings surfaces the gap between what a container config says about audio and what
// the runtime can currently hold it to.
//
// `default` mounts the session's PipeWire socket, and PipeWire grants that client capture
// regardless of which direction the config asked for. So on a container, `Playback: default`
// with `Microphone: none` describes an app that cannot listen, and the app can listen. Saying
// this out loud is the whole reason the two directions are separate fields: the old single
// flag could not even express the claim, let alone fail to keep it.
//
// A device list carries no such caveat, on either side.
func audioWarnings(cfg schema.AppConfig) []string {
	if cfg.Type != schema.ZincContainer {
		return nil // a guest gets a playback-only sound device, which the VM runner enforces
	}
	if !cfg.AudioMeta.Playback.Default && !cfg.AudioMeta.Microphone.Default {
		return nil
	}
	if cfg.AudioMeta.Microphone.Default {
		return nil // the config asked to be heard, so there is no gap between claim and grant
	}
	return []string{
		"AudioMeta: Playback: default mounts the session's PipeWire socket, which grants this app " +
			"microphone capture too, and the monitor sources that record what other apps are playing. " +
			"Microphone: none is not yet enforced for a container. Name exact /dev/snd nodes instead if " +
			"the app must not be able to listen.",
	}
}
