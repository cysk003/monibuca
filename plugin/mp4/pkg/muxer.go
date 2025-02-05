package mp4

import (
	"encoding/binary"
	"fmt"
	"io"

	"m7s.live/v5/pkg"
	. "m7s.live/v5/plugin/mp4/pkg/box"
)

const (
	FLAG_FRAGMENT Flag = (1 << 1)
	FLAG_KEYFRAME Flag = (1 << 3)
	FLAG_CUSTOM   Flag = (1 << 5)
	FLAG_DASH     Flag = (1 << 11)
)

type (
	Flag uint32

	Muxer struct {
		nextTrackId    uint32
		nextFragmentId uint32
		CurrentOffset  int64
		Tracks         map[uint32]*Track
		Flag
		fragDuration uint32
		moov         *BasicBox
		mdatOffset   uint64
		mdatSize     uint64
	}
	FileMuxer struct {
		*Muxer
		io.ReadWriteSeeker
	}
	FMP4Muxer struct {
		FileMuxer
	}
)

func (m Muxer) isFragment() bool {
	return (m.Flag & FLAG_FRAGMENT) != 0
}

func (m Muxer) isDash() bool {
	return (m.Flag & FLAG_DASH) != 0
}

func (m Muxer) has(flag Flag) bool {
	return (m.Flag & flag) != 0
}

func NewFileMuxer(w io.ReadWriteSeeker) (muxer *FileMuxer, err error) {
	muxer = &FileMuxer{
		ReadWriteSeeker: w,
		Muxer:           NewMuxer(0),
	}
	err = muxer.WriteInitSegment(w)
	if err != nil {
		return nil, err
	}
	err = muxer.WriteEmptyMdat(w)
	if err != nil {
		return nil, err
	}
	return
}

func NewFMP4Muxer(w io.ReadWriteSeeker) *FMP4Muxer {
	muxer := &FMP4Muxer{
		FileMuxer: FileMuxer{
			ReadWriteSeeker: w,
			Muxer:           NewMuxer(FLAG_FRAGMENT),
		},
	}
	muxer.fragDuration = 2000 // Default 2 seconds fragment duration

	// Write initialization segment immediately
	if err := muxer.WriteInitSegment(w); err != nil {
		panic(err)
	}

	return muxer
}

func NewMuxer(flag Flag) *Muxer {
	return &Muxer{
		nextTrackId:    1,
		nextFragmentId: 1,
		Tracks:         make(map[uint32]*Track),
		Flag:           flag,
	}
}

func (m *Muxer) WriteInitSegment(w io.Writer) (err error) {
	var n int
	var ftypBox []byte
	if m.isFragment() {
		// 对于 FMP4,使用 iso5 作为主品牌,兼容 iso5, iso6, mp41
		ftypBox = MakeFtypBox(TypeISO5, 0x200, TypeISO5, TypeISO6, TypeMP41)
	} else {
		// 对于普通 MP4,使用 isom 作为主品牌
		ftypBox = MakeFtypBox(TypeISOM, 0x200, TypeISOM, TypeISO2, TypeAVC1, TypeMP41)
	}
	n, err = w.Write(ftypBox)
	if err != nil {
		return
	}
	m.CurrentOffset = int64(n)

	// Only write free box for non-fragmented MP4
	if !m.isFragment() {
		// Write a free box with enough space for moov box
		freeSize := uint64(m.GetMoovSize())
		free := &FreeBox{Data: make([]byte, freeSize-BasicBoxLen)}
		n, err = w.Write(free.Encode())
		if err != nil {
			return
		}
		m.CurrentOffset += int64(n)
	}
	return
}

func (m *Muxer) WriteEmptyMdat(w io.Writer) (err error) {
	// Write mdat box header with initial size
	mdat := MediaDataBox(0)
	mdatlen, mdatBox := mdat.Encode()
	m.mdatOffset = uint64(m.CurrentOffset)
	m.mdatSize = 0
	var n int
	n, err = w.Write(mdatBox[0:mdatlen])
	if err != nil {
		return
	}
	m.CurrentOffset += int64(n)
	fmt.Printf("WriteEmptyMdat: wrote mdat box header at offset %d, size %d\n", m.mdatOffset, mdatlen)
	return
}

func (m *Muxer) AddTrack(cid MP4_CODEC_TYPE) *Track {
	track := &Track{
		Cid:       cid,
		TrackId:   m.nextTrackId,
		Timescale: 1000,
	}
	if m.isFragment() || m.isDash() {
		track.writer = NewFmp4WriterSeeker(1024 * 1024)
	}
	m.Tracks[m.nextTrackId] = track
	m.nextTrackId++
	return track
}

