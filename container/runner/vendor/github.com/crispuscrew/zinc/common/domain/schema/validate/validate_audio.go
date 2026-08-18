package validate

import (
	"strings"

	"github.com/crispuscrew/zinc/common/domain/schema"
)

// alsaRoot is the only directory an audio device node may come from. Every ALSA character
// device the kernel exposes lives here, so anything outside it is not a sound device and has
// no business being handed to an app through a field labelled Playback or Microphone.
const alsaRoot = "/dev/snd/"

// checkAudio screens both directions of AudioMeta. The device-list form is the enforceable one - each
// entry becomes a `--device` argument - which makes the entries argv, and they get the same treatment
// as every other value reaching a command line.
func checkAudio(cfg schema.AppConfig, add addFunc) {
	checkAudioDevice("Playback", cfg.AudioMeta.Playback, add)
	checkAudioDevice("Microphone", cfg.AudioMeta.Microphone, add)
	checkAudioDevice("Monitor", cfg.AudioMeta.Monitor, add)
	if len(cfg.AudioMeta.Monitor.Devices) > 0 {
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
		// Unreachable through YAML, where the two forms are a scalar and a list, but a config built in
		// code could set both.
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
			// The field an entry sits in has to mean something. Every named node is passed with --device and the
			// runner unions the two lists, so without this a capture PCM listed under Playback grants a microphone
			// to a config that reads "output only". A label the kernel never sees is not enforcement.
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

// audioWarnings surfaces the gap between what a container config says about audio and what the runtime
// can hold it to. Two gaps, reported separately because a reader can act on one and not the other.
//
// Direction: `default` mounts the PipeWire socket, which grants capture as well as playback, so
// `Playback: default` with `Microphone: none` does not stop the app listening.
//
// Monitor: the socket exposes every sink's `.monitor` source. An app declaring `Monitor: default` is
// describing what it gets; one saying `none` is making a promise the runtime cannot keep.
//
// Neither applies to a device list: ALSA nodes are the card, not PipeWire's graph.
func audioWarnings(cfg schema.AppConfig) []string {
	if cfg.Type != schema.ZincContainer {
		return nil // a guest gets a codec chosen by the runner, and cannot see the host graph
	}
	if !usesSessionAudio(cfg.AudioMeta) {
		return nil
	}
	var warns []string
	// Microphone: none is enforced now - the runner gives the app a socket of its own under a
	// PipeWire security context and then takes away its permission on every capture node - so
	// there is nothing to warn about. What is worth saying is that the enforcement needs a
	// daemon that implements a security context; on one that does not, the app is given the
	// session socket and the launch says so on stderr rather than pretending.
	//
	// Monitor is the one direction that cannot be enforced this way, and the reason is
	// structural rather than unfinished work: a sink's .monitor is a set of ports on the sink
	// node, not a node of its own, so there is no object to deny that is not also the object
	// the app needs in order to play at all.
	if !cfg.AudioMeta.Monitor.Default && cfg.AudioMeta.Playback.Default {
		warns = append(warns,
			"AudioMeta: Monitor: none cannot be enforced alongside Playback: default. A sink's .monitor "+
				"source is a set of ports on the sink this app plays to, not a separate object, so denying "+
				"it would mean denying playback. Microphone: none IS enforced. Name exact /dev/snd nodes "+
				"if the app must not be able to record other applications at all.")
	}
	return warns
}

// usesSessionAudio reports whether any direction asked for the session's own devices, which
// is what puts the PipeWire socket in the container and brings both caveats with it.
func usesSessionAudio(audio schema.AudioMeta) bool {
	return audio.Playback.Default || audio.Microphone.Default || audio.Monitor.Default
}
