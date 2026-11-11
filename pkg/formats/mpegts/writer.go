package mpegts

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/asticode/go-astits"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4video"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/substructs"
)

const (
	streamIDVideo    = 224
	streamIDAudio    = 192
	streamIDMetadata = 0xFC
	streamIDPrivate  = 0xBD

	// PCR is needed to read H265 tracks with VLC+VDPAU hardware encoder
	// (and is probably needed by other combinations too)
	dtsPCRDiff = (90000 / 10)
)

func opusMarshalSize(packets [][]byte) int {
	n := 0
	for _, packet := range packets {
		au := substructs.OpusAccessUnit{
			ControlHeader: substructs.OpusControlHeader{
				PayloadSize: len(packet),
			},
			Packet: packet,
		}
		n += au.MarshalSize()
	}
	return n
}

func mpeg1AudioMarshalSize(frames [][]byte) int {
	n := 0
	for _, frame := range frames {
		n += len(frame)
	}
	return n
}

const (
	maxPendingOpaqueUnits = 64
	maxPendingOpaqueBytes = 256 * 1024
)

type pendingOpaqueUnit struct {
	track    *Track
	pts      int64
	streamID uint8
	data     []byte
}

// Writer is a MPEG-TS writer.
type Writer struct {
	W      io.Writer
	Tracks []*Track

	// Raw descriptor loop of the program (program_info of the PMT), written verbatim.
	// It is optional.
	ProgramDescriptors []byte

	nextPID            uint16
	out                io.Writer
	opaqueCC           map[uint16]uint8
	pendingOpaque      []pendingOpaqueUnit
	pendingOpaqueBytes int
	mux                *astits.Muxer
	pcrCounter         int
	leadingTrackChosen bool
}

// Initialize initializes a Writer.
func (w *Writer) Initialize() error {
	w.nextPID = 256

	w.out = w.W

	streamDescs := make(map[uint16][]byte)
	for _, track := range w.Tracks {
		if track.PID == 0 {
			track.PID = w.nextPID
			w.nextPID++
		}
		if c, ok := track.Codec.(*codecs.Opaque); ok {
			streamDescs[track.PID] = c.Descriptors
		}
	}

	if len(streamDescs) != 0 || len(w.ProgramDescriptors) != 0 {
		w.out = &pmtRewriter{
			W:            w.W,
			programDescs: w.ProgramDescriptors,
			streamDescs:  streamDescs,
		}
	}

	w.opaqueCC = make(map[uint16]uint8)

	w.mux = astits.NewMuxer(
		context.Background(),
		w.out)

	for _, track := range w.Tracks {
		es, _ := track.marshal()

		err := w.mux.AddElementaryStream(*es)
		if err != nil {
			return err
		}
	}

	// WriteTables() is not necessary for normal operation
	// since it's called automatically when WriteData() is called with
	// * PID == PCRPID
	// * AdaptationField != nil
	// * RandomAccessIndicator = true
	// However, it can be called explicitly via Writer.WriteTables() when PAT/PMT
	// need to be written before any media data (e.g., for late-joining clients).

	return nil
}

// WriteTables explicitly writes PAT and PMT tables to the output.
// This is useful for cases where PAT/PMT need to be captured before any
// media data is written (e.g., to send to late-joining MPEG-TS clients).
func (w *Writer) WriteTables() (int, error) {
	// Ensure PCR PID is set before writing tables (required by astits)
	// If no leading track has been chosen yet, use the first track's PID
	if !w.leadingTrackChosen && len(w.Tracks) > 0 {
		w.mux.SetPCRPID(w.Tracks[0].PID)
	}
	return w.mux.WriteTables()
}

// NewWriter allocates a Writer.
//
// Deprecated: replaced by Writer.Initialize().
func NewWriter(
	bw io.Writer,
	tracks []*Track,
) *Writer {
	w := &Writer{
		W:      bw,
		Tracks: tracks,
	}
	err := w.Initialize()
	if err != nil {
		panic(err)
	}
	return w
}

