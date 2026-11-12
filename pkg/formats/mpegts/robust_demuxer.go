package mpegts

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astits"
)

// this is a wrapper around astits.Demuxer with the following differences:
// - non-fatal errors are not returned, but routed to OnDecodeError.
// - last PTS is intercepted and returned.

const (
	packetSize = 188
	syncByte   = 0x47
)

// https://github.com/asticode/go-astits/blob/f593538dc04ea116394690b6cf3c916e16f0195f/data.go#L120
func isPESPayload(i []byte) bool {
	return uint32(i[0])<<16|uint32(i[1])<<8|uint32(i[2]) == 1
}

// https://github.com/asticode/go-astits/blob/f593538dc04ea116394690b6cf3c916e16f0195f/data_pes.go#L150
func hasPESOptionalHeader(streamID uint8) bool {
	return streamID != astits.StreamIDPaddingStream && streamID != astits.StreamIDPrivateStream2
}

func parseClock(b []byte) int64 {
	return int64((((uint64(b[0]) >> 1) & 0x7) << 30) | (uint64(b[1]) << 22) |
		(((uint64(b[2]) >> 1) & 0x7f) << 15) | (uint64(b[3]) << 7) | ((uint64(b[4]) >> 1) & 0x7f))
}

type wrappedError struct {
	w error
}

func (w wrappedError) Error() string {
	return w.w.Error()
}

type errorWrapper struct {
	R io.Reader
}

func (r *errorWrapper) Read(p []byte) (int, error) {
	n, err := r.R.Read(p)
	if err != nil {
		// keep io.EOF and io.ErrUnexpectedEOF untouched
		// since they are needed by astits to handle the last packet of a stream.
		// they are transformed into astits.ErrNoMorePackets.
		// https://github.com/asticode/go-astits/blob/7c2bf6b71173d24632371faa01f28a9122db6382/packet_buffer.go#L131
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			err = &wrappedError{w: err}
		}
	}
	return n, err
}

// capturedSection is a section received on a PID that has been registered with captureSections.
type capturedSection struct {
	pid     uint16
	lastPTS *int64
	data    []byte
}

type ptsInterceptor struct {
	R io.Reader

	hasLastPTS bool
	lastPTS    int64

	// raw PSI capture, used to read PMT descriptors verbatim.
	capturePSI bool
	patAsm     sectionAssembler
	pmtAsms    map[uint16]*sectionAssembler
	rawPMTs    map[uint16][]byte

	// private section capture, used for streams that carry sections.
	sectionAsms map[uint16]*sectionAssembler
	sections    []capturedSection

	// sections are drained by robustDemuxer.nextData once a full PAT/PMT/PES completes, so a
	// publisher that only sends sections could grow them without bound: they are capped.
	sectionBytes    int
	droppedSections uint64
}

const (
	// maximum number of captured sections that wait to be drained.
	maxPendingSections = 64
	// maximum total size of captured sections that wait to be drained.
	maxPendingSectionBytes = 64 * 1024
	// a section that cannot hold the 3-byte header and the CRC32 is invalid.
	minSectionSize = 3 + 4
)

func (r *ptsInterceptor) capturePacket(p []byte) {
	pid := uint16(p[1]&0x1F)<<8 | uint16(p[2])

	if r.capturePSI {
		if pid == pidPAT {
			for _, sec := range r.patAsm.push(p) {
				pmtPIDs, err := parsePAT(sec)
				if err != nil {
					continue
				}
				for _, pmtPID := range pmtPIDs {
					if _, ok := r.pmtAsms[pmtPID]; !ok {
						r.pmtAsms[pmtPID] = &sectionAssembler{}
					}
				}
			}
		} else if asm, ok := r.pmtAsms[pid]; ok {
			for _, sec := range asm.push(p) {
				if len(sec) > 0 && sec[0] == tableIDPMT && crc32MPEG(sec) == 0 {
					r.rawPMTs[pid] = sec
				}
			}
		}
	}

	if asm, ok := r.sectionAsms[pid]; ok {
		for _, sec := range asm.push(p) {
			if len(sec) < minSectionSize ||
				len(r.sections) >= maxPendingSections ||
				r.sectionBytes+len(sec) > maxPendingSectionBytes {
				r.droppedSections++
				continue
			}
			r.sectionBytes += len(sec)
			cs := capturedSection{pid: pid, data: sec}
			if r.hasLastPTS {
				pts := r.lastPTS
				cs.lastPTS = &pts
			}
			r.sections = append(r.sections, cs)
		}
	}
}

