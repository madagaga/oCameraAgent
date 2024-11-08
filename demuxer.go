package main

import (
    "log"
	"bytes"
)

type Demuxer struct {
	startCode []byte
	videoTS uint32
	audioTS uint32
	pps []byte
	sps []byte
	fuBuffer *bytes.Buffer
	fuStarted bool
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

func NewDemuxer() *Demuxer {
	Obj := &Demuxer{
		startCode: []byte{0x00, 0x00, 0x00, 0x01},
		fuBuffer:  bytes.NewBuffer([]byte{}),
	}
	return Obj
}
func (this *Demuxer) reset() {
	this.videoTS = 0
	this.audioTS = 0
	this.fuBuffer.Reset()
	this.fuStarted = false
}

func (this *Demuxer) handlePacket(data *[]byte) ([]byte , uint32){
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

    //sequenceNumber := (uint16(rtpPacket[2]) << 8) + uint16(rtpPacket[3])
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
		// Check if this is the first packet, if so, initialize previous timestamp
		if this.videoTS == 0 {
			// Set a default offset for the previous timestamp to avoid initial zero value
			this.videoTS = timestamp
		}
		return this.parseNalu(rtpPacket[offset:end], timestamp)
	case 2:
		// Check if this is the first packet, if so, initialize previous timestamp
		if this.audioTS == 0 {
			// Set a default offset for the previous timestamp to avoid initial zero value
			this.audioTS = timestamp			
		}
		
		duration := timestamp - this.audioTS		
		// audioPayload := rtpPacket[offset:end]
        // log.Printf("[Audio] - Extracted PCMA payload of size %d bytes", len(audioPayload))

        // // Decode PCMA to PCM
        // pcmData := this.decodePCMA(audioPayload)
        // log.Printf("[Audio] - Decoded PCM data of size %d bytes", len(pcmData))

		this.audioTS = timestamp
		return rtpPacket[offset:end],duration
		
	}

	return nil, 0
	
}

func (this *Demuxer) parseNalu(data []byte, timestamp uint32) ([]byte, uint32) {
	naluType  := data[0] & 0x1F

	log.Printf("[H264] - nal type : %d - ts : %d", naluType, timestamp)
	switch {
	case naluType  >= 1 && naluType  <= 5:
		return this.handleNALU(naluType , data, timestamp)
	case naluType  == NALU_SPS:
		this.updateSPS(data)
	case naluType  == NALU_PPS:
		this.updatePPS(data)
	case naluType  == 24:
		log.Printf("[H264] - 24")

	case naluType  == 28:
		fuIndicator := data[0]		
		fuHeader := data[1]
		isStart := fuHeader&0x80 != 0
		isEnd := fuHeader&0x40 != 0        
		
		 //nri := (data[headerIndexr+1]&0x60)>>5
		 
		 //log.Printf("[H264] - nal type : %d - isStart : %t - isEnd : %t", naluType , isStart, isEnd)

		 if isStart {			 
			 this.fuBuffer.Reset()
			 this.fuBuffer.WriteByte(fuIndicator&0xe0 | fuHeader&0x1f)
			 this.fuStarted = true
		 }
		 if this.fuStarted {
			this.fuBuffer.Write(data[2:])
		 
		 
			if isEnd {
				this.fuStarted = false
				// // packet may contain multiple NAL units
				subNaluType := this.fuBuffer.Bytes()[0] & 0x1F
				//log.Printf("[H264] - SUB nal type : %d - ts : %d", subNaluType, timestamp)
				if subNaluType == NALU_SPS || subNaluType == NALU_AUD {
					bufered, _ := this.splitNALUs(append(this.startCode, this.fuBuffer.Bytes()...))
					//log.Printf("Buffered count : %d", len(bufered))
					for _, v := range bufered {
						subNaluType = v[0] & 0x1F
						log.Printf("[H264] - v nal type : %d - ts : %d", subNaluType, timestamp)
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

func (this *Demuxer) handleNALU(naluType byte, payload []byte, ts uint32) ([]byte, uint32) {
    

    // Calculate the duration since the last packet was processed
    duration := ts - this.videoTS
    log.Printf("[H264] - duration : %d", duration)

    // Check if the NAL unit type is a keyframe
    if naluType == 5 {
        log.Println("[H264] - keyframe")
        // Prepend start code, PPS, and SPS to the payload for keyframe
        payload = append(this.startCode, payload...) // Prepend start code
        payload = append(this.pps, payload...)       // Prepend PPS
        payload = append(this.startCode, payload...) // Prepend another start code
        payload = append(this.sps, payload...)       // Prepend SPS
        payload = append(this.startCode, payload...) // Prepend final start code
    } 

    // Update previous timestamp to current timestamp
    this.videoTS = ts
	return payload, duration
}



func (this *Demuxer) splitNALUs(b []byte) (nalus [][]byte, typ int) {
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

func (this *Demuxer) U24BE(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func (this *Demuxer) U32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func (this *Demuxer) updatePPS(b []byte) {
	if(len(this.pps) == 0) {
	this.pps = b
	log.Printf("[H264] - pps: %v" ,this.pps)
	}
}

func (this *Demuxer) updateSPS(b []byte) {
	if(len(this.sps) == 0) {
	this.sps = b
	log.Printf("[H264] - sps: %v" ,this.sps)
	}
}	