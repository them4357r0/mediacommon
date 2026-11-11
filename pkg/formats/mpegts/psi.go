package mpegts

import (
	"fmt"
)

// Minimal, allocation-light helpers to handle raw MPEG-TS sections (PSI and private sections)
// without going through astits. They are used to
// - read the raw PMT (program-level and elementary-stream-level descriptors are kept byte-exact),
// - carry private sections (i.e. SCTE-35) that astits does not expose,
// - re-generate a PMT that contains raw descriptors.

const (
	tableIDPAT = 0x00
	tableIDPMT = 0x02

	maxSectionSize = 4096 // section_length is a 12-bit field, plus the 3 header bytes

	pidPAT = 0x0000

	// astits.Muxer uses this PID for the PMT, and cannot be configured otherwise by Writer.
	writerPMTPID = 0x1000
)

var crc32MPEGTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = (c << 1) ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

// crc32MPEG computes the CRC-32/MPEG-2 used by PSI sections.
func crc32MPEG(b []byte) uint32 {
	c := uint32(0xFFFFFFFF)
	for _, v := range b {
		c = (c << 8) ^ crc32MPEGTable[byte(c>>24)^v]
	}
	return c
}

// tsPacketPayload returns PID, payload_unit_start_indicator, continuity counter and payload
// of a 188-byte packet. ok is false when the packet is invalid, in error
// or carries no payload.
func tsPacketPayload(p []byte) (pid uint16, pusi bool, cc uint8, payload []byte, ok bool) {
	if len(p) != packetSize || p[0] != syncByte || p[1]&0x80 != 0 { // transport_error_indicator
		return 0, false, 0, nil, false
	}

	pid = uint16(p[1]&0x1F)<<8 | uint16(p[2])
	pusi = p[1]&0x40 != 0
	cc = p[3] & 0x0F

	if p[3]&0x10 == 0 { // no payload
		return pid, pusi, cc, nil, false
	}

	pos := 4
	if p[3]&0x20 != 0 {
		pos += 1 + int(p[4])
	}
	if pos >= packetSize {
		return pid, pusi, cc, nil, false
	}

	return pid, pusi, cc, p[pos:], true
}

// sectionAssembler reassembles the sections carried by a single PID.
// Sections are returned as they have been received, without any validation of their content.
type sectionAssembler struct {
	buf    []byte
	active bool
	hasCC  bool
	lastCC uint8
}

// push processes a 188-byte packet of the PID and returns the sections that are completed by it.
func (a *sectionAssembler) push(p []byte) [][]byte {
	_, pusi, cc, payload, ok := tsPacketPayload(p)
	if !ok {
		return nil
	}

	// duplicate packet (allowed once by ISO 13818-1)
	if a.hasCC && cc == a.lastCC {
		return nil
	}
	if a.hasCC && cc != (a.lastCC+1)&0x0F {
		// discontinuity: the section in progress cannot be completed
		a.reset()
	}
	a.hasCC = true
	a.lastCC = cc

	var out [][]byte

	if pusi {
		pointer := int(payload[0])
		if 1+pointer > len(payload) {
			a.reset()
			return nil
		}

		// the end of the section in progress
		if a.active {
			out = a.feed(out, payload[1:1+pointer])
		}
		a.reset()

		// new sections
		return a.feed(out, payload[1+pointer:])
	}

	if !a.active {
		return nil
	}

	return a.feed(out, payload)
}

func (a *sectionAssembler) reset() {
	a.buf = a.buf[:0]
	a.active = false
}

// feed appends bytes and extracts all complete sections.
func (a *sectionAssembler) feed(out [][]byte, data []byte) [][]byte {
	for len(data) > 0 {
		if !a.active {
			// stuffing: the rest of the payload is unused
			if data[0] == 0xFF {
				return out
			}
			a.active = true
			a.buf = a.buf[:0]
		}

		// header
		if len(a.buf) < 3 {
			n := min(3-len(a.buf), len(data))
			a.buf = append(a.buf, data[:n]...)
			data = data[n:]
			if len(a.buf) < 3 {
				return out
			}
		}

		total := 3 + int(a.buf[1]&0x0F)<<8 + int(a.buf[2])
		n := min(total-len(a.buf), len(data))
		a.buf = append(a.buf, data[:n]...)
		data = data[n:]

		if len(a.buf) == total {
			out = append(out, append([]byte(nil), a.buf...))
			a.reset()
		}
	}

	return out
}

// parsePAT returns the PMT PIDs listed in a PAT section.
func parsePAT(sec []byte) ([]uint16, error) {
	if len(sec) < 12 || sec[0] != tableIDPAT {
		return nil, fmt.Errorf("invalid PAT")
	}
	if crc32MPEG(sec) != 0 {
		return nil, fmt.Errorf("invalid PAT CRC")
	}

	var pids []uint16
	for pos := 8; pos+4 <= len(sec)-4; pos += 4 {
		program := uint16(sec[pos])<<8 | uint16(sec[pos+1])
		if program == 0 { // network PID
			continue
		}
		pids = append(pids, uint16(sec[pos+2]&0x1F)<<8|uint16(sec[pos+3]))
	}
	return pids, nil
}

