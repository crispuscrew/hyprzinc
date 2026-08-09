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
// Two separate gaps, and they are worth reporting separately because a reader can act on one
// of them and not the other.
//
// The first is direction. `default` mounts the session's PipeWire socket, and PipeWire grants
// that client capture as well as playback, so `Playback: default` with `Microphone: none`
// describes an app that cannot listen and does not stop it listening.
//
// The second is subtler and has no field at all: a PipeWire sink carries a `.monitor` source,
// which is a readable tap on everything being played through it. A client on the socket can
// open one. So the socket also grants "record what every OTHER app is playing", which crosses
// the boundary between two sandboxed apps rather than between an app and a host device - a
// music player and a video call share a sink. Asking for a microphone is not asking for that,
// so granting Microphone does not make this one go away.
//
// Neither caveat applies to a device list. ALSA nodes are the card, not PipeWire's graph, so
// there are no other applications' streams there to open.
func audioWarnings(cfg schema.AppConfig) []string {
	if cfg.Type != schema.ZincContainer {
		return nil // a guest gets a codec chosen by the runner, and cannot see the host graph
	}
	if !cfg.AudioMeta.Playback.Default && !cfg.AudioMeta.Microphone.Default {
		return nil
	}
	var warns []string
	if !cfg.AudioMeta.Microphone.Default {
		warns = append(warns,
			"AudioMeta: Playback: default mounts the session's PipeWire socket, which grants this app "+
				"microphone capture too. Microphone: none is not yet enforced for a container. Name exact "+
				"/dev/snd nodes instead if the app must not be able to listen.")
	}
	warns = append(warns,
		"AudioMeta: the PipeWire socket also exposes every sink's .monitor source, so this app can record "+
			"what OTHER apps are playing, not just its own audio. No config field narrows that today; a device "+
			"list avoids it, because ALSA nodes carry no other application's stream.")
	return warns
}
