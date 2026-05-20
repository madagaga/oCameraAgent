package main

import (
	"bytes"
)

type AnnexBParser struct {
	startCode   []byte
	videoTS     uint32
	audioTS     uint32
	videoSeq    uint16
	audioSeq    uint16
	hasVideoSeq bool
	hasAudioSeq bool
	pps         []byte
	sps         []byte
	fuBuffer    *bytes.Buffer
	fuStarted   bool
}

const (
	NALU_RAW = iota
	NALU_AVCC
	NALU_ANNEXB
)

const (
	NALU_SEI = 6
	NALU_SPS = 7
	NALU_PPS = 8
	NALU_AUD = 9
)

func NewAnnexBParser() *AnnexBParser {
	Obj := &AnnexBParser{
		startCode: []byte{0x00, 0x00, 0x00, 0x01},
		fuBuffer:  bytes.NewBuffer([]byte{}),
	}
	return Obj
}

func (this *AnnexBParser) reset() {
	this.videoTS = 0
	this.audioTS = 0
	this.videoSeq = 0
	this.audioSeq = 0
	this.hasVideoSeq = false
	this.hasAudioSeq = false
	this.fuBuffer.Reset()
	this.fuStarted = false
	Debug("H264", "AnnexB parser reset")
}

func (this *AnnexBParser) handlePacket(data *[]byte) ([]byte, uint32) {
	content := *data

	if len(content) < 4 {
		return nil, 0 // Not enough data
	}

	// Assuming RTSP interleaved data
	channel := int(content[1])

	// The RTP packet starts at content[4:]
	rtpPacket := content[4:]

	if len(rtpPacket) < 12 {
		return nil, 0 // Not enough data for RTP header
	}

	// Parse RTP header
	firstByte := rtpPacket[0]
	padding := (firstByte>>5)&1 == 1
	extension := (firstByte>>4)&1 == 1
	CSRCCnt := int(firstByte & 0x0F)
	//secondByte := rtpPacket[1]
	//marker := (secondByte>>7)&1 == 1
	//payloadType := secondByte & 0x7F

	sequenceNumber := (uint16(rtpPacket[2]) << 8) + uint16(rtpPacket[3])
	timestamp := (uint32(rtpPacket[4]) << 24) + (uint32(rtpPacket[5]) << 16) + (uint32(rtpPacket[6]) << 8) + uint32(rtpPacket[7])
	//ssrc := (uint32(rtpPacket[8]) << 24) + (uint32(rtpPacket[9]) << 16) + (uint32(rtpPacket[10]) << 8) + uint32(rtpPacket[11])

	offset := 12

	// Skip CSRC identifiers if present
	if len(rtpPacket) < offset+4*CSRCCnt {
		return nil, 0 // Not enough data for CSRC list
	}
	offset += 4 * CSRCCnt

	// Skip extension header if present
	if extension {
		if len(rtpPacket) < offset+4 {
			return nil, 0 // Not enough data for extension header
		}
		extHeaderLength := (uint16(rtpPacket[offset+2]) << 8) + uint16(rtpPacket[offset+3])
		extHeaderLengthInBytes := int(extHeaderLength) * 4
		offset += 4 // Skip extension header
		if len(rtpPacket) < offset+extHeaderLengthInBytes {
			return nil, 0 // Not enough data for extension payload
		}
		offset += extHeaderLengthInBytes
	}

	// Adjust for padding
	end := len(rtpPacket)
	if padding {
		paddingLength := int(rtpPacket[end-1])
		if paddingLength > len(rtpPacket)-offset {
			return nil, 0 // Invalid padding length
		}
		end -= paddingLength
	}

	switch channel {
	case 0:
		if this.detectSequenceGap(0, sequenceNumber) {
			// Drop any in-progress FU-A if we detected RTP loss on the video channel.
			this.fuStarted = false
			this.fuBuffer.Reset()
		}
		// Check if this is the first packet, if so, initialize previous timestamp
		if this.videoTS == 0 {
			// Set a default offset for the previous timestamp to avoid initial zero value
			this.videoTS = timestamp
		}
		return this.parseNalu(rtpPacket[offset:end], timestamp)
	case 2:
		this.detectSequenceGap(2, sequenceNumber)
		// Check if this is the first packet, if so, initialize previous timestamp
		if this.audioTS == 0 {
			// Set a default offset for the previous timestamp to avoid initial zero value
			this.audioTS = timestamp
		}
		if end <= offset {
			return nil, 0
		}

		duration := timestamp - this.audioTS
		this.audioTS = timestamp
		return rtpPacket[offset:end], duration
	}

	return nil, 0
}