// WriteH265 writes a H265 access unit.
func (w *Writer) WriteH265(
	track *Track,
	pts int64,
	dts int64,
	au [][]byte,
) error {
	// prepend an AUD. This is required by video.js, iOS, QuickTime
	if au[0][0] != byte(h265.NALUType_AUD_NUT<<1) {
		au = append([][]byte{
			{byte(h265.NALUType_AUD_NUT) << 1, 1, 0x50},
		}, au...)
	}

	enc, err := h264.AnnexB(au).Marshal()
	if err != nil {
		return err
	}

	randomAccess := h265.IsRandomAccess(au)

	return w.writeVideo(track, pts, dts, randomAccess, enc)
}

// WriteH264 writes a H264 access unit.
func (w *Writer) WriteH264(
	track *Track,
	pts int64,
	dts int64,
	au [][]byte,
) error {
	// prepend an AUD. This is required by video.js, iOS, QuickTime
	if au[0][0] != byte(h264.NALUTypeAccessUnitDelimiter) {
		au = append([][]byte{
			{byte(h264.NALUTypeAccessUnitDelimiter), 240},
		}, au...)
	}

	enc, err := h264.AnnexB(au).Marshal()
	if err != nil {
		return err
	}

	randomAccess := h264.IsRandomAccess(au)

	return w.writeVideo(track, pts, dts, randomAccess, enc)
}

// WriteMPEG4Video writes a MPEG-4 Video frame.
func (w *Writer) WriteMPEG4Video(
	track *Track,
	pts int64,
	frame []byte,
) error {
	randomAccess := bytes.Contains(frame, []byte{0, 0, 1, byte(mpeg4video.GroupOfVOPStartCode)})

	return w.writeVideo(track, pts, pts, randomAccess, frame)
}

// WriteMPEG1Video writes a MPEG-1/2 Video frame.
func (w *Writer) WriteMPEG1Video(
	track *Track,
	pts int64,
	frame []byte,
) error {
	randomAccess := bytes.Contains(frame, []byte{0, 0, 1, 0xB8})

	return w.writeVideo(track, pts, pts, randomAccess, frame)
}

// WriteOpus writes Opus packets.
func (w *Writer) WriteOpus(
	track *Track,
	pts int64,
	packets [][]byte,
) error {
	enc := make([]byte, opusMarshalSize(packets))
	n := 0
	for _, packet := range packets {
		au := substructs.OpusAccessUnit{
			ControlHeader: substructs.OpusControlHeader{
				PayloadSize: len(packet),
			},
			Packet: packet,
		}
		mn, err := au.MarshalTo(enc[n:])
		if err != nil {
			return err
		}
		n += mn
	}

	return w.writeAudio(track, pts, enc)
}

// WriteMPEG4Audio writes MPEG-4 Audio access units.
func (w *Writer) WriteMPEG4Audio(
	track *Track,
	pts int64,
	aus [][]byte,
) error {
	codec := track.Codec.(*codecs.MPEG4Audio)

	pkts := make(mpeg4audio.ADTSPackets, len(aus))

	for i, au := range aus {
		pkts[i] = &mpeg4audio.ADTSPacket{
			Type:          codec.Type,
			SampleRate:    codec.SampleRate,
			ChannelConfig: codec.ChannelConfig,
			ChannelCount:  codec.ChannelCount, //nolint:staticcheck
			AU:            au,
		}
	}

	enc, err := pkts.Marshal()
	if err != nil {
		return err
	}

	return w.writeAudio(track, pts, enc)
}

// WriteMPEG4AudioLATM writes MPEG-4 Audio LATM audioMuxElements.
func (w *Writer) WriteMPEG4AudioLATM(
	track *Track,
	pts int64,
	els [][]byte,
) error {
	enc, err := mpeg4audio.AudioSyncStream{
		AudioMuxElements: els,
	}.Marshal()
	if err != nil {
		return err
	}

	return w.writeAudio(track, pts, enc)
}

