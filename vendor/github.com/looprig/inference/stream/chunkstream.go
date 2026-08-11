package stream

import (
	"errors"
	"io"

	"github.com/looprig/core/content"
)

// FramesToChunks adapts a raw StreamFrame reader plus a per-frame semantic mapper into
// a content.Chunk stream. A StreamDecoder built on a wire framer (sse, ndjson, …) uses
// it to keep the frame-draining, multi-chunk buffering, and body-close plumbing in one
// place while supplying only its codec's per-frame decode logic.
//
// mapFrame maps one raw frame to zero or more chunks:
//   - returning (chunks, nil) yields those chunks (buffered so a frame that decodes to
//     several chunks drops none);
//   - returning (nil, nil) is a tolerant skip (malformed or uninteresting frame);
//   - returning a non-nil error ends the stream with that error. Returning io.EOF is
//     how a codec signals a terminal sentinel such as OpenAI's [DONE].
//
// The returned reader's Close delegates to frames.Close, so the wire framer keeps
// ownership of the underlying body.
func FramesToChunks(frames *StreamReader[StreamFrame], mapFrame func(StreamFrame) ([]content.Chunk, error)) *StreamReader[content.Chunk] {
	return FramesToChunksWithResult(frames, mapFrame, nil)
}

type frameChunkAdapter struct {
	frames         *StreamReader[StreamFrame]
	mapFrame       func(StreamFrame) ([]content.Chunk, error)
	producer       StreamResultProducer
	pending        []content.Chunk
	terminal       bool
	semanticRead   bool
	semanticResult StreamResult
	semanticOK     bool
	semanticErr    error
}

// FramesToChunksWithResult adds a semantic result producer for provider codecs
// that accumulate metadata while mapping frames. A present semantic result is
// authoritative; when it is absent, a result carried by the frame reader is
// propagated instead.
//
// Mapper io.EOF is a clean semantic end. Chunks returned with io.EOF are fully
// drained before the downstream reader observes EOF. Chunks returned with any
// other error are discarded and the error permanently terminates the stream.
func FramesToChunksWithResult(frames *StreamReader[StreamFrame], mapFrame func(StreamFrame) ([]content.Chunk, error), producer StreamResultProducer) *StreamReader[content.Chunk] {
	adapter := &frameChunkAdapter{
		frames:   frames,
		mapFrame: mapFrame,
		producer: producer,
	}
	return NewStreamReaderWithResult(adapter.next, frames.Close, adapter.result)
}

func (a *frameChunkAdapter) next() (content.Chunk, error) {
	if a.mapFrame == nil {
		return nil, &StreamReaderError{Operation: StreamOperationNext, Failure: StreamReaderFailureMissingFrameMapper}
	}
	for {
		if len(a.pending) > 0 {
			chunk := a.pending[0]
			a.pending = a.pending[1:]
			return chunk, nil
		}
		if a.terminal {
			return nil, io.EOF
		}
		frame, err := a.frames.Next()
		if err != nil {
			return nil, a.resolveFrameError(err)
		}
		chunks, err := a.mapChunks(frame)
		if err != nil {
			return nil, err
		}
		if len(chunks) == 0 {
			continue
		}
		a.pending = chunks[1:]
		return chunks[0], nil
	}
}

func (a *frameChunkAdapter) mapChunks(frame StreamFrame) ([]content.Chunk, error) {
	chunks, err := a.mapFrame(frame)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if errors.Is(err, io.EOF) {
		a.terminal = true
		if len(chunks) == 0 {
			return nil, io.EOF
		}
	}
	return chunks, nil
}

func (a *frameChunkAdapter) result() (StreamResult, bool, error) {
	semantic, ok, err := a.readSemanticResult()
	if err != nil || ok {
		return semantic, ok, err
	}
	frameResult, ok := a.frames.Result()
	return frameResult, ok, nil
}

func (a *frameChunkAdapter) resolveFrameError(err error) error {
	var resultErr *StreamResultError
	if a.producer == nil || !errors.As(err, &resultErr) {
		return err
	}
	_, ok, semanticErr := a.readSemanticResult()
	if semanticErr != nil {
		return &StreamResultError{Cause: semanticErr}
	}
	if !ok {
		return err
	}
	a.terminal = true
	return io.EOF
}

func (a *frameChunkAdapter) readSemanticResult() (StreamResult, bool, error) {
	if a.semanticRead {
		return cloneStreamResult(a.semanticResult), a.semanticOK, a.semanticErr
	}
	a.semanticRead = true
	if a.producer == nil {
		return StreamResult{}, false, nil
	}
	result, ok, err := a.producer()
	if err == nil && ok {
		result = cloneStreamResult(result)
		if result.Usage != nil {
			err = result.Usage.Validate()
		}
	}
	a.semanticResult = result
	a.semanticOK = ok && err == nil
	a.semanticErr = err
	return cloneStreamResult(result), a.semanticOK, err
}
