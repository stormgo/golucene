package classic

import (
	"io"
)

type FastCharStream struct {
	buffer []rune

	bufferLength   int
	bufferPosition int

	tokenStart  int
	bufferStart int

	input io.RuneReader // source of chars
}

func newFastCharStream(r io.RuneReader) *FastCharStream {
	return &FastCharStream{input: r}
}

func (cs *FastCharStream) readChar() (rune, error) {
	if cs.bufferPosition >= cs.bufferLength {
		if err := cs.refill(); err != nil {
			return 0, err
		}
	}
	cs.bufferPosition++
	return cs.buffer[cs.bufferPosition-1], nil
}

func (cs *FastCharStream) refill() (err error) {
	newPosition := cs.bufferLength - cs.tokenStart

	if cs.tokenStart == 0 { // token won't fit in buffer
		if cs.buffer == nil { // first time: alloc buffer
			cs.buffer = make([]rune, 2048)
		} else if cs.bufferLength == len(cs.buffer) { // grow buffer
			panic("not implemented yet")
		}
	} else { // shift token to front
		copy(cs.buffer, cs.buffer[cs.tokenStart:cs.tokenStart+newPosition])
	}

	cs.bufferLength = newPosition // update state
	cs.bufferPosition = newPosition
	cs.bufferStart += cs.tokenStart
	cs.tokenStart = 0

	var charsRead int // fill space in buffer
	limit := len(cs.buffer) - newPosition
	for charsRead < limit && err == nil {
		cs.buffer[newPosition+charsRead], _, err = cs.input.ReadRune()
		charsRead++
	}
	if err != nil && err != io.EOF || charsRead == 0 {
		return err
	}
	cs.bufferLength += charsRead
	return nil
}

func (cs *FastCharStream) beginToken() (rune, error) {
	cs.tokenStart = cs.bufferPosition
	return cs.readChar()
}

func (cs *FastCharStream) backup(amount int) {
	panic("not implemented yet")
}

func (cs *FastCharStream) image() string {
	panic("not implemented yet")
}

func (cs *FastCharStream) endColumn() int {
	panic("not implemented yet")
}

func (cs *FastCharStream) endLine() int {
	panic("not implemented yet")
}