// WriteMPEG1Audio writes MPEG-1 Audio packets.
func (w *Writer) WriteMPEG1Audio(
	track *Track,
	pts int64,
	frames [][]byte,
) error {
	if !track.mp3Checked {
		var h mpeg1audio.FrameHeader
		err := h.Unmarshal(frames[0])
		if err != nil {
			return err
		}

		if h.MPEG2 {
			return fmt.Errorf("only MPEG-1 audio is supported")
		}

		track.mp3Checked = true
	}

	enc := make([]byte, mpeg1AudioMarshalSize(frames))
	n := 0
	for _, frame := range frames {
		n += copy(enc[n:], frame)
	}

	return w.writeAudio(track, pts, enc)
}

// WriteAC3 writes a AC-3 frame.
func (w *Writer) WriteAC3(
	track *Track,
	pts int64,
	frame []byte,
) error {
	return w.writeAudio(track, pts, frame)
}

// WriteEAC3 writes an E-AC-3 (Dolby Digital Plus) frame.
func (w *Writer) WriteEAC3(
	track *Track,
	pts int64,
	frame []byte,
) error {
	return w.writeAudio(track, pts, frame)
}

// WriteKLV writes a KLV unit.
func (w *Writer) WriteKLV(
	track *Track,
	pts int64,
	unit []byte,
) error {
	codec := track.Codec.(*codecs.KLV)

	if codec.Synchronous {
		out, err := writeMetadataAUWrapper(unit)
		if err != nil {
			return err
		}

		return w.writeData(track, true, pts, streamIDMetadata, out)
	}

	return w.writeData(track, false, 0, streamIDPrivate, unit)
}

// WriteDVBSubtitle writes DVB subtitle data.
func (w *Writer) WriteDVBSubtitle(
	track *Track,
	pts int64,
	data []byte,
) error {
	return w.writeData(track, true, pts, streamIDPrivate, data)
}

// WriteOpaque writes a unit of an Opaque track.
// For tracks that carry PES packets, data is the PES payload, that is written with a PTS
// (taken modulo 2^33, so that timestamps that are negative after a rebase are
// represented as they are for any other 33-bit timestamp) and with the given stream_id
// (when zero, private_stream_1 is used).
// For tracks that carry sections, data must be one or more complete sections,
// and pts is ignored since sections carry no timestamp.
// Units written before the first video or audio frame (opaque tracks cannot carry the PCR)
// are kept, up to 64 units and 256 KiB, and written after that frame; the exceeding ones are dropped.
func (w *Writer) WriteOpaque(
	track *Track,
	pts int64,
	streamID uint8,
	data []byte,
) error {
	codec := track.Codec.(*codecs.Opaque)

	if streamID == 0 {
		streamID = streamIDPrivate
	}

	if !w.leadingTrackChosen {
		// opaque tracks cannot carry the PCR, therefore units are kept until
		// a video or audio frame has been written.
		if len(w.pendingOpaque) < maxPendingOpaqueUnits && w.pendingOpaqueBytes+len(data) <= maxPendingOpaqueBytes {
			w.pendingOpaque = append(w.pendingOpaque, pendingOpaqueUnit{track, pts, streamID, append([]byte(nil), data...)})
			w.pendingOpaqueBytes += len(data)
		}
		return nil
	}

	return w.writeOpaque(track, codec, pts, streamID, data)
}

func (w *Writer) flushOpaque() error {
	units := w.pendingOpaque
	w.pendingOpaque = nil
	w.pendingOpaqueBytes = 0

	for _, u := range units {
		err := w.writeOpaque(u.track, u.track.Codec.(*codecs.Opaque), u.pts, u.streamID, u.data)
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) writeOpaque(track *Track, codec *codecs.Opaque, pts int64, streamID uint8, data []byte) error {
	if codec.Sections {
		// astits.Muxer supports only PES packets, therefore packets are written directly.
		// They are written through the same writer, in order to keep packets aligned.
		pkts, cc := packetizeSection(track.PID, w.opaqueCC[track.PID], data)
		w.opaqueCC[track.PID] = cc
		_, err := w.out.Write(pkts)
		return err
	}

	_, err := w.mux.WriteData(&astits.MuxerData{
		PID: track.PID,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				OptionalHeader: &astits.PESOptionalHeader{
					MarkerBits:      2,
					PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
					PTS:             &astits.ClockReference{Base: pts & 0x1FFFFFFFF},
				},
				StreamID: streamID,
			},
			Data: data,
		},
	})
	return err
}