func (m *FMP4Muxer) ReWriteWithMoov(temp io.WriteSeeker) error {
	return pkg.ErrSkip
}

func (m *FMP4Muxer) WriteSample(t *Track, sample Sample) (err error) {
	// For fragmented MP4, write to track's buffer
	fmt.Printf("FMP4Muxer.WriteSample: writing sample, size %d\n", len(sample.Data))
	fmt.Printf("Sample data: % x\n", sample.Data[:min(32, len(sample.Data))])
	err = m.Muxer.WriteSample(m, t, sample)
	if err != nil {
		return
	}
	// For fragmented MP4, check if we should create a new fragment
	if sample.KeyFrame && t.Duration >= m.fragDuration {
		err = m.flushFragment()
	}
	return
}

func (m *FileMuxer) WriteSample(t *Track, sample Sample) error {
	return m.Muxer.WriteSample(m, t, sample)
}

func (m *Muxer) WriteSample(w io.Writer, t *Track, sample Sample) (err error) {
	if m.isFragment() {
		// For fragmented MP4, write to track's buffer
		if sample.Offset, err = t.writer.Seek(0, io.SeekCurrent); err != nil {
			return
		}
		if sample.Size, err = t.writer.Write(sample.Data); err != nil {
			return
		}
	} else {
		// For regular MP4, write directly to output
		sample.Offset = m.CurrentOffset - int64(m.mdatOffset) + 8 // Adjust offset to be relative to mdat data start
		sample.Size, err = w.Write(sample.Data)
		if err != nil {
			return
		}
		m.CurrentOffset += int64(sample.Size)
		m.mdatSize += uint64(sample.Size)
	}
	sample.Data = nil
	t.AddSampleEntry(sample)
	return
}

func (m *FileMuxer) reWriteMdatSize() (err error) {
	// Update mdat box size
	mdat := MediaDataBox(m.mdatSize)
	mdatlen, mdatBox := mdat.Encode()
	if _, err = m.Seek(int64(m.mdatOffset), io.SeekStart); err != nil {
		return
	}
	if _, err = m.Write(mdatBox[:mdatlen]); err != nil {
		return
	}
	if _, err = m.Seek(m.CurrentOffset, io.SeekStart); err != nil {
		return
	}
	return
}

func (m *FileMuxer) ReWriteWithMoov(f io.WriteSeeker) (err error) {
	_, err = m.Seek(0, io.SeekStart)
	if err != nil {
		return
	}
	_, err = io.CopyN(f, m, int64(m.mdatOffset)-16)
	if err != nil {
		return
	}
	for _, track := range m.Tracks {
		for i := range len(track.Samplelist) {
			track.Samplelist[i].Offset += int64(m.moov.Size)
		}
	}
	err = m.WriteMoov(f)
	if err != nil {
		return
	}
	_, err = io.CopyN(f, m, int64(m.mdatSize)+16)
	return
}

func (m *Muxer) makeMvex() []byte {
	mvex := BasicBox{Type: TypeMVEX}
	trexs := make([]byte, 0, 64)
	for i := uint32(1); i < m.nextTrackId; i++ {
		if track := m.Tracks[i]; track != nil {
			trex := NewTrackExtendsBox(track.TrackId)
			trex.DefaultSampleDescriptionIndex = 1
			trex.DefaultSampleDuration = 0
			trex.DefaultSampleSize = 0
			if track.Cid.IsVideo() {
				trex.DefaultSampleFlags = 0x00010000 // NonSyncSampleFlags in mp4ff
			} else {
				trex.DefaultSampleFlags = 0x02000000 // SyncSampleFlags in mp4ff
			}
			_, boxData := trex.Encode()
			trexs = append(trexs, boxData...)
		}
	}
	mvex.Size = 8 + uint64(len(trexs))
	offset, mvexBox := mvex.Encode()
	copy(mvexBox[offset:], trexs)
	return mvexBox
}

func (m *Muxer) makeTrak(track *Track) []byte {
	edts := []byte{}
	if m.isDash() || m.isFragment() {
		// track.makeEmptyStblTable()
	} else {
		if len(track.Samplelist) > 0 {
			track.makeStblBox()
			edts = track.makeEdtsBox()
		}
	}

	tkhd := track.makeTkhdBox()
	mdia := track.makeMdiaBox()

	trak := BasicBox{Type: TypeTRAK}
	trak.Size = 8 + uint64(len(tkhd)+len(edts)+len(mdia))
	offset, trakBox := trak.Encode()
	copy(trakBox[offset:], tkhd)
	offset += len(tkhd)
	copy(trakBox[offset:], edts)
	offset += len(edts)
	copy(trakBox[offset:], mdia)
	return trakBox
}

