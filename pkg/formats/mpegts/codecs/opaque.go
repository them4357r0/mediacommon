package codecs

// Opaque is an elementary stream that is not decoded nor interpreted:
// its payload is transported as it is, together with the stream_type and the descriptors
// that the stream has in the PMT. It is meant for data streams that must be forwarded
// verbatim, like timed ID3 metadata (stream_type 0x15) or SCTE-35 (stream_type 0x86).
type Opaque struct {
	// stream_type of the PMT entry.
	StreamType uint8

	// Raw descriptor loop of the PMT entry, i.e. a sequence of
	// descriptor_tag, descriptor_length, descriptor_data.
	// It includes any ISO 639 language descriptor.
	Descriptors []byte

	// Whether the payload is a sequence of private sections (i.e. SCTE-35)
	// or PES packets.
	// When true, units are complete sections including header and CRC;
	// when false, units are PES payloads.
	Sections bool
}

// IsVideo implements Codec.
func (*Opaque) IsVideo() bool {
	return false
}

func (*Opaque) isCodec() {}