func (this *AnnexBParser) parseNalu(data []byte, timestamp uint32) ([]byte, uint32) {
	if len(data) == 0 {
		return nil, 0
	}

	naluType := data[0] & 0x1F

	// Reduce logging to decrease CPU usage - only for important types
	if naluType == 5 || naluType == NALU_SPS || naluType == NALU_PPS {
		Debug("H264", "NAL type: %d - timestamp: %d", naluType, timestamp)
	}

	switch {
	case naluType >= 1 && naluType <= 5:
		return this.handleNALU(naluType, data, timestamp)
	case naluType == NALU_SPS:
		this.updateSPS(data)
	case naluType == NALU_PPS:
		this.updatePPS(data)
	case naluType == 24:
		return this.handleSTAPA(data, timestamp)

	case naluType == 28:
		if len(data) < 2 {
			Debug("H264", "Invalid FU-A packet: missing FU header")
			return nil, 0
		}

		fuIndicator := data[0]
		fuHeader := data[1]
		isStart := fuHeader&0x80 != 0
		isEnd := fuHeader&0x40 != 0

		//nri := (data[headerIndexr+1]&0x60)>>5

		//Debug("H264", "FU-A packet - isStart: %t - isEnd: %t", isStart, isEnd)

		if isStart {
			this.fuBuffer.Reset()
			this.fuBuffer.WriteByte(fuIndicator&0xe0 | fuHeader&0x1f)
			this.fuStarted = true
		} else if !this.fuStarted {
			// We got a middle/end fragment without start; likely packet loss.
			return nil, 0
		}

		if this.fuStarted {
			if len(data) < 3 {
				Debug("H264", "Invalid FU-A packet: missing FU payload")
				this.fuStarted = false
				this.fuBuffer.Reset()
				return nil, 0
			}
			this.fuBuffer.Write(data[2:])

			if isEnd {
				this.fuStarted = false
				// packet may contain multiple NAL units
				bufferedBytes := this.fuBuffer.Bytes()
				if len(bufferedBytes) == 0 {
					return nil, 0
				}
				subNaluType := bufferedBytes[0] & 0x1F
				//Debug("H264", "SUB NAL type: %d - timestamp: %d", subNaluType, timestamp)

				if subNaluType == NALU_SPS || subNaluType == NALU_AUD {
					bufered, _ := this.splitNALUs(append(this.startCode, this.fuBuffer.Bytes()...))
					//Debug("H264", "Buffered count: %d", len(bufered))

					for _, v := range bufered {
						subNaluType = v[0] & 0x1F
						Debug("H264", "Fragmented NAL type: %d - timestamp: %d", subNaluType, timestamp)

						switch subNaluType {
						case NALU_SPS:
							this.updateSPS(v)
						case NALU_PPS:
							this.updatePPS(v)
						case 5:
							this.fuBuffer.Reset()
							this.fuBuffer.Write(v)
						}
					}
				}

				return this.handleNALU(subNaluType, this.fuBuffer.Bytes(), timestamp)
			}
		}
	}

	return nil, 0
}

func (this *AnnexBParser) handleNALU(naluType byte, payload []byte, ts uint32) ([]byte, uint32) {
	// Calculate the duration since the last packet was processed
	duration := ts - this.videoTS

	// Reduce logging to decrease CPU usage
	if duration > 5000 {
		Debug("H264", "Long duration between frames: %d ms", duration)
	}

	// Check if the NAL unit type is a keyframe
	if naluType == 5 {
		// Less frequent logging
		if duration > 5000 {
			Info("H264", "Keyframe received")
		}

		// Optimization: preallocate buffer to avoid costly multiple appends
		// If SPS/PPS are missing, don't inject empty parameter sets.
		if len(this.sps) == 0 || len(this.pps) == 0 {
			this.videoTS = ts
			return payload, duration
		}

		totalSize := len(this.startCode)*3 + len(this.pps) + len(this.sps) + len(payload)
		optimizedPayload := make([]byte, 0, totalSize)

		// Build the buffer in a single pass
		optimizedPayload = append(optimizedPayload, this.startCode...)
		optimizedPayload = append(optimizedPayload, this.sps...)
		optimizedPayload = append(optimizedPayload, this.startCode...)
		optimizedPayload = append(optimizedPayload, this.pps...)
		optimizedPayload = append(optimizedPayload, this.startCode...)
		optimizedPayload = append(optimizedPayload, payload...)

		payload = optimizedPayload
	}

	// Update previous timestamp to current timestamp
	this.videoTS = ts
	return payload, duration
}