func (w *Writer) writeVideo(
	track *Track,
	pts int64,
	dts int64,
	randomAccess bool,
	data []byte,
) error {
	if !w.leadingTrackChosen {
		w.leadingTrackChosen = true
		track.isLeading = true
		w.mux.SetPCRPID(track.PID)
	}

	var af *astits.PacketAdaptationField

	if randomAccess {
		af = &astits.PacketAdaptationField{}
		af.RandomAccessIndicator = true
	}

	if track.isLeading {
		if randomAccess || w.pcrCounter == 0 {
			if af == nil {
				af = &astits.PacketAdaptationField{}
			}
			af.HasPCR = true
			af.PCR = &astits.ClockReference{Base: dts - dtsPCRDiff}
			w.pcrCounter = 3
		}
		w.pcrCounter--
	}

	oh := &astits.PESOptionalHeader{
		MarkerBits: 2,
	}

	if dts == pts {
		oh.PTSDTSIndicator = astits.PTSDTSIndicatorOnlyPTS
		oh.PTS = &astits.ClockReference{Base: pts}
	} else {
		oh.PTSDTSIndicator = astits.PTSDTSIndicatorBothPresent
		oh.DTS = &astits.ClockReference{Base: dts}
		oh.PTS = &astits.ClockReference{Base: pts}
	}

	_, err := w.mux.WriteData(&astits.MuxerData{
		PID:             track.PID,
		AdaptationField: af,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				OptionalHeader: oh,
				StreamID:       streamIDVideo,
			},
			Data: data,
		},
	})
	if err != nil {
		return err
	}

	return w.flushOpaque()
}

func (w *Writer) writeAudio(track *Track, pts int64, data []byte) error {
	if !w.leadingTrackChosen {
		w.leadingTrackChosen = true
		track.isLeading = true
		w.mux.SetPCRPID(track.PID)
	}

	af := &astits.PacketAdaptationField{
		RandomAccessIndicator: true,
	}

	if track.isLeading {
		if w.pcrCounter == 0 {
			af.HasPCR = true
			af.PCR = &astits.ClockReference{Base: pts - dtsPCRDiff}
			w.pcrCounter = 3
		}
		w.pcrCounter--
	}

	_, err := w.mux.WriteData(&astits.MuxerData{
		PID:             track.PID,
		AdaptationField: af,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				OptionalHeader: &astits.PESOptionalHeader{
					MarkerBits:      2,
					PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
					PTS:             &astits.ClockReference{Base: pts},
				},
				StreamID: streamIDAudio,
			},
			Data: data,
		},
	})
	if err != nil {
		return err
	}

	return w.flushOpaque()
}

func (w *Writer) writeData(track *Track, hasPTS bool, pts int64, streamID uint8, data []byte) error {
	if !w.leadingTrackChosen {
		w.leadingTrackChosen = true
		track.isLeading = true
		w.mux.SetPCRPID(track.PID)
	}

	af := &astits.PacketAdaptationField{
		RandomAccessIndicator: true,
	}

	if track.isLeading {
		if w.pcrCounter == 0 {
			af.HasPCR = true
			af.PCR = &astits.ClockReference{Base: pts - dtsPCRDiff}
			w.pcrCounter = 3
		}
		w.pcrCounter--
	}

	oh := &astits.PESOptionalHeader{
		MarkerBits: 2,
	}

	if hasPTS {
		oh.PTSDTSIndicator = astits.PTSDTSIndicatorOnlyPTS
		oh.PTS = &astits.ClockReference{Base: pts}
	}

	header := &astits.PESHeader{
		StreamID:       streamID,
		OptionalHeader: oh,
	}

	_, err := w.mux.WriteData(&astits.MuxerData{
		PID:             track.PID,
		AdaptationField: af,
		PES: &astits.PESData{
			Header: header,
			Data:   data,
		},
	})
	if err != nil {
		return err
	}

	return w.flushOpaque()
}