func (m *Muxer) GetMoovSize() int {
	moovsize := FullBoxLen + 96
	if m.isDash() || m.isFragment() {
		moovsize += 64
	}
	traks := make([][]byte, len(m.Tracks))
	for i := uint32(1); i < m.nextTrackId; i++ {
		traks[i-1] = m.makeTrak(m.Tracks[i])
		moovsize += len(traks[i-1])
	}
	return int(8 + uint64(moovsize))
}

func (m *Muxer) WriteMoov(w io.Writer) (err error) {
	// Create mvhd box
	var mvhd []byte
	var mvex []byte
	if m.isDash() || m.isFragment() {
		mvhd = MakeMvhdBox(m.nextTrackId, 0)
		mvex = m.makeMvex()
	} else {
		maxdurtaion := uint32(0)
		for _, track := range m.Tracks {
			if maxdurtaion < track.Duration {
				maxdurtaion = track.Duration
			}
		}
		mvhd = MakeMvhdBox(m.nextTrackId, maxdurtaion)
	}

	// Create trak boxes
	moovsize := len(mvhd)
	if mvex != nil {
		moovsize += len(mvex)
	}
	traks := make([][]byte, 0, len(m.Tracks))
	for i := uint32(1); i < m.nextTrackId; i++ {
		if track := m.Tracks[i]; track != nil {
			// Create tkhd box
			tkhd := track.makeTkhdBox()

			// Create mdia box
			mdia := track.makeMdiaBox()

			// Create edts box if needed
			edts := track.makeEdtsBox()

			// Create stbl box with all required sub-boxes
			if !m.isDash() && !m.isFragment() && len(track.Samplelist) > 0 {
				track.makeStblBox()
			}

			// Create trak box
			trak := BasicBox{Type: TypeTRAK}
			traksize := len(tkhd) + len(mdia)
			if len(edts) > 0 {
				traksize += len(edts)
			}
			trak.Size = uint64(traksize + 8) // Add 8 for trak box header
			offset, trakBox := trak.Encode()
			copy(trakBox[offset:], tkhd)
			offset += len(tkhd)
			if len(edts) > 0 {
				copy(trakBox[offset:], edts)
				offset += len(edts)
			}
			copy(trakBox[offset:], mdia)
			traks = append(traks, trakBox)
			moovsize += len(trakBox)
		}
	}

	// Create moov box
	moov := BasicBox{Type: TypeMOOV}
	moov.Size = 8 + uint64(moovsize)
	offset, moovBox := moov.Encode()
	copy(moovBox[offset:], mvhd)
	offset += len(mvhd)
	for _, trak := range traks {
		copy(moovBox[offset:], trak)
		offset += len(trak)
	}
	if mvex != nil {
		copy(moovBox[offset:], mvex)
	}

	// Write moov box
	_, err = w.Write(moovBox)
	m.moov = &moov
	m.CurrentOffset += int64(moov.Size)
	return
}

func (m *FileMuxer) WriteTrailer() (err error) {
	if err = m.reWriteMdatSize(); err != nil {
		return err
	}
	return m.WriteMoov(m)
}

func (m *FMP4Muxer) WriteTrailer() (err error) {
	// Flush any remaining samples
	if err = m.flushFragment(); err != nil {
		return err
	}

	// Write mfra box
	mfraSize := 0
	tfras := make([][]byte, len(m.Tracks))
	for i := uint32(1); i < m.nextTrackId; i++ {
		if track := m.Tracks[i]; track != nil && len(track.fragments) > 0 {
			tfras[i-1] = track.makeTfraBox()
			mfraSize += len(tfras[i-1])
		}
	}

	// Only write mfra if we have fragments
	if mfraSize > 0 {
		mfro := MakeMfroBox(uint32(mfraSize) + 16)
		mfraSize += len(mfro)
		mfra := BasicBox{Type: TypeMFRA}
		mfra.Size = 8 + uint64(mfraSize)
		offset, mfraBox := mfra.Encode()
		for _, tfra := range tfras {
			if tfra == nil {
				continue
			}
			copy(mfraBox[offset:], tfra)
			offset += len(tfra)
		}
		copy(mfraBox[offset:], mfro)
		if _, err = m.Write(mfraBox); err != nil {
			return err
		}
	}

	// Clean up any remaining buffers
	for i := uint32(1); i < m.nextTrackId; i++ {
		if track := m.Tracks[i]; track != nil && track.writer != nil {
			if ws, ok := track.writer.(*Fmp4WriterSeeker); ok {
				ws.Buffer = nil
			}
		}
	}
	return nil
}

