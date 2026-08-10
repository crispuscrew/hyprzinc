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
	checkAudioDevice("Monitor", cfg.AudioMeta.Monitor, add)
	if len(cfg.AudioMeta.Monitor.Devices) > 0 {
		// The device-list form is the strong one everywhere else, and here it is meaningless.
		// A monitor source is a tap on PipeWire's mix; the card knows nothing about it, so
		// there is no /dev/snd node that could grant or deny it. Accepting a list would let a
		// config look like it had narrowed this to one device when it had done nothing.
		add("AudioMeta.Monitor: a device list means nothing here - a .monitor source is part of PipeWire's graph, not a card, so no /dev/snd node carries one; write none or default")
	}
}

// alsaDirection reports the direction an ALSA node name declares, if it declares one. A PCM
// node is named pcmC<card>D<device><direction>, where the trailing letter is 'c' for capture
// and 'p' for playback. A control node (controlC<card>) has no direction and is needed by both.
func alsaDirection(device string) (capture bool, known bool) {
	name := device[strings.LastIndex(device, "/")+1:]
	if !strings.HasPrefix(name, "pcm") || len(name) == 0 {
		return false, false // controlC0, hwC0D0, seq, timer: not direction-bearing
	}
	switch name[len(name)-1] {
	case 'c':
		return true, true
	case 'p':
		return false, true
	}
	return false, false
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
		default:
			// The field an entry sits in has to mean something. Every named node is passed with
			// --device, and the runner unions the two lists into one set, so without this a
			// capture PCM listed under Playback grants a microphone to a config that reads
			// "output only" - and says nothing, because nothing asked for `default`. The whole
			// claim about the list form is that it is the enforced one; a label the kernel
			// never sees is not enforcement.
			if capture, known := alsaDirection(device); known {
				switch {
				case capture && field == "Playback":
					add("AudioMeta.Playback[%d] %q: that is a CAPTURE device (the trailing 'c'), so listing it under Playback grants a microphone to an app whose config reads output-only; move it to Microphone", index, device)
				case !capture && field == "Microphone":
					add("AudioMeta.Microphone[%d] %q: that is a PLAYBACK device (the trailing 'p'), so it grants no capture; move it to Playback", index, device)
				}
			}
		}
	}
}

// audioWarnings surfaces the gap between what a container config says about audio and what
// the runtime can currently hold it to.
//
// Two separate gaps, reported separately because a reader can act on one and not the other.
//
// The first is direction. `default` mounts the session's PipeWire socket, and PipeWire grants
// that client capture as well as playback, so `Playback: default` with `Microphone: none`
// describes an app that cannot listen and does not stop it listening.
//
// The second is Monitor. The socket exposes every sink's `.monitor` source, so it also grants
// "record what every OTHER app is playing". An app that declares `Monitor: default` is simply
// describing what it gets, and gets no warning; one that says `none` is making a promise the
// runtime cannot keep, and is told so.
//
// Neither caveat applies to a device list. ALSA nodes are the card, not PipeWire's graph, so
// there are no other applications' streams there to open.
func audioWarnings(cfg schema.AppConfig) []string {
	if cfg.Type != schema.ZincContainer {
		return nil // a guest gets a codec chosen by the runner, and cannot see the host graph
	}
	if !usesSessionAudio(cfg.AudioMeta) {
		return nil
	}
	var warns []string
	if !cfg.AudioMeta.Microphone.Default {
		warns = append(warns,
			"AudioMeta: asking for a session audio device (default) mounts the session's PipeWire socket, which grants this app "+
				"microphone capture too. Microphone: none is not yet enforced for a container. Name exact "+
				"/dev/snd nodes instead if the app must not be able to listen.")
	}
	if !cfg.AudioMeta.Monitor.Default {
		warns = append(warns,
			"AudioMeta: the PipeWire socket also exposes every sink's .monitor source, so this app can record "+
				"what OTHER apps are playing. Monitor: none is not yet enforced for a container. A device list "+
				"avoids it, because ALSA nodes carry no other application's stream.")
	}
	return warns
}

// usesSessionAudio reports whether any direction asked for the session's own devices, which
// is what puts the PipeWire socket in the container and brings both caveats with it.
func usesSessionAudio(audio schema.AudioMeta) bool {
	return audio.Playback.Default || audio.Microphone.Default || audio.Monitor.Default
}
