package mpegts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// sectionsPacket returns a PUSI packet of the PID that carries as many copies of the section as fit.
func sectionsPacket(pid uint16, cc uint8, sec []byte) []byte {
	pkt := make([]byte, packetSize)
	pkt[0], pkt[1], pkt[2], pkt[3] = syncByte, 0x40|byte(pid>>8), byte(pid), 0x10|cc&0x0F
	pos := 5 // pointer_field = 0
	for pos+len(sec) <= packetSize {
		pos += copy(pkt[pos:], sec)
	}
	for ; pos < packetSize; pos++ {
		pkt[pos] = 0xff
	}
	return pkt
}

func TestPTSInterceptorSectionsAreBounded(t *testing.T) {
	const pid = 0x86

	t.Run("short sections are dropped", func(t *testing.T) {
		r := &ptsInterceptor{sectionAsms: map[uint16]*sectionAssembler{pid: {}}}
		short := []byte{0x86, 0xb0, 0x00} // section_length 0: cannot hold a CRC
		for i := 0; i < 100000; i++ {
			r.capturePacket(sectionsPacket(pid, uint8(i), short))
		}
		require.Empty(t, r.sections)
		require.Zero(t, r.sectionBytes)
		require.Equal(t, uint64(100000*61), r.droppedSections)
	})

	t.Run("valid sections are capped", func(t *testing.T) {
		r := &ptsInterceptor{sectionAsms: map[uint16]*sectionAssembler{pid: {}}}
		sec := testSCTE35Section(20)
		for i := 0; i < 100000; i++ {
			r.capturePacket(sectionsPacket(pid, uint8(i), sec))
		}
		require.LessOrEqual(t, len(r.sections), maxPendingSections)
		require.LessOrEqual(t, r.sectionBytes, maxPendingSectionBytes)
		require.NotZero(t, r.droppedSections)
	})

	t.Run("large sections are capped by size", func(t *testing.T) {
		r := &ptsInterceptor{sectionAsms: map[uint16]*sectionAssembler{pid: {}}}
		sec := testSCTE35Section(3000)
		pkts, _ := packetizeSection(pid, 0, sec)
		for i := 0; i < 1000; i++ {
			for j := 0; j < len(pkts); j += packetSize {
				p := append([]byte(nil), pkts[j:j+packetSize]...)
				p[3] = p[3]&0xF0 | byte(i*len(pkts)/packetSize+j/packetSize)&0x0F
				r.capturePacket(p)
			}
		}
		require.LessOrEqual(t, r.sectionBytes, maxPendingSectionBytes)
		require.Less(t, len(r.sections), 64)
		require.NotZero(t, r.droppedSections)
	})
}