func (m *FMP4Muxer) flushFragment() (err error) {
	// Check if there are any samples to write
	hasSamples := false
	for i := uint32(1); i < m.nextTrackId; i++ {
		if len(m.Tracks[i].Samplelist) > 0 {
			hasSamples = true
			break
		}
	}
	if !hasSamples {
		return nil
	}

	// Write moov box if not written yet
	if m.moov == nil {
		if err = m.WriteMoov(m); err != nil {
			return err
		}
	}

	// Get current file position for moof offset
	var moofOffset int64
	if moofOffset, err = m.Seek(0, io.SeekCurrent); err != nil {
		return err
	}

	// Calculate mdat size first
	var mdatSize uint64 = 8 // mdat box header
	for i := uint32(1); i < m.nextTrackId; i++ {
		if len(m.Tracks[i].Samplelist) == 0 {
			continue
		}
		ws := m.Tracks[i].writer.(*Fmp4WriterSeeker)
		mdatSize += uint64(len(ws.Buffer))
	}

	// Write moof box
	mfhd := MakeMfhdBox(m.nextFragmentId)
	trafs := make([][]byte, len(m.Tracks))
	moofSize := len(mfhd)
	trunOffsets := make([]int, len(m.Tracks)) // track index -> trun data_offset position in moof box
	var boxOffset int = 8 + len(mfhd)         // 8 for moof header
	for i := uint32(1); i < m.nextTrackId; i++ {
		if len(m.Tracks[i].Samplelist) == 0 {
			continue
		}
		track := m.Tracks[i]
		// 传递 moof 偏移和 mdat 大小
		traf := track.makeTraf(&trunOffsets[int(i-1)]) // +8 for moof box header
		// Record trun data_offset position: current offset + 16 (after trun header)
		trafs[i-1] = traf
		trunOffsets[int(i-1)] += boxOffset
		boxOffset += len(traf)
		moofSize += len(traf)
	}

	// Write moof box
	moof := BasicBox{Type: TypeMOOF}
	moof.Size = uint64(moofSize + 8) // Add 8 for moof box header
	offset, moofBox := moof.Encode()
	copy(moofBox[offset:], mfhd)
	offset += len(mfhd)
	for i, traf := range trafs {
		if traf == nil {
			continue
		}
		copy(moofBox[offset:], traf)
		// Update trun data_offset
		binary.BigEndian.PutUint32(moofBox[trunOffsets[i]:trunOffsets[i]+4], uint32(moof.Size)+8) // +8 for mdat header
		offset += len(traf)
	}

	if _, err = m.Write(moofBox); err != nil {
		return err
	}

	// Write mdat box
	mdat := BasicBox{Type: TypeMDAT}
	mdat.Size = mdatSize
	offset, mdatBox := mdat.Encode()
	if _, err = m.Write(mdatBox[:offset]); err != nil {
		return err
	}

	// Write sample data
	var sampleOffset int64 = 0
	for i := uint32(1); i < m.nextTrackId; i++ {
		if len(m.Tracks[i].Samplelist) == 0 {
			continue
		}
		track := m.Tracks[i]
		ws := track.writer.(*Fmp4WriterSeeker)

		// Update sample offsets relative to mdat start
		for j := range track.Samplelist {
			track.Samplelist[j].Offset = sampleOffset
			sampleOffset += int64(track.Samplelist[j].Size)
		}

		if _, err = m.Write(ws.Buffer); err != nil {
			return err
		}

		// Record fragment info
		if len(track.Samplelist) > 0 {
			firstPts := track.Samplelist[0].PTS
			firstDts := track.Samplelist[0].DTS
			lastPts := track.Samplelist[len(track.Samplelist)-1].PTS
			lastDts := track.Samplelist[len(track.Samplelist)-1].DTS
			frag := Fragment{
				Offset:   uint64(moofOffset),
				Duration: track.Duration,
				FirstDts: firstDts,
				FirstPts: firstPts,
				LastPts:  lastPts,
				LastDts:  lastDts,
			}
			track.fragments = append(track.fragments, frag)
		}

		// Clear track buffers
		ws.Buffer = ws.Buffer[:0]
		ws.Offset = 0
		track.Samplelist = track.Samplelist[:0]
		track.Duration = 0
	}

	m.nextFragmentId++
	return nil
}

// SetFragmentDuration sets the target duration for each fragment in milliseconds
func (m *FMP4Muxer) SetFragmentDuration(duration uint32) {
	m.fragDuration = duration
}