func (this *AnnexBParser) handleSTAPA(data []byte, timestamp uint32) ([]byte, uint32) {
	// STAP-A: [24][size:2][nal][size:2][nal]...
	if len(data) < 3 {
		return nil, 0
	}

	offset := 1
	emitFrame := false
	keyframe := false
	aggregated := make([]byte, 0, len(data)+len(this.startCode)*2)

	for offset+2 <= len(data) {
		nalSize := int(data[offset])<<8 | int(data[offset+1])
		offset += 2
		if nalSize <= 0 || offset+nalSize > len(data) {
			Debug("H264", "Invalid STAP-A packet size")
			return nil, 0
		}

		nal := data[offset : offset+nalSize]
		offset += nalSize
		if len(nal) == 0 {
			continue
		}

		nalType := nal[0] & 0x1F
		switch nalType {
		case NALU_SPS:
			this.updateSPS(nal)
		case NALU_PPS:
			this.updatePPS(nal)
		case 5:
			keyframe = true
			emitFrame = true
			aggregated = append(aggregated, this.startCode...)
			aggregated = append(aggregated, nal...)
		case 1, 2, 3, 4:
			emitFrame = true
			aggregated = append(aggregated, this.startCode...)
			aggregated = append(aggregated, nal...)
		default:
			// Keep non-frame NALs out of output by default.
		}
	}

	if !emitFrame || len(aggregated) == 0 {
		return nil, 0
	}

	if keyframe && len(this.sps) > 0 && len(this.pps) > 0 {
		withParams := make([]byte, 0, len(this.startCode)*2+len(this.sps)+len(this.pps)+len(aggregated))
		withParams = append(withParams, this.startCode...)
		withParams = append(withParams, this.sps...)
		withParams = append(withParams, this.startCode...)
		withParams = append(withParams, this.pps...)
		withParams = append(withParams, aggregated...)
		aggregated = withParams
	}

	duration := timestamp - this.videoTS
	this.videoTS = timestamp
	return aggregated, duration
}

func (this *AnnexBParser) detectSequenceGap(channel int, seq uint16) bool {
	switch channel {
	case 0:
		if !this.hasVideoSeq {
			this.videoSeq = seq
			this.hasVideoSeq = true
			return false
		}
		expected := this.videoSeq + 1
		gap := seq != expected
		if gap {
			Debug("RTP", "Video RTP sequence gap detected: expected=%d got=%d", expected, seq)
		}
		this.videoSeq = seq
		return gap
	case 2:
		if !this.hasAudioSeq {
			this.audioSeq = seq
			this.hasAudioSeq = true
			return false
		}
		expected := this.audioSeq + 1
		gap := seq != expected
		if gap {
			Debug("RTP", "Audio RTP sequence gap detected: expected=%d got=%d", expected, seq)
		}
		this.audioSeq = seq
		return gap
	default:
		return false
	}
}

func (this *AnnexBParser) splitNALUs(b []byte) (nalus [][]byte, typ int) {
	// Check if there's enough data to do anything meaningful
	if len(b) < 4 {
		return [][]byte{b}, NALU_RAW
	}

	// Check if this is an AVCC stream
	// AVCC (AVC Elementary Stream) is a container format that wraps H.264 NALUs in a
	// simple format that can be easily parsed by most H.264 decoders.
	// Each NALU is prefixed with a 32-bit big-endian integer that indicates the
	// length of the NALU.
	val3 := this.U24BE(b)
	val4 := this.U32BE(b)
	if val4 <= uint32(len(b)) {
		_b := b[4:]
		for len(_b) >= 4 {
			// Read the length of the NALU
			size := this.U32BE(_b)
			// Skip over the length field
			_b = _b[4:]
			// If there's not enough data to read the NALU, break out
			if size > uint32(len(_b)) {
				break
			}
			// Append the NALU to the list of NALUs
			nalus = append(nalus, _b[:size])
			// Skip over the NALU
			_b = _b[size:]
		}
		// If there's no more data left, we're done
		if len(_b) == 0 {
			return nalus, NALU_AVCC
		}
	}

	// Check if this is an Annex B stream
	// Annex B is a format that wraps H.264 NALUs in a slightly more complex
	// format that can be parsed by most H.264 decoders.
	// Each NALU is prefixed with a 3-byte or 4-byte start code, which is either
	// 0x000001 or 0x00000001.
	if val3 == 1 || val4 == 1 {
		start := 0
		for i := 0; i < len(b)-3; {
			// Look for the start code
			if b[i] == 0 && b[i+1] == 0 && ((b[i+2] == 1) || (b[i+2] == 0 && b[i+3] == 1)) {
				// If we found a start code, append the previous NALU (if any)
				// to the list of NALUs
				if start != i {
					nalus = append(nalus, b[start:i])
				}
				// Skip over the start code
				if b[i+2] == 1 {
					i += 3
				} else {
					i += 4
				}
				// Set the start of the next NALU
				start = i
			} else {
				// If we didn't find a start code, just increment the index
				i++
			}
		}
		// If there's any data left over, append it to the list of NALUs
		if start < len(b) {
			nalus = append(nalus, b[start:])
		}
		return nalus, NALU_ANNEXB
	}

	// If we got here, we didn't recognize the format
	return [][]byte{b}, NALU_RAW
}

func (this *AnnexBParser) U24BE(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func (this *AnnexBParser) U32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func (this *AnnexBParser) updatePPS(b []byte) {
	if bytes.Equal(this.pps, b) {
		return
	}
	this.pps = append(this.pps[:0], b...)
	Info("H264", "PPS updated (length: %d bytes)", len(this.pps))
	Debug("H264", "PPS data: %v", this.pps)
}

func (this *AnnexBParser) updateSPS(b []byte) {
	if bytes.Equal(this.sps, b) {
		return
	}
	this.sps = append(this.sps[:0], b...)
	Info("H264", "SPS updated (length: %d bytes)", len(this.sps))
	Debug("H264", "SPS data: %v", this.sps)
}
