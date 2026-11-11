package mpegts

import (
	"bytes"
	"io"
	"testing"

	"github.com/asticode/go-astits"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/substructs"
)

var (
	// metadata_descriptor: format 0xFF, identifier 'ID3 '
	testID3Descriptors = []byte{
		0x26, 0x0d, 0xff, 0xff, 0x49, 0x44, 0x33, 0x20,
		0xff, 0x00, 0x0f, 0x00, 0x00, 0x00, 0x00,
	}

	// registration_descriptor 'CUEI'
	testCUEIDescriptors = []byte{0x05, 0x04, 'C', 'U', 'E', 'I'}

	testID3Tag = append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0x0a}, bytes.Repeat([]byte{0xab}, 10)...)
)

func testSCTE35Section(size int) []byte {
	// table_id 0xFC, section_syntax_indicator=0, private=0, section_length
	sl := size - 3
	b := []byte{0xfc, 0x30 | byte(sl>>8), byte(sl)}
	for i := len(b); i < size-4; i++ {
		b = append(b, byte(i))
	}
	crc := crc32MPEG(b)
	return append(b, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func testOpaqueTracks() (video, audio, id3, scte *Track) {
	return &Track{
			PID:   256,
			Codec: &codecs.H264{},
		},
		&Track{
			PID:      257,
			Language: "eng",
			Codec: &codecs.MPEG4Audio{
				Config: mpeg4audio.AudioSpecificConfig{
					Type:          2,
					SampleRate:    48000,
					ChannelConfig: 2,
					ChannelCount:  2, //nolint:staticcheck
				},
			},
		},
		&Track{
			PID: 258,
			Codec: &codecs.Opaque{
				StreamType:  0x15,
				Descriptors: testID3Descriptors,
			},
		},
		&Track{
			PID: 496,
			Codec: &codecs.Opaque{
				StreamType:  0x86,
				Descriptors: testCUEIDescriptors,
				Sections:    true,
			},
		}
}

func TestOpaqueRoundTrip(t *testing.T) {
	video, audio, id3, scte := testOpaqueTracks()

	// one section that fits in a packet, one that spans over three packets
	sections := [][]byte{testSCTE35Section(30), testSCTE35Section(400)}

	var buf bytes.Buffer
	w := &Writer{
		W:                  &buf,
		Tracks:             []*Track{video, audio, id3, scte},
		ProgramDescriptors: testCUEIDescriptors,
	}
	require.NoError(t, w.Initialize())

	// opaque units written before the first frame are written after it
	require.NoError(t, w.WriteOpaque(id3, 1, 0xFC, testID3Tag))

	aac := []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
	for i := range 5 {
		pts := int64(90000 + i*3000)

		err := w.WriteH264(video, pts, pts, [][]byte{testH264SPS, {byte(h264.NALUTypeIDR), 1, 2, 3}})
		require.NoError(t, err)

		err = w.WriteMPEG4Audio(audio, pts, [][]byte{aac})
		require.NoError(t, err)

		err = w.WriteOpaque(id3, pts, 0xFC, testID3Tag)
		require.NoError(t, err)

		err = w.WriteOpaque(scte, pts, 0, sections[i%2])
		require.NoError(t, err)
	}

	// every packet is valid and continuity counters are consecutive for each PID
	raw := buf.Bytes()
	require.Zero(t, len(raw)%188)
	lastCC := map[uint16]uint8{}
	for i := 0; i < len(raw); i += 188 {
		pid, _, cc, _, ok := tsPacketPayload(raw[i : i+188])
		_ = ok
		if prev, seen := lastCC[pid]; seen && pid != 0x1000 && pid != 0 {
			// PES PIDs may carry adaptation-only packets without CC increment
			if cc != prev && cc != (prev+1)&0xF {
				t.Fatalf("PID %d: CC %d after %d", pid, cc, prev)
			}
		}
		lastCC[pid] = cc
	}

	r, err := NewReader(bytes.NewReader(raw))
	require.NoError(t, err)

	require.Equal(t, testCUEIDescriptors, r.ProgramDescriptors())

	tracks := r.Tracks()
	require.Equal(t, 4, len(tracks))
	require.Equal(t, &codecs.H264{}, tracks[0].Codec)
	require.Equal(t, "eng", tracks[1].Language)
	require.Equal(t, &Track{
		PID: 258,
		Codec: &codecs.Opaque{
			StreamType:  0x15,
			Descriptors: testID3Descriptors,
		},
	}, tracks[2])
	require.Equal(t, &Track{
		PID: 496,
		Codec: &codecs.Opaque{
			StreamType:  0x86,
			Descriptors: testCUEIDescriptors,
			Sections:    true,
		},
	}, tracks[3])

	var id3Got, scteGot [][]byte
	var id3PTS []int64
	var scteSeen bool

	r.OnDataH264(tracks[0], func(_ int64, _ int64, _ [][]byte) error { return nil })
	r.OnDataMPEG4Audio(tracks[1], func(_ int64, _ [][]byte) error { return nil })
	r.OnDataOpaque(tracks[2], func(pts int64, streamID uint8, data []byte) error {
		require.Equal(t, uint8(0xFC), streamID)
		id3PTS = append(id3PTS, pts)
		id3Got = append(id3Got, data)
		return nil
	})
	r.OnDataOpaque(tracks[3], func(pts int64, _ uint8, data []byte) error {
		require.GreaterOrEqual(t, pts, int64(90000))
		scteSeen = true
		scteGot = append(scteGot, data)
		return nil
	})

	for {
		err = r.Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}

	require.True(t, scteSeen)
	require.Equal(t, [][]byte{
		sections[0], sections[1], sections[0], sections[1], sections[0],
	}, scteGot)
	require.Equal(t, []int64{1, 90000, 93000, 96000, 99000, 102000}, id3PTS)
	for _, d := range id3Got {
		require.Equal(t, testID3Tag, d)
	}
}

func TestOpaqueNegativePTS(t *testing.T) {
	video, audio, id3, _ := testOpaqueTracks()

	var buf bytes.Buffer
	w := &Writer{W: &buf, Tracks: []*Track{video, audio, id3}}
	require.NoError(t, w.Initialize())

	require.NoError(t, w.WriteH264(video, 90000, 90000, [][]byte{testH264SPS, {byte(h264.NALUTypeIDR), 1}}))
	require.NoError(t, w.WriteOpaque(id3, -1920, 0xBD, testID3Tag))
	require.NoError(t, w.WriteH264(video, 93000, 93000, [][]byte{testH264SPS, {byte(h264.NALUTypeIDR), 1}}))
	require.NoError(t, w.WriteMPEG4Audio(audio, 93000, [][]byte{{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}}))

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)

	var got []int64
	r.OnDataOpaque(r.Tracks()[2], func(pts int64, streamID uint8, _ []byte) error {
		require.Equal(t, uint8(0xBD), streamID)
		got = append(got, pts)
		return nil
	})
	r.OnDataH264(r.Tracks()[0], func(int64, int64, [][]byte) error { return nil })
	r.OnDataMPEG4Audio(r.Tracks()[1], func(int64, [][]byte) error { return nil })
	for {
		err = r.Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}
	require.Equal(t, []int64{0x1FFFFFFFF + 1 - 1920}, got)
}

// opaque tracks must not change how other streams are detected.
func TestOpaqueUnsupportedStaysUnsupported(t *testing.T) {
	var buf bytes.Buffer
	mux := astits.NewMuxer(t.Context(), &buf)
	require.NoError(t, mux.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 256,
		StreamType:    astits.StreamTypeH264Video,
	}))
	require.NoError(t, mux.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 257,
		StreamType:    0x88, // not carried by any codec
	}))
	require.NoError(t, mux.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 258,
		StreamType:    astits.StreamTypeMetadata, // opaque, since without KLV descriptor
		ElementaryStreamDescriptors: []*astits.Descriptor{{
			Length:  1,
			Tag:     substructs.DescriptorTagMetadata,
			Unknown: &astits.DescriptorUnknown{Content: []byte{0, 1, 2, 3}},
		}},
	}))
	mux.SetPCRPID(256)
	for range 2 {
		_, err := mux.WriteTables()
		require.NoError(t, err)
	}

	r, err := NewReader(&buf)
	require.NoError(t, err)
	require.Equal(t, &codecs.H264{}, r.Tracks()[0].Codec)
	require.IsType(t, &codecs.Unsupported{}, r.Tracks()[1].Codec)
	require.IsType(t, &codecs.Opaque{}, r.Tracks()[2].Codec)
	require.Empty(t, r.ProgramDescriptors())
}
