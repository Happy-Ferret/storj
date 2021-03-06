// Copyright (C) 2018 Storj Labs, Inc.
// See LICENSE for copying information.

package eestream

import (
	"context"
	"io"
	"io/ioutil"
	"sync"
	"time"

	"storj.io/storj/pkg/ranger"
)

// ErasureScheme represents the general format of any erasure scheme algorithm.
// If this interface can be implemented, the rest of this library will work
// with it.
type ErasureScheme interface {
	// Encode will take 'in' and call 'out' with erasure coded pieces.
	Encode(in []byte, out func(num int, data []byte)) error

	// Decode will take a mapping of available erasure coded piece num -> data,
	// 'in', and append the combined data to 'out', returning it.
	Decode(out []byte, in map[int][]byte) ([]byte, error)

	// EncodedBlockSize is the size the erasure coded pieces should be that come
	// from Encode and are passed to Decode.
	EncodedBlockSize() int

	// DecodedBlockSize is the size the combined file blocks that should be
	// passed in to Encode and will come from Decode.
	DecodedBlockSize() int

	// Encode will generate this many pieces
	TotalCount() int

	// Decode requires at least this many pieces
	RequiredCount() int
}

type encodedReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	r      io.Reader
	es     ErasureScheme
	min    int // minimum threshold
	opt    int // optimum threshold
	inbuf  []byte
	eps    map[int](*encodedPiece)
	mux    sync.Mutex
	start  time.Time
	done   int // number of readers done
}

// EncodeReader takes a Reader and an ErasureScheme and return a slice of
// Readers.
//
// min is the minimum threshold - the minimum number of erasure pieces that
// are completely stored. If set to 0, it will be reset to the TotalCount
// of the ErasureScheme.
// opt is the optimum threshold - the optimum number of erasure pieces that
// are completely stored. If set to 0, it will be reset to the TotalCount
// of the ErasureScheme.
// mbm is the maximum memory (in bytes) to be allocated for read buffers. If
// set to 0, the minimum possible memory will be used.
//
// When the minimum threshold is reached a timer will be started with another
// 1.5x the amount of time that took so far. The Readers will be aborted as
// soon as the timer expires or the optimum threshold is reached.
func EncodeReader(ctx context.Context, r io.Reader, es ErasureScheme,
	min, opt, mbm int) ([]io.Reader, error) {
	if min == 0 {
		min = es.TotalCount()
	}
	if opt == 0 {
		opt = es.TotalCount()
	}
	if err := checkParams(es, min, opt, mbm); err != nil {
		return nil, err
	}
	er := &encodedReader{
		r:     r,
		es:    es,
		min:   min,
		opt:   opt,
		inbuf: make([]byte, es.DecodedBlockSize()),
		eps:   make(map[int](*encodedPiece), es.TotalCount()),
		start: time.Now(),
	}
	er.ctx, er.cancel = context.WithCancel(ctx)
	readers := make([]io.Reader, 0, es.TotalCount())
	for i := 0; i < es.TotalCount(); i++ {
		er.eps[i] = &encodedPiece{
			er: er,
		}
		er.eps[i].ctx, er.eps[i].cancel = context.WithCancel(er.ctx)
		readers = append(readers, er.eps[i])
	}
	chanSize := mbm / (es.TotalCount() * es.EncodedBlockSize())
	if chanSize < 1 {
		chanSize = 1
	}
	for i := 0; i < es.TotalCount(); i++ {
		er.eps[i].ch = make(chan block, chanSize)
	}
	go er.fillBuffer()
	return readers, nil
}