func (r *ptsInterceptor) Read(p []byte) (int, error) {
	if len(p) != packetSize {
		return 0, fmt.Errorf("unsupported")
	}

	_, err := io.ReadFull(r.R, p)
	if err != nil {
		return 0, err
	}

	if p[0] == syncByte {
		if r.capturePSI || len(r.sectionAsms) != 0 {
			r.capturePacket(p)
		}

		hasPayload := (p[3]&0x10 > 0)
		payloadStartIndicator := (p[1]&0x40 > 0)

		if payloadStartIndicator && hasPayload {
			payloadPos := 4
			hasAdaptationField := (p[3]&0x20 > 0)

			if hasAdaptationField {
				adaptationFieldLength := int(p[4])
				payloadPos += 1 + adaptationFieldLength
			}

			if payloadPos <= (packetSize - 14) {
				payload := p[payloadPos:]

				if isPESPayload(payload) {
					streamID := payload[3]

					if hasPESOptionalHeader(streamID) {
						ptsDTSIndicator := (payload[7] >> 6) & 0x03

						if ptsDTSIndicator == astits.PTSDTSIndicatorOnlyPTS ||
							ptsDTSIndicator == astits.PTSDTSIndicatorBothPresent {
							r.hasLastPTS = true
							r.lastPTS = parseClock(payload[9:14])
						}
					}
				}
			}
		}
	}

	return packetSize, nil
}

type robustDemuxerData struct {
	lastPTS *int64
	PID     uint16
	PMT     *astits.PMTData
	PES     *astits.PESData
	Section []byte // complete private section, only for PIDs registered with captureSections
}

type robustDemuxer struct {
	R             io.Reader
	OnDecodeError func(err error)

	dem            *astits.Demuxer
	ptsInterceptor *ptsInterceptor
	pending        []*robustDemuxerData
	pendingErr     error

	reportedDropped uint64
}

// DroppedSections returns the number of captured private sections that have been dropped
// because they were too short to hold a CRC or because the capture buffer was full.
func (d *robustDemuxer) DroppedSections() uint64 {
	return d.ptsInterceptor.droppedSections
}

func (d *robustDemuxer) initialize() {
	if d.OnDecodeError == nil {
		d.OnDecodeError = func(_ error) {}
	}

	errorWrapper := &errorWrapper{R: d.R}
	d.ptsInterceptor = &ptsInterceptor{R: errorWrapper}

	d.dem = astits.NewDemuxer(
		context.Background(),
		d.ptsInterceptor,
		astits.DemuxerOptPacketSize(packetSize),
	)
}

// enablePSICapture makes the demuxer keep the raw PMT of each program.
func (d *robustDemuxer) enablePSICapture() {
	d.ptsInterceptor.capturePSI = true
	d.ptsInterceptor.pmtAsms = make(map[uint16]*sectionAssembler)
	d.ptsInterceptor.rawPMTs = make(map[uint16][]byte)
}

// captureSections makes the demuxer return the sections that are carried by the given PIDs.
// They are returned in robustDemuxerData.Section, in the order they are received,
// and with the PTS of the last PES that preceded them.
func (d *robustDemuxer) captureSections(pids []uint16) {
	d.ptsInterceptor.sectionAsms = make(map[uint16]*sectionAssembler)
	for _, pid := range pids {
		d.ptsInterceptor.sectionAsms[pid] = &sectionAssembler{}
	}
}

func (d *robustDemuxer) nextData() (*robustDemuxerData, error) {
	if len(d.pending) != 0 {
		data := d.pending[0]
		d.pending = d.pending[1:]
		return data, nil
	}

	if d.pendingErr != nil {
		return nil, d.pendingErr
	}

	data, err := d.nextDataInner()

	if n := d.ptsInterceptor.droppedSections; n != d.reportedDropped {
		d.OnDecodeError(fmt.Errorf("dropped %d private sections (too short or buffer full)", n-d.reportedDropped))
		d.reportedDropped = n
	}

	// sections completed while reading are returned after the data (that is a PES
	// completed by the packets that have just been read) and before an error.
	if len(d.ptsInterceptor.sections) != 0 {
		d.pending = d.pending[:0]
		for _, sec := range d.ptsInterceptor.sections {
			d.pending = append(d.pending, &robustDemuxerData{
				lastPTS: sec.lastPTS,
				PID:     sec.pid,
				Section: sec.data,
			})
		}
		d.ptsInterceptor.sections = nil
		d.ptsInterceptor.sectionBytes = 0

		if err != nil {
			d.pendingErr = err
			data = d.pending[0]
			d.pending = d.pending[1:]
			return data, nil
		}
	}

	return data, err
}

func (d *robustDemuxer) nextDataInner() (*robustDemuxerData, error) {
	for {
		data, err := d.dem.NextData()
		if err != nil {
			// astits.ErrNoMorePackets / io.EOF: this is fatal.
			if errors.Is(err, astits.ErrNoMorePackets) {
				return nil, io.EOF // convert back to io.EOF.
			}

			// error from underlying reader: this is fatal.
			if w, ok := errors.AsType[*wrappedError](err); ok {
				return nil, w.w
			}

			// error from MPEG-TS demuxer: this is not fatal.
			d.OnDecodeError(err)
			continue
		}

		return &robustDemuxerData{
			lastPTS: func() *int64 {
				if d.ptsInterceptor.hasLastPTS {
					pts := d.ptsInterceptor.lastPTS
					return &pts
				}
				return nil
			}(),
			PID: data.PID,
			PMT: data.PMT,
			PES: data.PES,
		}, nil
	}
}
