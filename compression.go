// Copyright 2017 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"compress/flate"
	"errors"
	"io"
	"strings"
	"sync"
)

const (
	minCompressionLevel     = -2 // flate.HuffmanOnly not defined in Go < 1.6
	maxCompressionLevel     = flate.BestCompression
	defaultCompressionLevel = 1
)

var (
	flateWriterPools     [maxCompressionLevel - minCompressionLevel + 1]sync.Pool
	flateWriterDictPools [maxCompressionLevel - minCompressionLevel + 1]sync.Pool
	flateReaderPool      = sync.Pool{New: func() interface{} {
		return flate.NewReader(nil)
	}}
)

func decompressNoContextTakeover(r io.Reader, dict *[]byte) io.ReadCloser {
	const tail =
	// Add four bytes as specified in RFC
	"\x00\x00\xff\xff" +
		// Add final block to squelch unexpected EOF error from flate reader.
		"\x01\x00\x00\xff\xff"

	fr, _ := flateReaderPool.Get().(io.ReadCloser)
	fr.(flate.Resetter).Reset(io.MultiReader(r, strings.NewReader(tail)), nil)
	return &flateReadWrapper{fr: fr}
}

func decompressContextTakeover(r io.Reader, dict *[]byte) io.ReadCloser {
	const tail =
	// Add four bytes as specified in RFC
	"\x00\x00\xff\xff" +
		// Add final block to squelch unexpected EOF error from flate reader.
		"\x01\x00\x00\xff\xff"

	fr, _ := flateReaderPool.Get().(io.ReadCloser)

	if dict != nil {
		fr.(flate.Resetter).Reset(io.MultiReader(r, strings.NewReader(tail)), *dict)
	} else {
		fr.(flate.Resetter).Reset(io.MultiReader(r, strings.NewReader(tail)), nil)
	}

	return &flateReadWrapper{fr: fr, hasDict: true, dict: dict}
}

func isValidCompressionLevel(level int) bool {
	return minCompressionLevel <= level && level <= maxCompressionLevel
}

func compressNoContextTakeover(w io.WriteCloser, level int, dict *[]byte) io.WriteCloser {
	p := &flateWriterPools[level-minCompressionLevel]
	tw := &truncWriter{w: w}
	fw, _ := p.Get().(*flate.Writer)
	if fw == nil {
		fw, _ = flate.NewWriter(tw, level)
	} else {
		fw.Reset(tw)
	}
	return &flateWriteWrapper{fw: fw, tw: tw, p: p}
}

func compressContextTakeover(w io.WriteCloser, level int, dict *[]byte) io.WriteCloser {
	tw := &truncWriter{w: w}

	var fw *flate.Writer

	if dict != nil {
		fw, _ = flate.NewWriterDict(tw, level, *dict)
	} else {
		fw, _ = flate.NewWriterDict(tw, level, nil)
	}

	return &flateWriteWrapper{fw: fw, tw: tw, hasDict: true, dict: dict}
}

// truncWriter is an io.Writer that writes all but the last four bytes of the
// stream to another io.Writer.
type truncWriter struct {
	w io.WriteCloser
	n int
	p [4]byte
}

func (w *truncWriter) Write(p []byte) (int, error) {
	n := 0

	// fill buffer first for simplicity.
	if w.n < len(w.p) {
		n = copy(w.p[w.n:], p)
		p = p[n:]
		w.n += n
		if len(p) == 0 {
			return n, nil
		}
	}

	m := len(p)
	if m > len(w.p) {
		m = len(w.p)
	}

	if nn, err := w.w.Write(w.p[:m]); err != nil {
		return n + nn, err
	}

	copy(w.p[:], w.p[m:])
	copy(w.p[len(w.p)-m:], p[len(p)-m:])
	nn, err := w.w.Write(p[:len(p)-m])
	return n + nn, err
}

type flateWriteWrapper struct {
	fw *flate.Writer
	tw *truncWriter
	p  *sync.Pool

	hasDict bool
	dict    *[]byte
}

func (w *flateWriteWrapper) Write(p []byte) (int, error) {
	if w.fw == nil {
		return 0, errWriteClosed
	}

	if w.hasDict {
		w.addDict(p)
	}

	return w.fw.Write(p)
}

func (w *flateWriteWrapper) Close() error {
	if w.fw == nil {
		return errWriteClosed
	}
	err1 := w.fw.Flush()

	if !w.hasDict {
		w.p.Put(w.fw)
	}

	w.fw = nil
	if w.tw.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: internal error, unexpected bytes at end of flate stream")
	}
	err2 := w.tw.w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// addDict adds payload to dict.
func (w *flateWriteWrapper) addDict(b []byte) {
	*w.dict = append(*w.dict, b...)

	if len(*w.dict) > maxWindowBits {
		offset := len(*w.dict) - maxWindowBits
		*w.dict = (*w.dict)[offset:]
	}
}

type flateReadWrapper struct {
	fr io.ReadCloser // flate.NewReader

	hasDict bool
	dict    *[]byte
}

func (r *flateReadWrapper) Read(p []byte) (int, error) {
	if r.fr == nil {
		return 0, io.ErrClosedPipe
	}

	n, err := r.fr.Read(p)

	if err == io.EOF {
		// Preemptively place the reader back in the pool. This helps with
		// scenarios where the application does not call NextReader() soon after
		// this final read.
		r.Close()
	}

	if r.hasDict {
		if n > 0 {
			r.addDict(p[:n])
		}
	}

	return n, err
}

func (r *flateReadWrapper) Close() error {
	if r.fr == nil {
		return io.ErrClosedPipe
	}
	err := r.fr.Close()

	if !r.hasDict {
		flateReaderPool.Put(r.fr)
	}

	r.fr = nil
	return err
}

// addDict adds payload to dict.
func (r *flateReadWrapper) addDict(b []byte) {
	*r.dict = append(*r.dict, b...)

	if len(*r.dict) > maxWindowBits {
		offset := len(*r.dict) - maxWindowBits
		*r.dict = (*r.dict)[offset:]
	}
}
