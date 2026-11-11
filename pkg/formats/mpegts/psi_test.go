package mpegts

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSectionAssembler(t *testing.T) {
	a := testSCTE35Section(20)
	b := testSCTE35Section(500)
	c := testSCTE35Section(184) // exactly fills a packet payload after the pointer_field plus one byte

	t.Run("single", func(t *testing.T) {
		pkts, _ := packetizeSection(496, 0, a)
		var asm sectionAssembler
		require.Equal(t, [][]byte{a}, asm.push(pkts))
	})

	t.Run("multiple packets", func(t *testing.T) {
		pkts, _ := packetizeSection(496, 3, b)
		require.Equal(t, 3*188, len(pkts))
		var asm sectionAssembler
		var out [][]byte
		for i := 0; i < len(pkts); i += 188 {
			out = append(out, asm.push(pkts[i:i+188])...)
		}
		require.Equal(t, [][]byte{b}, out)
	})

	t.Run("two sections in one payload", func(t *testing.T) {
		pkt := make([]byte, 188)
		pkt[0], pkt[1], pkt[2], pkt[3] = 0x47, 0x41, 0xf0, 0x10
		n := copy(pkt[5:], append(append([]byte{}, a...), a...))
		for i := 5 + n; i < 188; i++ {
			pkt[i] = 0xff
		}
		var asm sectionAssembler
		require.Equal(t, [][]byte{a, a}, asm.push(pkt))
	})

	t.Run("pointer field with tail of previous section", func(t *testing.T) {
		pkts, _ := packetizeSection(496, 0, c)
		require.Equal(t, 2*188, len(pkts))
		// next section starts in the second packet, after the tail of c
		second := make([]byte, 188)
		copy(second, pkts[188:376])
		var asm sectionAssembler
		require.Empty(t, asm.push(pkts[:188]))
		// build packet with PUSI, pointer_field=1 (tail of c is 1 byte), then new section a
		p2 := make([]byte, 188)
		p2[0], p2[1], p2[2], p2[3] = 0x47, 0x41, 0xf0, 0x11
		p2[4] = 1
		p2[5] = c[len(c)-1]
		copy(p2[6:], a)
		for i := 6 + len(a); i < 188; i++ {
			p2[i] = 0xff
		}
		require.Equal(t, [][]byte{c, a}, asm.push(p2))
	})

	t.Run("discontinuity drops the section", func(t *testing.T) {
		pkts, _ := packetizeSection(496, 0, b)
		var asm sectionAssembler
		require.Empty(t, asm.push(pkts[:188]))
		require.Empty(t, asm.push(pkts[376:564-188])) // third packet, cc skips one
		require.Empty(t, asm.push(pkts[188:376]))
	})

	t.Run("duplicate packet ignored", func(t *testing.T) {
		pkts, _ := packetizeSection(496, 0, b)
		var asm sectionAssembler
		asm.push(pkts[:188])
		asm.push(pkts[:188])
		asm.push(pkts[188:376])
		require.Equal(t, [][]byte{b}, asm.push(pkts[376:]))
	})
}

func TestRawPMTRoundTrip(t *testing.T) {
	pmt := rawPMT{
		ProgramNumber:  1,
		Version:        5,
		PCRPID:         256,
		ProgramInfo:    testCUEIDescriptors,
		currentNextInd: true,
		Streams: []rawPMTStream{
			{StreamType: 0x1b, PID: 256},
			{StreamType: 0x15, PID: 258, Descriptors: testID3Descriptors},
			{StreamType: 0x86, PID: 496, Descriptors: testCUEIDescriptors},
		},
	}
	sec, err := pmt.marshal()
	require.NoError(t, err)
	require.Zero(t, crc32MPEG(sec))

	var got rawPMT
	require.NoError(t, got.unmarshal(sec))
	require.Equal(t, pmt, got)

	// PAT
	pat := []byte{0x00, 0xb0, 0x0d, 0x00, 0x01, 0xc1, 0x00, 0x00, 0x00, 0x01, 0xf0, 0x00}
	crc := crc32MPEG(pat)
	pat = append(pat, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	pids, err := parsePAT(pat)
	require.NoError(t, err)
	require.Equal(t, []uint16{0x1000}, pids)

	sec[len(sec)-1] ^= 1
	require.Error(t, (&rawPMT{}).unmarshal(sec))
}

func TestPMTRewriterMultiplePackets(t *testing.T) {
	// a PMT that needs more than one packet is split and keeps consecutive continuity counters
	big := bytes.Repeat([]byte{0x7f, 0x02, 0xaa, 0xbb}, 60) // 240 bytes of descriptors
	pmt := rawPMT{
		ProgramNumber: 1, PCRPID: 256, currentNextInd: true,
		Streams: []rawPMTStream{
			{StreamType: 0x1b, PID: 256},
			{StreamType: 0x15, PID: 258},
		},
	}
	sec, err := pmt.marshal()
	require.NoError(t, err)
	pkts, _ := packetizeSection(writerPMTPID, 7, sec)

	var out bytes.Buffer
	rw := &pmtRewriter{W: &out, programDescs: big, streamDescs: map[uint16][]byte{258: testID3Descriptors}}

	// write in odd sized chunks: packets must be re-aligned
	for i := 0; i < len(pkts); i += 100 {
		_, err = rw.Write(pkts[i:min(i+100, len(pkts))])
		require.NoError(t, err)
	}
	// a non-PMT packet is forwarded as it is
	other, _ := packetizeSection(300, 0, []byte{1, 2, 3})
	_, err = rw.Write(other)
	require.NoError(t, err)

	res := out.Bytes()
	require.Equal(t, 0, len(res)%188)
	require.Equal(t, other, res[len(res)-188:])

	var asm sectionAssembler
	var secs [][]byte
	for i := 0; i < len(res)-188; i += 188 {
		secs = append(secs, asm.push(res[i:i+188])...)
	}
	require.Len(t, secs, 1)

	var got rawPMT
	require.NoError(t, got.unmarshal(secs[0]))
	require.Equal(t, big, got.ProgramInfo)
	require.Equal(t, testID3Descriptors, got.Streams[1].Descriptors)
	require.Empty(t, got.Streams[0].Descriptors)
}
