package fsstore

import (
	"encoding/binary"
	"hash/crc32"
	"strconv"
)

// This file defines fsstore's on-disk ledger framing. Each record is written as
// a fixed 16-byte little-endian header followed by its raw payload:
//
//	[len uint32][crc uint32][seq uint64][payload ...]
//
// len is the payload length in bytes; crc is the CRC-32 Castagnoli (crc32c)
// checksum over the payload; seq is the record's 1-based ledger sequence.
// Concatenated frames form a ledger file. A reader walks them with decodeFrame,
// advancing by the returned bytesConsumed and stopping at the first fault — a
// Torn tail (recoverable by truncation) or a Corrupt frame (fail closed).

const (
	// frameHeaderSize is the fixed on-disk header width: len(4)+crc(4)+seq(8).
	frameHeaderSize = 16

	// MaxFramePayload caps a single frame's payload at 16 MiB. storage only
	// requires backends to accept 1 MiB payloads; the 16 MiB ceiling is
	// deliberate headroom. It also bounds the allocation a decoder performs for
	// any declared length: a larger declared length is rejected as corruption
	// before any buffer is sized to it, so a bogus header cannot force a huge
	// allocation.
	MaxFramePayload = 16 << 20
)

// castagnoli is the CRC-32 Castagnoli (crc32c) table used for every frame
// checksum. A *crc32.Table is read-only after construction and safe for
// concurrent use.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// FrameFault identifies the precise reason a frame failed to encode or decode.
// It is the machine-classifiable cause; callers fold faults into the two
// recovery modes with FrameError.IsTorn / FrameError.IsCorrupt.
type FrameFault uint8

const (
	// FaultShortHeader: fewer than frameHeaderSize bytes are present, so the
	// header itself is incomplete. Torn.
	FaultShortHeader FrameFault = iota + 1
	// FaultShortPayload: the header is complete and declares a payload length,
	// but fewer than that many payload bytes are present. Torn.
	FaultShortPayload
	// FaultOversize: a declared or requested payload length exceeds
	// MaxFramePayload. Corrupt — rejected before any buffer is sized to it.
	FaultOversize
	// FaultCRCMismatch: every declared byte is present but the payload CRC does
	// not match the header. Corrupt.
	FaultCRCMismatch
)

// FrameError is the single typed error the frame codec returns. Its Fault names
// the exact cause; IsTorn / IsCorrupt classify it into the two recovery modes
// the ledger acts on:
//
//   - Torn    — a clean truncation at a byte boundary (an interrupted final
//     write). Recoverable: the ledger truncates back to the last good frame.
//   - Corrupt — every declared byte is present but the frame is internally
//     inconsistent (bad CRC, or a length beyond the ceiling). Not recoverable
//     by truncation; the ledger must refuse to open and fail closed.
//
// Callers classify with errors.As(&FrameError) and the predicates, never by
// string. The Error() text carries only integers, never payload bytes.
type FrameError struct {
	Fault FrameFault
	// Have is the number of bytes available (Torn faults).
	Have int
	// Need is the number of bytes the frame required (Torn faults).
	Need int
	// Length is the declared or requested payload length (Oversize fault).
	Length uint64
}

// IsTorn reports a clean truncation: the header or payload is incomplete. The
// ledger recovers by truncating the file to the previous whole frame.
func (e *FrameError) IsTorn() bool {
	return e.Fault == FaultShortHeader || e.Fault == FaultShortPayload
}

// IsCorrupt reports a present-but-invalid frame: a CRC mismatch or a length
// beyond MaxFramePayload. The ledger cannot recover by truncation and fails
// closed.
func (e *FrameError) IsCorrupt() bool {
	return e.Fault == FaultOversize || e.Fault == FaultCRCMismatch
}

func (e *FrameError) Error() string {
	switch e.Fault {
	case FaultShortHeader:
		return "fsstore: frame torn: short header, have " + strconv.Itoa(e.Have) +
			" of " + strconv.Itoa(e.Need) + " bytes"
	case FaultShortPayload:
		return "fsstore: frame torn: short payload, have " + strconv.Itoa(e.Have) +
			" of " + strconv.Itoa(e.Need) + " bytes"
	case FaultOversize:
		return "fsstore: frame corrupt: payload length " + strconv.FormatUint(e.Length, 10) +
			" exceeds max " + strconv.Itoa(MaxFramePayload)
	case FaultCRCMismatch:
		return "fsstore: frame corrupt: payload crc mismatch"
	default:
		return "fsstore: frame error: unknown fault " + strconv.Itoa(int(e.Fault))
	}
}

// encodeFrame renders one ledger record as a self-describing frame: the 16-byte
// header (len, crc32c, seq) followed by payload. It rejects a payload larger
// than MaxFramePayload with a *FrameError (FaultOversize) so an oversized record
// can never reach the disk.
func encodeFrame(seq uint64, payload []byte) ([]byte, error) {
	if len(payload) > MaxFramePayload {
		return nil, &FrameError{Fault: FaultOversize, Length: uint64(len(payload))}
	}
	frame := make([]byte, frameHeaderSize+len(payload))
	// #nosec G115 -- guarded by the MaxFramePayload (16 MiB) check above, so len(payload) always fits uint32
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(frame[4:8], crc32.Checksum(payload, castagnoli))
	binary.LittleEndian.PutUint64(frame[8:16], seq)
	copy(frame[frameHeaderSize:], payload)
	return frame, nil
}

// decodeFrame reads exactly one frame from the front of buf. On success it
// returns the record's seq, a fresh copy of its payload, and bytesConsumed (the
// total frame width, header + payload) so a caller walking concatenated frames
// advances by bytesConsumed and halts at the first fault. The returned payload
// is a copy: buf may be reused or truncated freely.
//
// Every error is a *FrameError. IsTorn reports a clean truncation (recoverable
// by truncating to the previous frame); IsCorrupt reports a present-but-invalid
// frame that must fail closed. An over-ceiling declared length is rejected
// before any payload buffer is sized, so a corrupt header cannot trigger a large
// allocation. On any error the seq, payload, and bytesConsumed returns are zero.
func decodeFrame(buf []byte) (seq uint64, payload []byte, bytesConsumed int, err error) {
	if len(buf) < frameHeaderSize {
		return 0, nil, 0, &FrameError{Fault: FaultShortHeader, Have: len(buf), Need: frameHeaderSize}
	}
	payloadLen := binary.LittleEndian.Uint32(buf[0:4])
	if uint64(payloadLen) > MaxFramePayload {
		return 0, nil, 0, &FrameError{Fault: FaultOversize, Length: uint64(payloadLen)}
	}
	frameLen := frameHeaderSize + int(payloadLen)
	if len(buf) < frameLen {
		return 0, nil, 0, &FrameError{Fault: FaultShortPayload, Have: len(buf), Need: frameLen}
	}
	body := buf[frameHeaderSize:frameLen]
	if crc32.Checksum(body, castagnoli) != binary.LittleEndian.Uint32(buf[4:8]) {
		return 0, nil, 0, &FrameError{Fault: FaultCRCMismatch}
	}
	out := make([]byte, len(body))
	copy(out, body)
	return binary.LittleEndian.Uint64(buf[8:16]), out, frameLen, nil
}