// rawPMTStream is an elementary stream of a raw PMT.
type rawPMTStream struct {
	StreamType  uint8
	PID         uint16
	Descriptors []byte // raw descriptor loop
}

// rawPMT is a parsed PMT that keeps descriptors as they have been received.
type rawPMT struct {
	ProgramNumber  uint16
	Version        uint8
	PCRPID         uint16
	ProgramInfo    []byte // raw descriptor loop
	Streams        []rawPMTStream
	currentNextInd bool
}

func (p *rawPMT) unmarshal(sec []byte) error {
	if len(sec) < 16 || sec[0] != tableIDPMT {
		return fmt.Errorf("invalid PMT")
	}
	if crc32MPEG(sec) != 0 {
		return fmt.Errorf("invalid PMT CRC")
	}

	p.ProgramNumber = uint16(sec[3])<<8 | uint16(sec[4])
	p.Version = (sec[5] >> 1) & 0x1F
	p.currentNextInd = sec[5]&1 != 0
	p.PCRPID = uint16(sec[8]&0x1F)<<8 | uint16(sec[9])

	end := len(sec) - 4 // CRC
	pil := int(sec[10]&0x0F)<<8 | int(sec[11])
	pos := 12
	if pos+pil > end {
		return fmt.Errorf("invalid PMT program_info_length")
	}
	p.ProgramInfo = append([]byte(nil), sec[pos:pos+pil]...)
	pos += pil

	p.Streams = nil
	for pos < end {
		if pos+5 > end {
			return fmt.Errorf("invalid PMT stream entry")
		}
		esil := int(sec[pos+3]&0x0F)<<8 | int(sec[pos+4])
		if pos+5+esil > end {
			return fmt.Errorf("invalid PMT ES_info_length")
		}
		p.Streams = append(p.Streams, rawPMTStream{
			StreamType:  sec[pos],
			PID:         uint16(sec[pos+1]&0x1F)<<8 | uint16(sec[pos+2]),
			Descriptors: append([]byte(nil), sec[pos+5:pos+5+esil]...),
		})
		pos += 5 + esil
	}

	return nil
}

func (p *rawPMT) marshal() ([]byte, error) {
	size := 12 + len(p.ProgramInfo) + 4
	for _, s := range p.Streams {
		size += 5 + len(s.Descriptors)
	}
	if size > maxSectionSize || len(p.ProgramInfo) > 0x3FF {
		return nil, fmt.Errorf("PMT is too big")
	}
	for _, s := range p.Streams {
		if len(s.Descriptors) > 0x3FF {
			return nil, fmt.Errorf("descriptors of PID %d are too big", s.PID)
		}
	}

	b := make([]byte, 0, size)
	sl := size - 3
	cn := byte(0)
	if p.currentNextInd {
		cn = 1
	}
	b = append(b,
		tableIDPMT,
		0xB0|byte(sl>>8), byte(sl), // section_syntax_indicator=1, '0', reserved=11
		byte(p.ProgramNumber>>8), byte(p.ProgramNumber),
		0xC0|(p.Version&0x1F)<<1|cn,
		0, 0, // section_number, last_section_number
		0xE0|byte(p.PCRPID>>8), byte(p.PCRPID),
		0xF0|byte(len(p.ProgramInfo)>>8), byte(len(p.ProgramInfo)),
	)
	b = append(b, p.ProgramInfo...)
	for _, s := range p.Streams {
		b = append(b,
			s.StreamType,
			0xE0|byte(s.PID>>8), byte(s.PID),
			0xF0|byte(len(s.Descriptors)>>8), byte(len(s.Descriptors)),
		)
		b = append(b, s.Descriptors...)
	}

	crc := crc32MPEG(b)
	return append(b, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc)), nil
}

// packetizeSection splits a section into 188-byte TS packets of the given PID.
// cc is the continuity counter of the first packet; the counter of the next packet is returned.
func packetizeSection(pid uint16, cc uint8, sec []byte) ([]byte, uint8) {
	out := make([]byte, 0, ((len(sec)+1)/184+1)*packetSize)
	first := true

	for len(sec) > 0 || first {
		pkt := make([]byte, packetSize)
		pkt[0] = syncByte
		pkt[1] = byte(pid >> 8 & 0x1F)
		if first {
			pkt[1] |= 0x40
		}
		pkt[2] = byte(pid)
		pkt[3] = 0x10 | (cc & 0x0F)
		cc = (cc + 1) & 0x0F

		pos := 4
		if first {
			pkt[pos] = 0 // pointer_field
			pos++
			first = false
		}

		n := copy(pkt[pos:], sec)
		sec = sec[n:]
		for i := pos + n; i < packetSize; i++ {
			pkt[i] = 0xFF
		}

		out = append(out, pkt...)
	}

	return out, cc
}