func checkParams(es ErasureScheme, min, opt, mbm int) error {
	if min < 0 {
		return Error.New("negative minimum threshold")
	}
	if min > 0 && min < es.RequiredCount() {
		return Error.New("minimum threshold less than required count")
	}
	if min > es.TotalCount() {
		return Error.New("minimum threshold greater than total count")
	}
	if opt < 0 {
		return Error.New("negative optimum threshold")
	}
	if opt > 0 && opt < es.RequiredCount() {
		return Error.New("optimum threshold less than required count")
	}
	if opt > es.TotalCount() {
		return Error.New("optimum threshold greater than total count")
	}
	if min > opt {
		return Error.New("minimum threshold greater than optimum threshold")
	}
	if mbm < 0 {
		return Error.New("negative max buffer memory")
	}
	return nil
}

func (er *encodedReader) fillBuffer() {
	// these channels will synchronize the erasure encoder output with the
	// goroutines for adding the output to the reader buffers
	copiers := make(map[int]chan block, er.es.TotalCount())
	for i := 0; i < er.es.TotalCount(); i++ {
		copiers[i] = make(chan block)
		// closing the channel will exit the next goroutine
		defer close(copiers[i])
		// kick off goroutine for parallel copy of encoded data to each
		// reader buffer
		go er.copyData(i, copiers[i])
	}
	// read from the input and encode until EOF or error
	for blockNum := int64(0); ; blockNum++ {
		_, err := io.ReadFull(er.r, er.inbuf)
		if err != nil {
			for i := range copiers {
				copiers[i] <- block{i: i, num: blockNum, err: err}
			}
			return
		}
		err = er.es.Encode(er.inbuf, func(num int, data []byte) {
			b := block{
				i:    num,
				num:  blockNum,
				data: make([]byte, len(data)),
			}
			// data is reused by infecious, so add a copy to the channel
			copy(b.data, data)
			// send the block to the goroutine for adding it to the reader buffer
			copiers[num] <- b
		})
		if err != nil {
			for i := range copiers {
				copiers[i] <- block{i: i, num: blockNum, err: err}
			}
			return
		}
	}
}

// copyData waits for data block from the erasure encoder and copies it to the
// targeted reader buffer
func (er *encodedReader) copyData(num int, copier <-chan block) {
	// close the respective buffer channel when this goroutine exits
	defer func() {
		if er.eps[num].ch != nil {
			close(er.eps[num].ch)
		}
	}()
	// process the channel until closed
	for b := range copier {
		er.addToReader(b)
	}
}

func (er *encodedReader) addToReader(b block) {
	if er.eps[b.i].ch == nil {
		// this channel is already closed for slowliness - skip it
		return
	}
	for {
		// initialize timer
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		// add the encoded data to the respective reader buffer channel
		select {
		case er.eps[b.i].ch <- b:
			return
		// block for no more than 50 ms
		case <-timer.C:
			if er.checkSlowChannel(b.i) {
				return
			}
		}
	}
}

func (er *encodedReader) checkSlowChannel(num int) (closed bool) {
	// use mutex to avoid concurrent map iteration and map write on channels
	er.mux.Lock()
	defer er.mux.Unlock()
	// check how many buffer channels are already empty
	ec := 0
	for i := range er.eps {
		if er.eps[i].ch != nil && len(er.eps[i].ch) == 0 {
			ec++
		}
	}
	// check if more than the required buffer channels are empty, i.e. the
	// current channel is slow and should be closed and its context should be
	// canceled
	closed = ec >= er.min
	if closed {
		close(er.eps[num].ch)
		er.eps[num].ch = nil
		er.eps[num].cancel()
	}
	return closed
}

// Called every time an encoded piece is done reading everything
func (er *encodedReader) readerDone() {
	er.mux.Lock()
	defer er.mux.Unlock()
	er.done++
	if er.done == er.min {
		// minimum threshold reached, wait for 1.5x the duration and cancel
		// the context regardless if optimum threshold is reached
		time.AfterFunc(time.Since(er.start)*3/2, er.cancel)
	}
	if er.done == er.opt {
		// optimum threshold reached - cancel the context
		er.cancel()
	}
}

type encodedPiece struct {
	ctx    context.Context
	cancel context.CancelFunc
	er     *encodedReader
	ch     chan block
	outbuf []byte
	err    error
}

func (ep *encodedPiece) Read(p []byte) (n int, err error) {
	if ep.err != nil {
		return 0, ep.err
	}
	if len(ep.outbuf) <= 0 {
		// take the next block from the channel or block if channel is empty
		select {
		case b, ok := <-ep.ch:
			if !ok {
				// channel was closed due to slowliness
				return 0, io.ErrUnexpectedEOF
			}
			if b.err != nil {
				ep.err = b.err
				if ep.err == io.EOF {
					ep.er.readerDone()
				}
				return 0, ep.err
			}
			ep.outbuf = b.data
		case <-ep.ctx.Done():
			// context was canceled due to:
			//  - slowliness
			//  - optimum threshold reached
			//  - timeout after reaching minimum threshold expired
			return 0, io.ErrUnexpectedEOF
		}
	}

	// we have some buffer remaining for this piece. write it to the output
	n = copy(p, ep.outbuf)
	// slide the unused (if any) bytes to the beginning of the buffer
	copy(ep.outbuf, ep.outbuf[n:])
	// and shrink the buffer
	ep.outbuf = ep.outbuf[:len(ep.outbuf)-n]
	return n, nil
}

// EncodedRanger will take an existing Ranger and provide a means to get
// multiple Ranged sub-Readers. EncodedRanger does not match the normal Ranger
// interface.
type EncodedRanger struct {
	ctx context.Context
	rr  ranger.Ranger
	es  ErasureScheme
	min int // minimum threshold
	opt int // optimum threshold
	mbm int // max buffer memory
}

// NewEncodedRanger creates an EncodedRanger. See the comments for EncodeReader
// about min, opt and mbm.
func NewEncodedRanger(ctx context.Context, rr ranger.Ranger, es ErasureScheme,
	min, opt, mbm int) (*EncodedRanger, error) {
	if rr.Size()%int64(es.DecodedBlockSize()) != 0 {
		return nil, Error.New("invalid erasure encoder and range reader combo. " +
			"range reader size must be a multiple of erasure encoder block size")
	}
	if min == 0 {
		min = es.TotalCount()
	}
	if opt == 0 {
		opt = es.TotalCount()
	}
	err := checkParams(es, min, opt, mbm)
	if err != nil {
		return nil, err
	}
	return &EncodedRanger{
		ctx: ctx,
		es:  es,
		rr:  rr,
		min: min,
		opt: opt,
		mbm: mbm,
	}, nil
}

// OutputSize is like Ranger.Size but returns the Size of the erasure encoded
// pieces that come out.
func (er *EncodedRanger) OutputSize() int64 {
	blocks := er.rr.Size() / int64(er.es.DecodedBlockSize())
	return blocks * int64(er.es.EncodedBlockSize())
}

// Range is like Ranger.Range, but returns a slice of Readers
func (er *EncodedRanger) Range(offset, length int64) ([]io.Reader, error) {
	// the offset and length given may not be block-aligned, so let's figure
	// out which blocks contain the request.
	firstBlock, blockCount := calcEncompassingBlocks(
		offset, length, er.es.EncodedBlockSize())
	// okay, now let's encode the reader for the range containing the blocks
	readers, err := EncodeReader(er.ctx,
		er.rr.Range(
			firstBlock*int64(er.es.DecodedBlockSize()),
			blockCount*int64(er.es.DecodedBlockSize())),
		er.es, er.min, er.opt, er.mbm)
	if err != nil {
		return nil, err
	}
	for i, r := range readers {
		// the offset might start a few bytes in, so we potentially have to
		// discard the beginning bytes
		_, err := io.CopyN(ioutil.Discard, r,
			offset-firstBlock*int64(er.es.EncodedBlockSize()))
		if err != nil {
			return nil, Error.Wrap(err)
		}
		// the length might be shorter than a multiple of the block size, so
		// limit it
		readers[i] = io.LimitReader(r, length)
	}
	return readers, nil
}
